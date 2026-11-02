package command_test

import (
	"context"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lhjnilsson/foreverbull/pkg/backtest/internal/stream/command"
	financepkg "github.com/lhjnilsson/foreverbull/pkg/finance"
	financeServicePB "github.com/lhjnilsson/foreverbull/pkg/pb/finance/service"
)

// rawStatusText is STATUS_ERROR text with a URL and a body in it. Create
// builds its sentence from provider_status and never shows this text.
const rawStatusText = "error getting bars: GET https://data.example/v2/stocks/XYZ/bars?key=AKSECRET: {\"message\":\"hidden body\"}"

func providerStatus(symbol, provider string, code int) *financeServicePB.DownloadHistoricalDataResponse {
	return &financeServicePB.DownloadHistoricalDataResponse{
		Symbol: symbol,
		Status: financeServicePB.DownloadHistoricalDataResponse_STATUS_ERROR,
		Error:  rawStatusText,
		ProviderStatus: &financeServicePB.DownloadHistoricalDataResponse_ProviderStatus{
			Provider: provider, HttpStatus: int32(code),
		},
	}
}

func (test *BacktestCommandTest) requireNoRawStatusText(err error) {
	for _, hidden := range []string{"data.example", "AKSECRET", "hidden body", "error getting bars"} {
		test.NotContains(err.Error(), hidden)
	}
}

// A provider's HTTP error status fails create with its cause, read from
// the typed provider_status, and the backtest is removed.
func (test *BacktestCommandTest) TestDownloadFinancialDataNamesTheProviderStatus() {
	test.createQuarter("XYZ")

	_, err := test.download(quarterEnd, providerStatus("XYZ", "yahoo", http.StatusServiceUnavailable))

	test.Require().EqualError(err, "Couldn't load data for backtest 'oef-2024q1': foreverbull couldn't download prices "+
		"for XYZ from yahoo: yahoo had a server error (HTTP 503). The backtest wasn't saved. "+
		"Run `fbull backtest create` again in a few minutes, or try another provider with --provider.")
	test.requireNoRawStatusText(err)
	test.requireNothingSaved()
}

// Every failed download is collected before create fails, so one message
// names them all.
func (test *BacktestCommandTest) TestDownloadFinancialDataCollectsEveryProviderStatus() {
	test.createQuarter("XYZ", "ABC")

	_, err := test.download(quarterEnd,
		providerStatus("XYZ", "yahoo", http.StatusUnprocessableEntity),
		providerStatus("ABC", "yahoo", http.StatusTooManyRequests))

	test.Require().EqualError(err, "Couldn't load data for backtest 'oef-2024q1': foreverbull couldn't download prices "+
		"for 2 symbols from yahoo. The backtest wasn't saved.\n"+
		"  XYZ: yahoo rejected the request as invalid (HTTP 422).\n"+
		"  ABC: yahoo limited how many requests foreverbull can make.\n"+
		"Wait a few minutes, then run `fbull backtest create` again, or try another provider with --provider. "+
		"Check the symbol and the backtest dates, then run `fbull backtest create` again. "+
		"If it keeps happening, please report it at https://github.com/lhjnilsson/foreverbull/issues with this message.")
	test.requireNoRawStatusText(err)
	test.requireNothingSaved()
}

// A failed download shares the mixed block with another symbol's missing
// price. The failed symbol is not also reported as missing every session.
func (test *BacktestCommandTest) TestDownloadFinancialDataMixesProviderStatusWithMissingPrices() {
	test.createQuarter("XYZ", "AAA")
	test.storeBars("AAA", "2024-01-01", "2024-03-31", "2024-02-14")

	_, err := test.download(quarterEnd, providerStatus("XYZ", "yahoo", http.StatusInternalServerError))

	test.Require().EqualError(err, "Couldn't load data for backtest 'oef-2024q1': 2 symbols are missing prices or "+
		"couldn't be checked. The backtest wasn't saved.\n"+
		"  AAA: 2024-02-14\n"+
		"  XYZ: couldn't download prices: yahoo had a server error (HTTP 500).\n"+
		"Run `fbull backtest create` again in a few minutes, or try another provider with --provider. "+
		"Or change the backtest dates, then run `fbull backtest create` again.")
	test.requireNoRawStatusText(err)
	test.requireNothingSaved()
}

// A failed benchmark download leaves no benchmark: no stock stands in for
// it. BBB, the first other symbol create checks, covers the whole range
// and would confirm AAA's early stop if it stood in. Without the benchmark's
// bars nothing is confirmed, so the asset status is never asked, no event
// is stored, and create fails with one mixed block.
func (test *BacktestCommandTest) TestDownloadFinancialDataNeverReplacesAFailedBenchmark() {
	test.useAlpacaProfile()
	test.createPriced("alpaca", quarterEnd, "AAA", "BBB")
	test.storePriced("alpaca", "OEF", "2024-01-01", "2024-03-31")
	test.storePriced("alpaca", "BBB", "2024-01-01", "2024-03-31")
	test.storePriced("alpaca", "AAA", "2024-01-01", "2024-03-01")
	inspected := 0
	command.SetSymbolInspector(func(
		context.Context, *pgxpool.Pool, string, string, string, *time.Time, *time.Time,
	) (financepkg.SymbolFacts, error) {
		inspected++
		return financepkg.SymbolFacts{Gone: true, Evidence: "alpaca asset status inactive"}, nil
	})

	_, err := test.download(quarterEnd, providerStatus("OEF", "alpaca", http.StatusServiceUnavailable))

	test.Require().EqualError(err, "Couldn't load data for backtest 'oef-2024q1': 2 symbols are missing prices or "+
		"couldn't be checked. The backtest wasn't saved.\n"+
		"  AAA: no prices after 2024-03-01.\n"+
		"alpaca didn't confirm a delisting, so foreverbull treats it as missing data.\n"+
		"  OEF: couldn't download prices: alpaca had a server error (HTTP 503).\n"+
		"Run `fbull backtest create` again in a few minutes, or try another provider with --provider. "+
		"Choose an end on or before 2024-03-01, or remove AAA, then run `fbull backtest create` again.")
	test.Zero(inspected, "the asset status is never asked")
	test.Empty(test.symbolEvents())
	test.requireNoRawStatusText(err)
}
