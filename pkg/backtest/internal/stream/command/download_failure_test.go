package command

import (
	"net/http"
	"testing"
	"time"

	"github.com/lhjnilsson/foreverbull/pkg/finance"
	financeapi "github.com/lhjnilsson/foreverbull/pkg/finance/api"
	"github.com/stretchr/testify/require"
)

func barsFailed(symbol string, status int) downloadMiss {
	return barsFailedFrom("alpaca", symbol, status)
}

func barsFailedFrom(provider, symbol string, status int) downloadMiss {
	return downloadMiss{symbol: symbol, cause: &financeapi.BarsStatusError{Provider: provider, Symbol: symbol, Status: status}}
}

const issuesStep = " If it keeps happening, please report it at https://github.com/lhjnilsson/foreverbull/issues with this message."

// Each provider status names its own cause and step, word for word. The
// symbol is in the lead only, and the number of tries is never shown.
func TestDownloadFailureMessageNamesEachCause(t *testing.T) {
	t.Parallel()
	const lead = "Couldn't load data for backtest 'oef-2024q1': foreverbull couldn't download prices for XYZ from alpaca: "
	cases := map[int]string{
		http.StatusUnprocessableEntity: "alpaca rejected the request as invalid (HTTP 422). The backtest wasn't saved. " +
			"Check the symbol and the backtest dates, or try another provider with --provider, then run " +
			"`fbull backtest create` again." + issuesStep,
		http.StatusUnauthorized: "alpaca didn't accept foreverbull's API key (HTTP 401). The backtest wasn't saved. " +
			"Check your alpaca API key and secret, or try another provider with --provider, then run `fbull backtest create` again.",
		http.StatusForbidden: "alpaca didn't accept foreverbull's API key (HTTP 403). The backtest wasn't saved. " +
			"Check your alpaca API key and secret, or try another provider with --provider, then run `fbull backtest create` again.",
		http.StatusTooManyRequests: "alpaca limited how many requests foreverbull can make. The backtest wasn't saved. " +
			"Wait a few minutes, then run `fbull backtest create` again, or try another provider with --provider.",
		http.StatusInternalServerError: "alpaca had a server error (HTTP 500). The backtest wasn't saved. " +
			"Run `fbull backtest create` again in a few minutes, or try another provider with --provider.",
		http.StatusBadGateway: "alpaca had a server error (HTTP 502). The backtest wasn't saved. " +
			"Run `fbull backtest create` again in a few minutes, or try another provider with --provider.",
		http.StatusTeapot: "alpaca refused the request (HTTP 418). The backtest wasn't saved. " +
			"Try another provider with --provider, then run `fbull backtest create` again.",
	}
	for status, want := range cases {
		err := plainSentence(downloadFailureMessage("oef-2024q1", "alpaca", []downloadMiss{barsFailed("XYZ", status)}, true))
		require.EqualError(t, err, lead+want, status)
	}
}

// Two or more symbols are one block. Each step is listed once, and
// --provider is offered once.
func TestDownloadFailureMessageBlock(t *testing.T) {
	t.Parallel()
	err := plainSentence(downloadFailureMessage("oef-2024q1", "alpaca",
		[]downloadMiss{barsFailed("XYZ", http.StatusServiceUnavailable), barsFailed("ABC", http.StatusTooManyRequests)}, true))
	require.EqualError(t, err, "Couldn't load data for backtest 'oef-2024q1': foreverbull couldn't download prices for "+
		"2 symbols from alpaca. The backtest wasn't saved.\n"+
		"  XYZ: alpaca had a server error (HTTP 503).\n"+
		"  ABC: alpaca limited how many requests foreverbull can make.\n"+
		"Run `fbull backtest create` again in a few minutes, or try another provider with --provider. "+
		"Wait a few minutes, then run `fbull backtest create` again.")
}

// A backtest that couldn't be removed says so, and the step starts with
// deleting it.
func TestDownloadFailureMessageDeleteFirst(t *testing.T) {
	t.Parallel()
	err := plainSentence(downloadFailureMessage("oef-2024q1", "alpaca",
		[]downloadMiss{barsFailed("XYZ", http.StatusInternalServerError)}, false))
	require.EqualError(t, err, "Couldn't load data for backtest 'oef-2024q1': foreverbull couldn't download prices for "+
		"XYZ from alpaca: alpaca had a server error (HTTP 500). The backtest was partly saved and couldn't be removed. "+
		"Run `fbull backtest delete oef-2024q1` first. "+
		"Run `fbull backtest create` again in a few minutes, or try another provider with --provider.")
}

