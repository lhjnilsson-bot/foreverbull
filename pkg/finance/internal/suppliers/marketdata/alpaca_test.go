package marketdata

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/alpacahq/alpaca-trade-api-go/v3/marketdata"
	pb "github.com/lhjnilsson/foreverbull/pkg/pb/finance"
	"github.com/stretchr/testify/require"
)

// fakeAlpacaBars answers the bars endpoint like Alpaca: split-adjusted bars
// for adjustment=split, and raw bars, with AAPL's 4:1 split on 2020-08-31
// as a -75% day, for adjustment=raw. The fixture closes are AAPL's daily
// closes around the split; the adjusted file divides pre-split prices by
// 4 and multiplies volume by 4.
func fakeAlpacaBars(t *testing.T) (*AlpacaBars, *[]url.Values) {
	t.Helper()
	raw, err := os.ReadFile("testdata/alpaca_aapl_split_raw.json")
	require.NoError(t, err)
	adjusted, err := os.ReadFile("testdata/alpaca_aapl_split_adjusted.json")
	require.NoError(t, err)

	var mu sync.Mutex
	queries := make([]url.Values, 0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.Query())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("adjustment") {
		case "split":
			_, _ = w.Write(adjusted)
		case "raw":
			_, _ = w.Write(raw)
		default:
			http.Error(w, "unexpected adjustment", http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)

	return &AlpacaBars{opts: marketdata.ClientOpts{
		APIKey: "key", APISecret: "secret", BaseURL: srv.URL,
	}}, &queries
}

// maxDailyCloseMove is the largest one-day close-to-close move, as a fraction.
func maxDailyCloseMove(bars []*pb.OHLC) float64 {
	largest := 0.0
	for index := 1; index < len(bars); index++ {
		move := math.Abs(bars[index].Close/bars[index-1].Close - 1)
		largest = math.Max(largest, move)
	}
	return largest
}

func TestAlpacaGetOHLCRequestsSplitAdjustedDailyBars(t *testing.T) {
	t.Parallel()
	bars, queries := fakeAlpacaBars(t)
	start := time.Date(2020, 8, 27, 0, 0, 0, 0, time.UTC)
	end := time.Date(2020, 9, 1, 0, 0, 0, 0, time.UTC)

	ohlcs, err := bars.GetOHLC(context.Background(), "AAPL", start, &end)
	require.NoError(t, err)

	require.Len(t, *queries, 1)
	require.Equal(t, "split", (*queries)[0].Get("adjustment"))
	require.Equal(t, "1Day", (*queries)[0].Get("timeframe"))

	// Quant's AAPL 2020-08-31 golden: no one-day move beyond ±20%.
	require.Len(t, ohlcs, 4)
	require.Less(t, maxDailyCloseMove(ohlcs), 0.20)
	require.InDelta(t, 124.8075, ohlcs[1].Close, 1e-9)
	require.InDelta(t, 129.04, ohlcs[2].Close, 1e-9)
	require.Equal(t, uint64(187629916), ohlcs[1].Volume)
}

// The golden check above must be able to fail: raw bars show the split as a
// one-day drop of about 75%.
func TestAlpacaRawBarsShowTheSplitAsAFakeDrop(t *testing.T) {
	t.Parallel()
	bars, _ := fakeAlpacaBars(t)
	raw, err := bars.clientWith(context.Background(), nil).GetBars("AAPL", marketdata.GetBarsRequest{
		TimeFrame:  marketdata.OneDay,
		Adjustment: marketdata.AdjustmentRaw,
		Start:      time.Date(2020, 8, 27, 0, 0, 0, 0, time.UTC),
		End:        time.Date(2020, 9, 1, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	ohlcs := make([]*pb.OHLC, 0, len(raw))
	for _, bar := range raw {
		ohlcs = append(ohlcs, &pb.OHLC{Close: bar.Close})
	}
	require.InDelta(t, 0.74, maxDailyCloseMove(ohlcs), 0.01)
}

func TestAlpacaDailyBarsNamesTheAdjustment(t *testing.T) {
	t.Parallel()
	start := time.Date(2020, 8, 27, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 5)
	req := alpacaDailyBars(start, end)
	require.Equal(t, marketdata.AdjustmentSplit, req.Adjustment)
	require.Equal(t, marketdata.OneDay, req.TimeFrame)
	require.Equal(t, start, req.Start)
	require.Equal(t, end, req.End)
}
