package command_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lhjnilsson/foreverbull/internal/environment"
	bt "github.com/lhjnilsson/foreverbull/pkg/backtest/internal/backtest"
	"github.com/lhjnilsson/foreverbull/pkg/backtest/internal/repository"
	"github.com/lhjnilsson/foreverbull/pkg/backtest/internal/stream/command"
	financepkg "github.com/lhjnilsson/foreverbull/pkg/finance"
	"github.com/lhjnilsson/foreverbull/pkg/pb"
	backtestpb "github.com/lhjnilsson/foreverbull/pkg/pb/backtest"
)

// recordedIPODate is the firstTradeDate in testdata/yahoo_ipo_chart.json,
// read through the Yahoo chart parser the create check uses.
func (test *BacktestCommandTest) recordedIPODate() time.Time {
	body, err := os.ReadFile(filepath.Join(
		"..", "..", "..", "..", "finance", "internal", "suppliers", "marketdata", "testdata", "yahoo_ipo_chart.json"))
	test.Require().NoError(err)
	day, err := financepkg.FirstTradeDateFromChart("XYZ", body)
	test.Require().NoError(err)
	return day
}

func (test *BacktestCommandTest) useAlpacaProfile() {
	dir := test.T().TempDir()
	path := filepath.Join(dir, "profiles.yaml")
	body := []byte("profiles:\n  - name: alpaca-paper\n    provider: alpaca\n    apiKey: key\n    apiSecret: secret\n")
	test.Require().NoError(os.WriteFile(path, body, 0o600))
	test.T().Setenv(environment.TradingProfilesFile, path)
}

func (test *BacktestCommandTest) setFacts(facts financepkg.SymbolFacts, err error) {
	command.SetSymbolInspector(func(
		context.Context, *pgxpool.Pool, string, string, string, *time.Time, *time.Time,
	) (financepkg.SymbolFacts, error) {
		return facts, err
	})
}

func (test *BacktestCommandTest) createPriced(provider string, end time.Time, symbols ...string) {
	assets := make([]*backtestpb.Backtest_Period_Asset, 0, len(symbols))
	for _, symbol := range symbols {
		assets = append(assets, &backtestpb.Backtest_Period_Asset{Symbol: symbol, Enabled: true})
	}
	backtests := repository.Backtest{Conn: test.db}
	_, err := backtests.Create(context.Background(), &backtestpb.Backtest{
		Name:      quarterBacktest,
		Benchmark: "OEF",
		Provider:  provider,
		Periods: []*backtestpb.Backtest_Period{{
			StartDate: pb.GoTimeToDate(quarterStart),
			EndDate:   pb.GoTimeToDate(end),
		}},
	})
	test.Require().NoError(err)
	err = backtests.UpdatePeriod(context.Background(), quarterBacktest, &backtestpb.Backtest_Period{
		StartDate: pb.GoTimeToDate(quarterStart),
		EndDate:   pb.GoTimeToDate(end),
		Assets:    assets,
	})
	test.Require().NoError(err)
}

func (test *BacktestCommandTest) storePriced(provider, symbol, first, last string) {
	ctx := context.Background()
	_, err := test.db.Exec(ctx, `INSERT INTO asset (symbol, name) VALUES ($1, $1) ON CONFLICT DO NOTHING`, symbol)
	test.Require().NoError(err)
	for _, session := range xnysSessions(afterEaster) {
		day := session.Format(time.DateOnly)
		if day < first || day > last {
			continue
		}
		_, err = test.db.Exec(ctx, `
			INSERT INTO ohlc (symbol, open, high, low, close, volume, time, provider)
			VALUES ($1, 10, 11, 9, 10, 100, $2, $3)`,
			symbol, session.Add(14*time.Hour+30*time.Minute), provider)
		test.Require().NoError(err)
	}
}

func (test *BacktestCommandTest) symbolEvents() []repository.SymbolEvent {
	events, err := (&repository.Backtest{Conn: test.db}).SymbolEvents(context.Background(), quarterBacktest)
	test.Require().NoError(err)
	return events
}

// A confirmed delisting passes create. The benchmark keeps its bars, Alpaca
// says inactive, and the auto-close is stored for the run summary.
func (test *BacktestCommandTest) TestConfirmedAlpacaDelistingPassesAndIsStored() {
	test.useAlpacaProfile()
	test.createPriced("alpaca", quarterEnd, "XYZ")
	test.storePriced("alpaca", "OEF", "2024-01-01", "2024-03-31")
	test.storePriced("alpaca", "XYZ", "2024-01-01", "2024-03-01")
	test.setFacts(financepkg.SymbolFacts{Gone: true, Evidence: financepkg.EvidenceAlpacaInactive}, nil)

	_, err := test.downloadQuarter()
	test.Require().NoError(err)
	test.requireKept()

	events := test.symbolEvents()
	test.Require().Len(events, 1)
	test.Equal("XYZ", events[0].Symbol)
	test.Equal(repository.EventDelisted, events[0].Kind)
	test.Equal("2024-03-01", events[0].On.Format(time.DateOnly))
	test.Equal(financepkg.EvidenceAlpacaInactive, events[0].Evidence)
}

