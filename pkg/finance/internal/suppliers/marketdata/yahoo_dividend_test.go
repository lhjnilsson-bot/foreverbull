package marketdata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lhjnilsson/foreverbull/pkg/finance/supplier"
	"github.com/stretchr/testify/require"
)

// yahooKODividendFixture is Yahoo's chart for KO from 2024-02-29 to
// 2024-07-31 with events=div,splits, recorded 2026-11-04 with the volatile
// meta trimmed. KO went ex twice in it, $0.485 each (KO IR, 2024-02-15 and
// 2024-05-02).
const yahooKODividendFixture = "testdata/yahoo_chart_ko_dividends.json"

// yahooAAPLDividendFixture is Yahoo's AAPL chart around the 2020-08-07
// dividend, before the 2020-08-31 4:1 split. Apple declared $0.82; Yahoo
// sends it on the split-adjusted basis of its close.
const yahooAAPLDividendFixture = "testdata/yahoo_chart_aapl_dividend_split.json"

func TestYahooGetDividendsParsesKOIntoTwoEvents(t *testing.T) {
	t.Parallel()
	client := yahooFixtureServer(t, yahooKODividendFixture)
	start := time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC)
	end := time.Date(2024, 7, 31, 0, 0, 0, 0, time.UTC)

	dividends, err := client.GetDividends(context.Background(), "ko", start, &end)
	require.NoError(t, err)
	require.Equal(t, []supplier.Dividend{
		{
			ExDate: time.Date(2024, 3, 14, 0, 0, 0, 0, time.UTC), Amount: 0.485, RawAmount: 0.485, SplitFactor: 1,
			Currency: "USD", SourceRef: "yahoo:chart:1710423000",
		},
		{
			ExDate: time.Date(2024, 6, 14, 0, 0, 0, 0, time.UTC), Amount: 0.485, RawAmount: 0.485, SplitFactor: 1,
			Currency: "USD", SourceRef: "yahoo:chart:1718371800",
		},
	}, dividends)
}

func TestYahooGetDividendsKeepsOnlyTheRequestedExDates(t *testing.T) {
	t.Parallel()
	client := yahooFixtureServer(t, yahooKODividendFixture)
	start := time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC)
	end := time.Date(2024, 6, 14, 0, 0, 0, 0, time.UTC)

	dividends, err := client.GetDividends(context.Background(), "KO", start, &end)
	require.NoError(t, err)
	require.Len(t, dividends, 1)
	require.Equal(t, "2024-06-14", dividends[0].ExDate.Format(time.DateOnly))
}

// Yahoo's dividend is on the split-adjusted basis: 0.205, not the declared 0.82.
func TestYahooGetDividendsIsSplitAdjusted(t *testing.T) {
	t.Parallel()
	client := yahooFixtureServer(t, yahooAAPLDividendFixture)
	start := time.Date(2020, 7, 31, 0, 0, 0, 0, time.UTC)
	end := time.Date(2020, 8, 13, 0, 0, 0, 0, time.UTC)

	dividends, err := client.GetDividends(context.Background(), "AAPL", start, &end)
	require.NoError(t, err)
	require.Len(t, dividends, 1)
	require.Equal(t, "2020-08-07", dividends[0].ExDate.Format(time.DateOnly))
	require.InDelta(t, 0.205, dividends[0].Amount, 1e-12)
	require.InDelta(t, 1.0, dividends[0].SplitFactor, 1e-12)
	require.Nil(t, dividends[0].PayDate, "yahoo sends no pay date and none is estimated")
}

// A chart with no dividend events is an answer with no dividends.
func TestYahooGetDividendsWithNoEventsIsEmpty(t *testing.T) {
	t.Parallel()
	client := yahooBodyServer(t, http.StatusOK, `{"chart":{"result":[{"meta":{"currency":"USD"},"timestamp":[]}],"error":null}}`)
	dividends, err := client.GetDividends(context.Background(), "XYZ", time.Now().AddDate(0, -1, 0), nil)
	require.NoError(t, err)
	require.Empty(t, dividends)
}

// A failed request, a chart error, or a broken event is an error, never "no dividends".
func TestYahooGetDividendsFailures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{
			name: "http error", status: http.StatusInternalServerError, body: `{}`,
			want: "yahoo dividends for XYZ failed: HTTP 500 Internal Server Error",
		},
		{
			name: "not found", status: http.StatusNotFound,
			body: `{"chart":{"result":null,"error":{"code":"Not Found","description":"No data found"}}}`,
			want: "yahoo dividends for XYZ failed: HTTP 404 Not Found",
		},
		{
			name: "chart error", status: http.StatusOK,
			body: `{"chart":{"result":null,"error":{"code":"Bad Request","description":"Invalid range"}}}`,
			want: "yahoo dividends for XYZ failed: Invalid range",
		},
		{
			name: "no result", status: http.StatusOK, body: `{"chart":{"result":[],"error":null}}`,
			want: "yahoo dividends for XYZ failed: no result",
		},
		{
			name: "event without amount", status: http.StatusOK,
			body: `{"chart":{"result":[{"meta":{"currency":"USD"},"events":{"dividends":{"1":{"date":1}}}}],"error":null}}`,
			want: "yahoo dividends for XYZ: event 1 has no amount or date",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := yahooBodyServer(t, tc.status, tc.body)
			dividends, err := client.GetDividends(context.Background(), "xyz", time.Unix(0, 0), nil)
			require.Nil(t, dividends)
			require.EqualError(t, err, tc.want)
			var status *DividendStatusError
			if tc.status != http.StatusOK {
				require.ErrorAs(t, err, &status)
				require.Equal(t, "yahoo", status.Provider)
				require.Equal(t, tc.status, status.HTTPCode)
			} else {
				require.NotErrorAs(t, err, &status)
			}
		})
	}
}

func yahooBodyServer(t *testing.T, status int, body string) *YahooClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("events") != "div" {
			http.Error(w, "events=div missing", http.StatusTeapot)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &YahooClient{chartURL: srv.URL + "/"}
}
