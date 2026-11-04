package api_test

import (
	"net/http"
	"testing"

	"github.com/lhjnilsson/foreverbull/pkg/finance/api"
	servicePb "github.com/lhjnilsson/foreverbull/pkg/pb/finance/service"
	"github.com/stretchr/testify/require"
)

// Every HTTP status class has its cause and step, word for word. The
// STATUS_ERROR form ends with the download's re-run clause.
func TestBarsStatusErrorNamesTheCauseAndStep(t *testing.T) {
	t.Parallel()
	const report = " If it keeps happening, please report it at https://github.com/lhjnilsson/foreverbull/issues with this message."
	cases := map[int]struct{ cause, step string }{
		http.StatusUnprocessableEntity: {
			"alpaca rejected the request as invalid (HTTP 422)",
			"Check the symbol and the dates, or try another provider, then run the download again." + report,
		},
		http.StatusUnauthorized: {
			"alpaca didn't accept foreverbull's API key (HTTP 401)",
			"Check your alpaca API key and secret, or try another provider, then run the download again.",
		},
		http.StatusForbidden: {
			"alpaca didn't accept foreverbull's API key (HTTP 403)",
			"Check your alpaca API key and secret, or try another provider, then run the download again.",
		},
		http.StatusTooManyRequests: {
			"alpaca limited how many requests foreverbull can make",
			"Wait a few minutes, then run the download again, or try another provider.",
		},
		http.StatusInternalServerError: {
			"alpaca had a server error (HTTP 500)",
			"Run the download again in a few minutes, or try another provider.",
		},
		http.StatusServiceUnavailable: {
			"alpaca had a server error (HTTP 503)",
			"Run the download again in a few minutes, or try another provider.",
		},
		http.StatusTeapot: {
			"alpaca refused the request (HTTP 418)",
			"Try another provider, then run the download again.",
		},
	}
	for status, want := range cases {
		err := &api.BarsStatusError{Provider: "alpaca", Symbol: "XYZ", Status: status}
		require.Equal(t, want.cause, err.Cause(), status)
		require.NotContains(t, err.Cause(), "XYZ", "the symbol is only in the lead")
		require.EqualError(t, err, "foreverbull couldn't download prices for XYZ from alpaca: "+want.cause+". "+want.step)
	}
}

// The create form gives the lookup step sentences unchanged.
func TestStepTextForCreate(t *testing.T) {
	t.Parallel()
	require.Equal(t, "Check your internet connection, then run `fbull backtest create` again.",
		api.StepText(api.StepNetwork, api.CreateForm))
	require.Equal(t, "Run `fbull backtest create` again in a few minutes, or try another provider with --provider.",
		api.StepText(api.StepServer, api.CreateForm))
	require.Equal(t, "Wait a few minutes, then run `fbull backtest create` again, or try another provider with --provider.",
		api.StepText(api.StepRate, api.CreateForm))
	require.Equal(t, "Check that your alpaca API key and secret work for paper trading, or try another provider with --provider, "+
		"then run `fbull backtest create` again.", api.StepText(api.StepPaperKey, api.CreateForm))
	require.Equal(t, "Try another provider with --provider, then run `fbull backtest create` again.",
		api.StepText(api.StepRefused, api.CreateForm))
}

// DownloadFailure reads the typed status back from the response, not from
// its text, and gives nil for every other answer.
func TestDownloadFailureReadsTheProviderStatus(t *testing.T) {
	t.Parallel()
	rsp := &servicePb.DownloadHistoricalDataResponse{
		Symbol: "XYZ",
		Status: servicePb.DownloadHistoricalDataResponse_STATUS_ERROR,
		Error:  "any text at all",
		ProviderStatus: &servicePb.DownloadHistoricalDataResponse_ProviderStatus{
			Provider: "alpaca", HttpStatus: http.StatusUnprocessableEntity,
		},
	}
	var named *api.BarsStatusError
	require.ErrorAs(t, api.DownloadFailure(rsp), &named)
	require.Equal(t, &api.BarsStatusError{Provider: "alpaca", Symbol: "XYZ", Status: http.StatusUnprocessableEntity}, named)

	untyped := &servicePb.DownloadHistoricalDataResponse{
		Symbol: "XYZ",
		Status: servicePb.DownloadHistoricalDataResponse_STATUS_ERROR,
		Error:  "foreverbull couldn't download prices for XYZ from alpaca: alpaca had a server error (HTTP 503).",
	}
	require.NoError(t, api.DownloadFailure(untyped), "the text alone is never matched")
	ok := &servicePb.DownloadHistoricalDataResponse{
		Symbol: "XYZ", Status: servicePb.DownloadHistoricalDataResponse_STATUS_OK,
		ProviderStatus: rsp.GetProviderStatus(),
	}
	require.NoError(t, api.DownloadFailure(ok))
}