// The same bars, with Alpaca still calling the asset active, fail create.
func (test *BacktestCommandTest) TestAlpacaActiveSymbolIsNotDelisted() {
	test.useAlpacaProfile()
	test.createPriced("alpaca", quarterEnd, "XYZ")
	test.storePriced("alpaca", "OEF", "2024-01-01", "2024-03-31")
	test.storePriced("alpaca", "XYZ", "2024-01-01", "2024-03-01")
	test.setFacts(financepkg.SymbolFacts{}, nil)

	_, err := test.downloadQuarter()
	test.Require().ErrorContains(err, "XYZ has no prices after 2024-03-01, and ")
	test.requireNothingSaved()
}

// An end gap with no provider confirmation still fails.
func (test *BacktestCommandTest) TestUnconfirmedEndGapStillFails() {
	test.createPriced("yahoo", quarterEnd, "XYZ")
	test.storePriced("yahoo", "OEF", "2024-01-01", "2024-03-31")
	test.storePriced("yahoo", "XYZ", "2024-01-01", "2024-03-01")

	_, err := test.downloadQuarter()
	test.Require().EqualError(err, "Couldn't load data for backtest 'oef-2024q1': XYZ has no prices after "+
		"2024-03-01, and yahoo didn't confirm it was delisted then, so foreverbull treats it as missing data. "+
		"The backtest wasn't saved. Choose an end on or before 2024-03-01, or remove XYZ, then run "+
		"`fbull backtest create` again.")
	test.requireNothingSaved()
}

// A gap on every symbol, benchmark included, is a provider gap. Inactive
// does not make it a delisting.
func (test *BacktestCommandTest) TestGapOnEverySymbolIncludingTheBenchmarkFails() {
	test.useAlpacaProfile()
	test.createPriced("alpaca", quarterEnd, "XYZ")
	test.storePriced("alpaca", "OEF", "2024-01-01", "2024-03-01")
	test.storePriced("alpaca", "XYZ", "2024-01-01", "2024-03-01")
	test.setFacts(financepkg.SymbolFacts{Gone: true, Evidence: financepkg.EvidenceAlpacaInactive}, nil)

	_, err := test.downloadQuarter()
	test.Require().Error(err)
	test.Require().ErrorContains(err, "OEF")
	test.Require().ErrorContains(err, "XYZ")
	test.NotContains(err.Error(), financepkg.EvidenceAlpacaInactive)
	test.requireNothingSaved()
}

// A failed lookup is an error. It is never stored as a delisting.
func (test *BacktestCommandTest) TestFailedDelistingLookupDoesNotDelist() {
	test.useAlpacaProfile()
	test.createPriced("alpaca", quarterEnd, "XYZ")
	test.storePriced("alpaca", "OEF", "2024-01-01", "2024-03-31")
	test.storePriced("alpaca", "XYZ", "2024-01-01", "2024-03-01")
	test.setFacts(financepkg.SymbolFacts{}, &financepkg.LookupError{
		Kind: financepkg.LookupHTTP, Provider: "alpaca", Symbol: "XYZ", Status: 503,
		Err: errors.New("https://paper-api.alpaca.markets/v2/assets/XYZ {\"message\":\"no\"}"),
	})

	_, err := test.downloadQuarter()
	test.Require().EqualError(err, "Couldn't load data for backtest 'oef-2024q1': foreverbull couldn't check with "+
		"alpaca whether XYZ was delisted: alpaca had a server error (HTTP 503). The backtest wasn't saved. "+
		"Run `fbull backtest create` again in a few minutes, or try another provider with --provider.")
	test.NotContains(err.Error(), "paper-api.alpaca.markets")
	test.NotContains(err.Error(), "message")
	test.requireNothingSaved()
	test.Empty(test.symbolEvents())
}

// A failed listing lookup names the cause and is not stored as a listing.
func (test *BacktestCommandTest) TestFailedListingLookupDoesNotList() {
	test.createPriced("yahoo", quarterEnd, "XYZ")
	test.storePriced("yahoo", "OEF", "2024-01-01", "2024-03-31")
	test.storePriced("yahoo", "XYZ", "2024-03-15", "2024-03-31")
	test.setFacts(financepkg.SymbolFacts{}, &financepkg.LookupError{
		Kind: financepkg.LookupNetwork, Provider: "yahoo", Symbol: "XYZ",
		Err: errors.New("Get \"https://query2.finance.yahoo.com/v8/finance/chart/XYZ\": dial tcp: i/o timeout"),
	})

	_, err := test.downloadQuarter()
	test.Require().EqualError(err, "Couldn't load data for backtest 'oef-2024q1': foreverbull couldn't check with "+
		"yahoo when XYZ was listed: yahoo didn't answer. The backtest wasn't saved. "+
		"Check your internet connection, then run `fbull backtest create` again.")
	test.NotContains(err.Error(), "query2.finance.yahoo.com")
	test.requireNothingSaved()
	test.Empty(test.symbolEvents())
}

