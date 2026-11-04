package marketdata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/alpacahq/alpaca-trade-api-go/v3/marketdata"
	"github.com/lhjnilsson/foreverbull/pkg/finance/supplier"
	"github.com/stretchr/testify/require"
)

// The two AAPL pages follow Alpaca's documented corporate-actions schema
// (https://docs.alpaca.markets/us/reference/corporateactions-1): Apple's
// $0.82 dividend going ex on 2020-08-07, paid 2020-08-13, before its 4:1
// split on 2020-08-31, then a $0.205 dividend after it. They are written by
// hand, not recorded.
const (
	alpacaAAPLActionsPage1 = "testdata/alpaca_corporate_actions_aapl_page1.json"
	alpacaAAPLActionsPage2 = "testdata/alpaca_corporate_actions_aapl_page2.json"
)

// fakeAlpacaActions serves pages by page_token ("" is the first) and records
// every query.
func fakeAlpacaActions(t *testing.T, now time.Time, pages map[string]string) (*AlpacaBars, *[]url.Values) {
	t.Helper()
	var mu sync.Mutex
	queries := make([]url.Values, 0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.Query())
		mu.Unlock()
		if r.URL.Path != "/v1/corporate-actions" || r.Header.Get("APCA-API-KEY-ID") != "key" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		body, ok := pages[r.URL.Query().Get("page_token")]
		if !ok {
			http.Error(w, "unknown page", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &AlpacaBars{
		opts: marketdata.ClientOpts{APIKey: "key", APISecret: "secret", BaseURL: srv.URL},
		now:  func() time.Time { return now },
	}, &queries
}

func readFixture(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(body)
}

func date(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.DateOnly, value)
	require.NoError(t, err)
	return parsed
}

func datePtr(t *testing.T, value string) *time.Time {
	t.Helper()
	parsed := date(t, value)
	return &parsed
}

// A dividend before a 4:1 split is stored at a quarter of its declared rate,
// on the split-adjusted basis of the bars; one after the split is not
// re-scaled. Both pages are read, and the request asks by process date from
// start through today.
func TestAlpacaGetDividendsPutsTheRateOnTheSplitBasis(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	client, queries := fakeAlpacaActions(t, now, map[string]string{
		"":       readFixture(t, alpacaAAPLActionsPage1),
		"page-2": readFixture(t, alpacaAAPLActionsPage2),
	})

	dividends, err := client.GetDividends(context.Background(), "aapl", date(t, "2020-08-01"), datePtr(t, "2020-12-31"))
	require.NoError(t, err)
	require.Equal(t, []supplier.Dividend{
		{
			ExDate: date(t, "2020-08-07"), PayDate: datePtr(t, "2020-08-13"), RecordDate: datePtr(t, "2020-08-10"),
			Amount: 0.205, RawAmount: 0.82, SplitFactor: 4, SourceRef: "alpaca:a1",
		},
		{
			ExDate: date(t, "2020-11-06"), PayDate: datePtr(t, "2020-11-12"), RecordDate: datePtr(t, "2020-11-09"),
			Amount: 0.205, RawAmount: 0.205, SplitFactor: 1, SourceRef: "alpaca:a2",
		},
	}, dividends)

	require.Len(t, *queries, 2)
	first := (*queries)[0]
	require.Equal(t, "AAPL", first.Get("symbols"))
	require.Equal(t, "2020-08-01", first.Get("start"))
	require.Equal(t, "2026-11-04", first.Get("end"), "process date runs through today, past the backtest end")
	require.Equal(t, "cash_dividend,forward_split,reverse_split,stock_dividend,spin_off,cash_merger,stock_merger,stock_and_cash_merger",
		first.Get("types"))
	require.Empty(t, first.Get("page_token"))
	require.Equal(t, "page-2", (*queries)[1].Get("page_token"))
}

// The process-date window reaches past the end, so ex-dates are filtered
// here: a dividend processed in the window but ex after the end is left out.
func TestAlpacaGetDividendsFiltersOnTheExDate(t *testing.T) {
	t.Parallel()
	client, _ := fakeAlpacaActions(t, time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC), map[string]string{
		"":       readFixture(t, alpacaAAPLActionsPage1),
		"page-2": readFixture(t, alpacaAAPLActionsPage2),
	})
	dividends, err := client.GetDividends(context.Background(), "AAPL", date(t, "2020-08-01"), datePtr(t, "2020-08-31"))
	require.NoError(t, err)
	require.Len(t, dividends, 1)
	require.InDelta(t, 0.205, dividends[0].Amount, 1e-12)
}

// A foreign issuer's dividend is kept at its full rate: booked gross.
func TestAlpacaGetDividendsKeepsAForeignDividendGross(t *testing.T) {
	t.Parallel()
	page := `{"corporate_actions":{"cash_dividends":[{"id":"f1","symbol":"TSM","rate":0.5,"foreign":true,` +
		`"process_date":"2024-06-20","ex_date":"2024-06-13","payable_date":"2024-07-11"}]},"next_page_token":null}`
	client, _ := fakeAlpacaActions(t, time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC), map[string]string{"": page})
	dividends, err := client.GetDividends(context.Background(), "TSM", date(t, "2024-06-01"), datePtr(t, "2024-06-30"))
	require.NoError(t, err)
	require.Len(t, dividends, 1)
	require.True(t, dividends[0].Foreign)
	require.InDelta(t, 0.5, dividends[0].Amount, 1e-12)
	require.InDelta(t, 0.5, dividends[0].RawAmount, 1e-12)
}