// Failed downloads share the mixed block with failed lookups and missing
// prices, under one lead, and their steps join the others.
func TestMixedCoverageMessageListsFailedDownloads(t *testing.T) {
	t.Parallel()
	day := time.Date(2024, 2, 14, 0, 0, 0, 0, time.UTC)
	misses := []lookupMiss{miss("XYZ", false, lookupErr(finance.LookupHTTP, "alpaca", 503))}
	downloads := []downloadMiss{barsFailed("ABC", http.StatusForbidden)}
	err := plainSentence(mixedCoverageMessage("oef-2024q1", "alpaca", []time.Time{day}, nil, misses, downloads, true))
	require.EqualError(t, err, "Couldn't load data for backtest 'oef-2024q1': 2 symbols are missing prices or "+
		"couldn't be checked. The backtest wasn't saved.\n"+
		"  XYZ: couldn't check whether it was delisted: alpaca had a server error (HTTP 503).\n"+
		"  ABC: couldn't download prices: alpaca didn't accept foreverbull's API key (HTTP 403).\n"+
		"Run `fbull backtest create` again in a few minutes, or try another provider with --provider. "+
		"Check your alpaca API key and secret, then run `fbull backtest create` again.")

	inside := []symbolGaps{{symbol: "AAA", sessions: []int{0}}}
	err = plainSentence(mixedCoverageMessage("oef-2024q1", "alpaca", []time.Time{day}, inside, nil,
		[]downloadMiss{barsFailed("ABC", http.StatusUnprocessableEntity)}, false))
	require.EqualError(t, err, "Couldn't load data for backtest 'oef-2024q1': 2 symbols are missing prices or "+
		"couldn't be checked. The backtest was partly saved and couldn't be removed.\n"+
		"  AAA: 2024-02-14\n"+
		"  ABC: couldn't download prices: alpaca rejected the request as invalid (HTTP 422).\n"+
		"Run `fbull backtest delete oef-2024q1` first. Check the symbol and the backtest dates, or try another "+
		"provider with --provider, then run `fbull backtest create` again."+issuesStep+
		" Or change the backtest dates, then run `fbull backtest create` again.")
}

func TestMixedFailures(t *testing.T) {
	t.Parallel()
	require.False(t, mixedFailures(0, 0, 3))
	require.False(t, mixedFailures(2, 0, 0))
	require.True(t, mixedFailures(1, 0, 1))
	require.True(t, mixedFailures(0, 1, 1))
	require.True(t, mixedFailures(1, 1, 0))
}

func dividendsFailed(symbol string, kind financeapi.DividendFetchKind, status int) downloadMiss {
	return downloadMiss{symbol: symbol, dividends: true,
		cause: &financeapi.DividendFetchError{Provider: "yahoo", Symbol: symbol, Kind: kind, Status: status}}
}

// A failed dividend download is collected and worded like a failed price
// download, with the same step.
func TestDownloadFailureMessageNamesDividends(t *testing.T) {
	t.Parallel()
	err := plainSentence(downloadFailureMessage("oef-2024q1", "yahoo",
		[]downloadMiss{dividendsFailed("KO", financeapi.DividendHTTPStatus, http.StatusServiceUnavailable)}, true))
	require.EqualError(t, err, "Couldn't load data for backtest 'oef-2024q1': foreverbull couldn't download dividends for "+
		"KO from yahoo: yahoo had a server error (HTTP 503). The backtest wasn't saved. "+
		"Run `fbull backtest create` again in a few minutes, or try another provider with --provider.")

	err = plainSentence(downloadFailureMessage("oef-2024q1", "yahoo",
		[]downloadMiss{dividendsFailed("KO", financeapi.DividendNoAnswer, 0), dividendsFailed("PEP", financeapi.DividendUnreadable, 0)}, true))
	require.EqualError(t, err, "Couldn't load data for backtest 'oef-2024q1': foreverbull couldn't download dividends for "+
		"2 symbols from yahoo. The backtest wasn't saved.\n"+
		"  KO: yahoo didn't answer.\n"+
		"  PEP: yahoo's answer couldn't be read.\n"+
		"Check your internet connection, then run `fbull backtest create` again. "+
		"Try another provider with --provider, then run `fbull backtest create` again."+issuesStep)

	err = plainSentence(downloadFailureMessage("oef-2024q1", "yahoo",
		[]downloadMiss{barsFailedFrom("yahoo", "XYZ", http.StatusServiceUnavailable), dividendsFailed("KO", financeapi.DividendHTTPStatus, http.StatusServiceUnavailable)}, true))
	require.EqualError(t, err, "Couldn't load data for backtest 'oef-2024q1': foreverbull couldn't download prices or dividends for "+
		"2 symbols from yahoo. The backtest wasn't saved.\n"+
		"  XYZ: couldn't download prices: yahoo had a server error (HTTP 503).\n"+
		"  KO: couldn't download dividends: yahoo had a server error (HTTP 503).\n"+
		"Run `fbull backtest create` again in a few minutes, or try another provider with --provider.")
}
