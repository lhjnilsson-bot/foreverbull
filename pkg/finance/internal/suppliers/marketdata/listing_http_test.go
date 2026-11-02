package marketdata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	alpacamd "github.com/alpacahq/alpaca-trade-api-go/v3/marketdata"
	"github.com/stretchr/testify/require"
)

func TestYahooSearchLookupStatuses(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		status int
		body   string
		ctype  string
		cancel bool
		want   string
	}{
		{name: "not found", status: http.StatusNotFound, body: `{"quotes":[]}`, want: "yahoo search for XYZ failed: HTTP 404 Not Found"},
		{name: "forbidden", status: http.StatusForbidden, body: `{}`, want: "yahoo search for XYZ failed: HTTP 403 Forbidden"},
		{name: "unprocessable", status: http.StatusUnprocessableEntity, body: `{}`, want: "yahoo search for XYZ failed: HTTP 422 Unprocessable Entity"},
		{name: "rate limit", status: http.StatusTooManyRequests, body: `{}`, want: "yahoo search for XYZ failed: HTTP 429 Too Many Requests"},
		{name: "unknown", status: http.StatusTeapot, body: `{}`, want: "yahoo search for XYZ failed: HTTP 418 I'm a teapot"},
		{name: "cancel", cancel: true, want: "yahoo search for XYZ: context canceled"},
		{
			name: "bot check", status: http.StatusOK, ctype: "text/html", body: "<html>bot</html>",
			want: "Yahoo blocked the download for XYZ (it returned a bot-check page instead of data). Retry with --provider stooq.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.ctype != "" {
					w.Header().Set("Content-Type", tc.ctype)
				}
				if tc.status == http.StatusTooManyRequests {
					w.Header().Set("Retry-After", "1")
				}
				if tc.status != 0 {
					w.WriteHeader(tc.status)
				}
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(server.Close)
			client := &YahooClient{
				searchURL:  server.URL,
				crumb:      "test",
				httpClient: server.Client(),
				limiter:    newRequestLimiterEvery(time.Millisecond),
			}
			ctx := context.Background()
			if tc.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			_, err := client.SearchSymbols(ctx, "XYZ")
			require.EqualError(t, err, tc.want)
		})
	}
}

func TestYahooListingDateLookupStatuses(t *testing.T) {
	t.Parallel()

	chart := `{"chart":{"result":[{"meta":{"firstTradeDate":1710509400}}],"error":{}}}`
	millis := `{"chart":{"result":[{"meta":{"firstTradeDate":1710509400000}}],"error":{}}}`
	missing := `{"chart":{"result":[{"meta":{}}],"error":{}}}`
	chartErr := `{"chart":{"result":[],"error":{"code":"Not Found","description":"No data found"}}}`
	cases := []struct {
		name   string
		status int
		body   string
		ctype  string
		cancel bool
		want   string
		day    string
	}{
		{name: "not found", status: http.StatusNotFound, body: chart, want: "yahoo listing date for XYZ failed: HTTP 404 Not Found"},
		{name: "forbidden", status: http.StatusForbidden, body: `{}`, want: "yahoo listing date for XYZ failed: HTTP 403 Forbidden"},
		{name: "unprocessable", status: http.StatusUnprocessableEntity, body: `{}`, want: "yahoo listing date for XYZ failed: HTTP 422 Unprocessable Entity"},
		{name: "rate limit", status: http.StatusTooManyRequests, body: `{}`, want: "yahoo listing date for XYZ failed: HTTP 429 Too Many Requests"},
		{name: "unknown", status: http.StatusTeapot, body: `{}`, want: "yahoo listing date for XYZ failed: HTTP 418 I'm a teapot"},
		{name: "cancel", cancel: true, want: "yahoo listing date for XYZ: context canceled"},
		{
			name: "bot check", status: http.StatusOK, ctype: "text/html", body: "<html>bot</html>",
			want: "Yahoo blocked the download for XYZ (it returned a bot-check page instead of data). Retry with --provider stooq.",
		},
		{name: "missing first trade", status: http.StatusOK, body: missing, want: "yahoo did not say when XYZ was listed"},
		{name: "chart error", status: http.StatusOK, body: chartErr, want: "yahoo listing date for XYZ couldn't be read"},
		{name: "milliseconds", status: http.StatusOK, body: millis, day: "2024-03-15"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.ctype != "" {
					w.Header().Set("Content-Type", tc.ctype)
				}
				if tc.status == http.StatusTooManyRequests {
					w.Header().Set("Retry-After", "1")
				}
				if tc.status != 0 {
					w.WriteHeader(tc.status)
				}
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(server.Close)
			client := &YahooClient{
				chartURL:   server.URL + "/",
				crumb:      "test",
				httpClient: server.Client(),
				limiter:    newRequestLimiterEvery(time.Millisecond),
			}
			ctx := context.Background()
			if tc.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			got, err := client.FirstTradeDate(ctx, "XYZ")
			if tc.day != "" {
				require.NoError(t, err)
				require.Equal(t, tc.day, got.Format(time.DateOnly))
				return
			}
			require.EqualError(t, err, tc.want)
		})
	}
}

func TestAlpacaAssetStatusLookupStatuses(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		status int
		body   string
		cancel bool
		want   string
		ok     string
	}{
		{name: "inactive", status: http.StatusOK, body: `{"status":"inactive"}`, ok: "inactive"},
		{name: "not found", status: http.StatusNotFound, body: `{"status":"inactive"}`, want: "alpaca asset status for XYZ failed: HTTP 404 Not Found"},
		{name: "forbidden", status: http.StatusForbidden, body: `{}`, want: "alpaca asset status for XYZ failed: HTTP 403 Forbidden"},
		{name: "unprocessable", status: http.StatusUnprocessableEntity, body: `{}`, want: "alpaca asset status for XYZ failed: HTTP 422 Unprocessable Entity"},
		{name: "server", status: http.StatusServiceUnavailable, body: `{"status":"inactive"}`, want: "alpaca asset status for XYZ failed: HTTP 503 Service Unavailable"},
		{name: "rate limit", status: http.StatusTooManyRequests, body: `{}`, want: "alpaca asset status for XYZ failed: HTTP 429 Too Many Requests"},
		{name: "unknown", status: http.StatusTeapot, body: `{}`, want: "alpaca asset status for XYZ failed: HTTP 418 I'm a teapot"},
		{name: "cancel", cancel: true, want: "alpaca asset status for XYZ: context canceled"},
		{name: "unexpected status", status: http.StatusOK, body: `{"status":"pending"}`, want: `alpaca asset status for XYZ is "pending"`},
		{name: "unreadable", status: http.StatusOK, body: `not-json`, want: "alpaca asset status for XYZ couldn't be read"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.status == http.StatusTooManyRequests {
					w.Header().Set("Retry-After", "1")
				}
				if tc.status != 0 {
					w.WriteHeader(tc.status)
				}
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(server.Close)
			client := &AlpacaBars{
				opts:       alpacamd.ClientOpts{APIKey: "key", APISecret: "secret", HTTPClient: server.Client()},
				tradingURL: server.URL,
				limiter:    newRequestLimiterEvery(time.Millisecond),
			}
			ctx := context.Background()
			if tc.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			got, err := client.AssetStatus(ctx, "XYZ")
			if tc.ok != "" {
				require.NoError(t, err)
				require.Equal(t, tc.ok, got)
				return
			}
			require.EqualError(t, err, tc.want)
		})
	}
}