// A missing payable date stays missing; nothing estimates it.
func TestAlpacaGetDividendsKeepsAMissingPayDate(t *testing.T) {
	t.Parallel()
	page := `{"corporate_actions":{"cash_dividends":[{"id":"n1","symbol":"KO","rate":0.485,` +
		`"process_date":"2024-03-20","ex_date":"2024-03-14"}]},"next_page_token":null}`
	client, _ := fakeAlpacaActions(t, time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC), map[string]string{"": page})
	dividends, err := client.GetDividends(context.Background(), "KO", date(t, "2024-03-01"), datePtr(t, "2024-03-31"))
	require.NoError(t, err)
	require.Len(t, dividends, 1)
	require.Nil(t, dividends[0].PayDate)
}

// A stock dividend, spin-off or merger dated inside the span comes back
// marked Unsupported with its ex-date, to be stored; one outside it does not.
// The fetch itself doesn't fail: whether one fails a backtest depends on the
// backtest's window.
func TestAlpacaGetDividendsReturnsUnsupportedActions(t *testing.T) {
	t.Parallel()
	page := `{"corporate_actions":{` +
		`"cash_dividends":[{"id":"c1","symbol":"XYZ","rate":0.5,"process_date":"2024-03-20","ex_date":"2024-03-07"}],` +
		`"spin_offs":[{"id":"s1","ex_date":"2024-03-05"}],` +
		`"stock_dividends":[{"id":"d1","ex_date":"2024-03-07"},{"id":"d0","ex_date":"2023-12-01"}],` +
		`"cash_mergers":[{"id":"m1","effective_date":"2024-03-20"}]},"next_page_token":null}`
	client, _ := fakeAlpacaActions(t, time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC), map[string]string{"": page})
	dividends, err := client.GetDividends(context.Background(), "XYZ", date(t, "2024-03-01"), datePtr(t, "2024-03-31"))
	require.NoError(t, err)
	type row struct{ exDate, unsupported, ref string }
	var got []row
	for _, dividend := range dividends {
		got = append(got, row{dividend.ExDate.Format(time.DateOnly), dividend.Unsupported, dividend.SourceRef})
	}
	require.Equal(t, []row{
		{"2024-03-05", "spin_off", "alpaca:s1"},
		{"2024-03-07", "", "alpaca:c1"},
		{"2024-03-07", "stock_dividend", "alpaca:d1"},
		{"2024-03-20", "cash_merger", "alpaca:m1"},
	}, got)
}

// A payable or record date that can't be read is stored as unknown, not a
// failure: the cash is credited on the ex-date and the dates are for display.
func TestAlpacaGetDividendsKeepsAnUnreadablePayDateAsUnknown(t *testing.T) {
	t.Parallel()
	page := `{"corporate_actions":{"cash_dividends":[{"id":"n1","symbol":"KO","rate":0.485,` +
		`"process_date":"2024-03-20","ex_date":"2024-03-14","payable_date":"2024-04-31","record_date":"soon"}]},` +
		`"next_page_token":null}`
	client, _ := fakeAlpacaActions(t, time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC), map[string]string{"": page})
	dividends, err := client.GetDividends(context.Background(), "KO", date(t, "2024-03-01"), datePtr(t, "2024-03-31"))
	require.NoError(t, err)
	require.Len(t, dividends, 1)
	require.Nil(t, dividends[0].PayDate)
	require.Nil(t, dividends[0].RecordDate)
	require.InDelta(t, 0.485, dividends[0].Amount, 1e-12)
}

func TestAlpacaGetDividendsFailures(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	t.Run("http error", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"message":"forbidden"}`, http.StatusForbidden)
		}))
		t.Cleanup(srv.Close)
		client := &AlpacaBars{
			opts: marketdata.ClientOpts{APIKey: "key", APISecret: "secret", BaseURL: srv.URL},
			now:  func() time.Time { return now },
		}
		_, err := client.GetDividends(context.Background(), "KO", date(t, "2024-03-01"), nil)
		require.EqualError(t, err, "alpaca corporate actions for KO failed: HTTP 403 Forbidden")
		var status *DividendStatusError
		require.ErrorAs(t, err, &status)
		require.Equal(t, "alpaca", status.Provider)
		require.Equal(t, http.StatusForbidden, status.HTTPCode)
	})
	t.Run("unreadable page", func(t *testing.T) {
		t.Parallel()
		client, _ := fakeAlpacaActions(t, now, map[string]string{"": `{"corporate_actions":`})
		_, err := client.GetDividends(context.Background(), "KO", date(t, "2024-03-01"), nil)
		require.EqualError(t, err, "alpaca corporate actions for KO couldn't be read")
	})
	t.Run("no rate", func(t *testing.T) {
		t.Parallel()
		client, _ := fakeAlpacaActions(t, now, map[string]string{
			"": `{"corporate_actions":{"cash_dividends":[{"id":"x","ex_date":"2024-03-14"}]},"next_page_token":null}`,
		})
		_, err := client.GetDividends(context.Background(), "KO", date(t, "2024-03-01"), nil)
		require.EqualError(t, err, "alpaca dividend for KO on 2024-03-14 has no rate")
	})
	t.Run("too many pages", func(t *testing.T) {
		t.Parallel()
		pages := map[string]string{}
		for page := 0; page <= alpacaCorporateActionsPages; page++ {
			token := ""
			if page > 0 {
				token = "p" + time.Duration(page).String()
			}
			pages[token] = `{"corporate_actions":{},"next_page_token":"p` + time.Duration(page+1).String() + `"}`
		}
		client, queries := fakeAlpacaActions(t, now, pages)
		_, err := client.GetDividends(context.Background(), "KO", date(t, "2024-03-01"), nil)
		require.EqualError(t, err, "alpaca corporate actions for KO: more than 50 pages, so the list would be truncated")
		require.Len(t, *queries, alpacaCorporateActionsPages)
	})
}