// A failed dividend download has the price download's form and causes,
// word for word, and says the prices weren't saved either.
func TestDividendFetchErrorNamesTheCauseAndStep(t *testing.T) {
	t.Parallel()
	const report = " If it keeps happening, please report it at https://github.com/lhjnilsson/foreverbull/issues with this message."
	cases := []struct {
		failure api.DividendFetchError
		want    string
	}{
		{
			api.DividendFetchError{Provider: "yahoo", Symbol: "KO", Kind: api.DividendHTTPStatus, Status: http.StatusServiceUnavailable},
			"foreverbull couldn't download dividends for KO from yahoo: yahoo had a server error (HTTP 503). " +
				"Its prices weren't saved either. Run the download again in a few minutes, or try another provider.",
		},
		{
			api.DividendFetchError{Provider: "Yahoo", Symbol: "KO", Kind: api.DividendHTTPStatus, Status: http.StatusTooManyRequests},
			"foreverbull couldn't download dividends for KO from yahoo: yahoo limited how many requests foreverbull can make. " +
				"Its prices weren't saved either. Wait a few minutes, then run the download again, or try another provider.",
		},
		{
			api.DividendFetchError{Provider: "alpaca", Symbol: "KO", Kind: api.DividendHTTPStatus, Status: http.StatusForbidden},
			"foreverbull couldn't download dividends for KO from alpaca: alpaca didn't accept foreverbull's API key (HTTP 403). " +
				"Its prices weren't saved either. Check your alpaca API key and secret, or try another provider, " +
				"then run the download again.",
		},
		{
			api.DividendFetchError{Provider: "yahoo", Symbol: "KO", Kind: api.DividendNoAnswer},
			"foreverbull couldn't download dividends for KO from yahoo: yahoo didn't answer. " +
				"Its prices weren't saved either. Check your internet connection, then run the download again.",
		},
		{
			api.DividendFetchError{Provider: "yahoo", Symbol: "KO", Kind: api.DividendUnreadable},
			"foreverbull couldn't download dividends for KO from yahoo: yahoo's answer couldn't be read. " +
				"Its prices weren't saved either. Try another provider, then run the download again." + report,
		},
	}
	for _, tc := range cases {
		require.EqualError(t, &tc.failure, tc.want)
		require.NotContains(t, tc.failure.Error(), "--provider")
	}
}

// dividend_failure crosses the RPC and reads back as the same error. An
// unknown kind or a status outside 300..599 reads as nothing.
func TestDividendFailureReadsTheProto(t *testing.T) {
	t.Parallel()
	sent := &api.DividendFetchError{Provider: "yahoo", Symbol: "KO", Kind: api.DividendHTTPStatus, Status: 503}
	rsp := &servicePb.DownloadHistoricalDataResponse{
		Symbol: "KO", Status: servicePb.DownloadHistoricalDataResponse_STATUS_ERROR, DividendFailure: sent.DividendFailureProto(),
	}
	require.Equal(t, sent, api.DividendFailure(rsp))

	for _, kind := range []api.DividendFetchKind{api.DividendNoAnswer, api.DividendUnreadable, api.DividendInvalid} {
		sent := &api.DividendFetchError{Provider: "alpaca", Symbol: "KO", Kind: kind}
		rsp.DividendFailure = sent.DividendFailureProto()
		require.Equal(t, sent, api.DividendFailure(rsp))
	}

	rsp.DividendFailure = &servicePb.DownloadHistoricalDataResponse_DividendFailure{Provider: "yahoo"}
	require.NoError(t, api.DividendFailure(rsp))
	rsp.DividendFailure = &servicePb.DownloadHistoricalDataResponse_DividendFailure{
		Provider: "yahoo", Kind: servicePb.DownloadHistoricalDataResponse_DividendFailure_KIND_HTTP_STATUS, HttpStatus: 200,
	}
	require.NoError(t, api.DividendFailure(rsp))
	rsp.Status = servicePb.DownloadHistoricalDataResponse_STATUS_OK
	rsp.DividendFailure = sent.DividendFailureProto()
	require.NoError(t, api.DividendFailure(rsp))
}