// Stooq has no listing source, so a symbol whose bars stop still fails
// even when a lookup would have said it was gone.
func (test *BacktestCommandTest) TestStooqEndGapFails() {
	test.createPriced("stooq", quarterEnd, "XYZ")
	test.storePriced("stooq", "OEF", "2024-01-01", "2024-03-31")
	test.storePriced("stooq", "XYZ", "2024-01-01", "2024-03-01")
	test.setFacts(financepkg.SymbolFacts{Gone: true, Evidence: financepkg.EvidenceAlpacaInactive}, nil)

	_, err := test.downloadQuarter()
	test.Require().ErrorContains(err, "XYZ has no prices after 2024-03-01, and stooq didn't confirm it was delisted then")
	test.requireNothingSaved()
}

// The recorded Yahoo IPO chart passes create and the run summary line names
// the day the symbol starts.
func (test *BacktestCommandTest) TestRecordedYahooIPOFixturePassesAndShowsTheStart() {
	listed := test.recordedIPODate()
	test.Equal("2024-03-15", listed.Format(time.DateOnly))
	test.createPriced("yahoo", quarterEnd, "XYZ")
	test.storePriced("yahoo", "OEF", "2024-01-01", "2024-03-31")
	test.storePriced("yahoo", "XYZ", listed.Format(time.DateOnly), "2024-03-31")
	test.setFacts(financepkg.SymbolFacts{LookbackEmpty: true, FirstTrade: listed}, nil)

	_, err := test.downloadQuarter()
	test.Require().NoError(err)
	test.requireKept()

	events := test.symbolEvents()
	test.Require().Len(events, 1)
	test.Equal("XYZ", events[0].Symbol)
	test.Equal(repository.EventListed, events[0].Kind)
	test.Equal("2024-03-15", events[0].On.Format(time.DateOnly))
	test.Equal("yahoo firstTradeDate 2024-03-15", events[0].Evidence)
	test.Equal("XYZ was listed on 2024-03-15 and has no prices before then.", bt.ListingLine(events[0].Symbol, events[0].On))
}

// Bars already stored before the start mean the first sessions are missing
// data, not a listing. Both sessions are named.
func (test *BacktestCommandTest) TestBarsBeforeTheStartNameBothMissingSessions() {
	test.createPriced("yahoo", quarterEnd, "XYZ")
	test.storePriced("yahoo", "OEF", "2024-01-01", "2024-03-31")
	test.storeBarsOn("XYZ", "2023-12-28", "2023-12-29")
	test.storePriced("yahoo", "XYZ", "2024-01-04", "2024-03-31")
	test.setFacts(financepkg.SymbolFacts{
		LookbackEmpty: true,
		FirstTrade:    time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC),
	}, nil)

	_, err := test.downloadQuarter()
	test.Require().ErrorContains(err, "yahoo is missing prices for XYZ on 2 trading days: 2024-01-02 to 2024-01-03.")
	test.requireNothingSaved()
}

// Alpaca cannot confirm a listing date, so a late first bar fails.
func (test *BacktestCommandTest) TestAlpacaLateFirstBarFails() {
	test.useAlpacaProfile()
	test.createPriced("alpaca", quarterEnd, "XYZ")
	test.storePriced("alpaca", "OEF", "2024-01-01", "2024-03-31")
	test.storePriced("alpaca", "XYZ", "2024-03-15", "2024-03-31")
	test.setFacts(financepkg.SymbolFacts{
		LookbackEmpty: true,
		FirstTrade:    time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC),
	}, nil)

	_, err := test.downloadQuarter()
	test.Require().ErrorContains(err, "XYZ has no prices before 2024-03-15, and ")
	test.requireNothingSaved()
}

// A first-trade date after the first stored bar contradicts the bars, so
// it is not a listing.
func (test *BacktestCommandTest) TestFirstTradeAfterTheFirstBarIsNotAListing() {
	test.createPriced("yahoo", quarterEnd, "XYZ")
	test.storePriced("yahoo", "OEF", "2024-01-01", "2024-03-31")
	test.storePriced("yahoo", "XYZ", "2024-03-15", "2024-03-31")
	test.setFacts(financepkg.SymbolFacts{
		LookbackEmpty: true,
		FirstTrade:    time.Date(2024, 3, 18, 0, 0, 0, 0, time.UTC),
	}, nil)

	_, err := test.downloadQuarter()
	test.Require().EqualError(err, "Couldn't load data for backtest 'oef-2024q1': XYZ has no prices before "+
		"2024-03-15, and yahoo didn't confirm it was listed then, so foreverbull treats it as missing data. "+
		"The backtest wasn't saved. Choose a start on or after 2024-03-15, or remove XYZ, then run "+
		"`fbull backtest create` again.")
	test.requireNothingSaved()
	test.Empty(test.symbolEvents())
}
