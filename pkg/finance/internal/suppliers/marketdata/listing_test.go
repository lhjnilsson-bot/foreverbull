package marketdata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	alpacamd "github.com/alpacahq/alpaca-trade-api-go/v3/marketdata"
	"github.com/stretchr/testify/require"
)

func TestParseSearchSymbolsSeparatesNotFoundFromAFailedBody(t *testing.T) {
	t.Parallel()

	symbols, err := parseSearchSymbols([]byte(`{"quotes":[]}`))
	require.NoError(t, err)
	require.Empty(t, symbols)

	_, err = parseSearchSymbols([]byte(`{"quotes":null}`))
	require.EqualError(t, err, "yahoo search did not say whether any symbol is listed")

	_, err = parseSearchSymbols([]byte(`{}`))
	require.EqualError(t, err, "yahoo search did not say whether any symbol is listed")

	symbols, err = parseSearchSymbols([]byte(`{"quotes":[{"symbol":"XYZ"}]}`))
	require.NoError(t, err)
	require.Equal(t, []string{"XYZ"}, symbols)
}

func TestRecordedYahooIPOFixtureNamesTheFirstTradeDate(t *testing.T) {
	t.Parallel()

	body, err := os.ReadFile(filepath.Join("testdata", "yahoo_ipo_chart.json"))
	require.NoError(t, err)
	day, err := firstTradeDate("XYZ", body)
	require.NoError(t, err)
	require.Equal(t, "2024-03-15", day.Format(time.DateOnly))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	client := &YahooClient{chartURL: server.URL + "/", crumb: "test", httpClient: server.Client()}
	got, err := client.FirstTradeDate(context.Background(), "XYZ")
	require.NoError(t, err)
	require.True(t, got.Equal(day))
}

func TestAlpacaAssetStatusReadsInactiveAndRefusesAFailedLookup(t *testing.T) {
	t.Parallel()

	var status int
	var body string
	var gotPath, gotKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("APCA-API-KEY-ID")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	client := &AlpacaBars{
		opts:       alpacamd.ClientOpts{APIKey: "key", APISecret: "secret", HTTPClient: server.Client()},
		tradingURL: server.URL,
		limiter:    newRequestLimiterEvery(time.Millisecond),
	}

	status, body = http.StatusOK, `{"status":"inactive"}`
	got, err := client.AssetStatus(context.Background(), "XYZ")
	require.NoError(t, err)
	require.Equal(t, "inactive", got)
	require.Equal(t, "/v2/assets/XYZ", gotPath)
	require.Equal(t, "key", gotKey)

	status, body = http.StatusOK, `{"status":"active"}`
	got, err = client.AssetStatus(context.Background(), "XYZ")
	require.NoError(t, err)
	require.Equal(t, "active", got)

	status, body = http.StatusInternalServerError, `{"status":"inactive"}`
	_, err = client.AssetStatus(context.Background(), "XYZ")
	require.EqualError(t, err, "alpaca asset status for XYZ failed: HTTP 500 Internal Server Error")

	status, body = http.StatusOK, `{}`
	_, err = client.AssetStatus(context.Background(), "XYZ")
	require.EqualError(t, err, `alpaca asset status for XYZ is ""`)
}
