package command_test

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lhjnilsson/foreverbull/internal/stream"
	"github.com/lhjnilsson/foreverbull/pkg/backtest/engine"
	"github.com/lhjnilsson/foreverbull/pkg/backtest/internal/repository"
	"github.com/lhjnilsson/foreverbull/pkg/backtest/internal/stream/command"
	"github.com/lhjnilsson/foreverbull/pkg/backtest/internal/stream/dependency"
	"github.com/lhjnilsson/foreverbull/pkg/pb"
	"github.com/lhjnilsson/foreverbull/pkg/pb/backtest"
	financeServicePB "github.com/lhjnilsson/foreverbull/pkg/pb/finance/service"
	"github.com/stretchr/testify/mock"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The backtest the copy in the brief names: OEF as the benchmark, 2024 Q1.
const quarterBacktest = "oef-2024q1"

var (
	quarterStart = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	quarterEnd   = time.Date(2024, 3, 31, 0, 0, 0, 0, time.UTC)
	// afterEaster runs past Good Friday, so a bar on 2024-04-01 puts
	// 2024-03-29 inside the symbol's span.
	afterEaster = time.Date(2024, 4, 5, 0, 0, 0, 0, time.UTC)
)

// xnysSessions is what Zipline's XNYS calendar answers from 2024-01-01 to
// end: weekdays without New Year's Day, MLK Day, Presidents Day and Good
// Friday. The zipline tests check the real calendar leaves Good Friday out.
func xnysSessions(end time.Time) []time.Time {
	return sessionsBetween(quarterStart, end)
}

// sessionsBetween is the weekday calendar the download engine answers for
// any window, including the 10 sessions before a period. The 2024 holidays
// the quarter tests name, and Christmas 2023, are not sessions.
func sessionsBetween(start, end time.Time) []time.Time {
	holidays := map[string]bool{
		"2023-12-25": true,
		"2024-01-01": true, "2024-01-15": true, "2024-02-19": true, "2024-03-29": true,
	}
	start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
	end = time.Date(end.Year(), end.Month(), end.Day(), 0, 0, 0, 0, time.UTC)
	sessions := make([]time.Time, 0, 80)
	for day := start; !day.After(end); day = day.AddDate(0, 0, 1) {
		if day.Weekday() == time.Saturday || day.Weekday() == time.Sunday || holidays[day.Format(time.DateOnly)] {
			continue
		}
		sessions = append(sessions, day)
	}
	return sessions
}

// testClock is the suite's fixed clock: 01:00 New York on Saturday
// 2026-10-31, before that day's 16:30, so the last completed session date
// is 2026-10-30. Every 2022 and 2024 period is wholly before it.
var testClock = time.Date(2026, 10, 31, 5, 0, 0, 0, time.UTC)

// fixClock fixes the clock the session check caps the period with.
func (test *BacktestCommandTest) fixClock(now time.Time) {
	test.restoreClock()
	test.restore = command.SetSessionClock(func() time.Time { return now })
}

func (test *BacktestCommandTest) restoreClock() {
	if test.restore != nil {
		test.restore()
		test.restore = nil
	}
}

// weekdayEngine is an engine whose calendar has every weekday as a
// session, for a test that only needs the period read before the download.
func weekdayEngine() *engine.MockEngine {
	ziplineEngine := new(engine.MockEngine)
	ziplineEngine.On("Sessions", mock.Anything, mock.Anything, mock.Anything).Return(
		func(_ context.Context, start, end time.Time) ([]time.Time, error) {
			var sessions []time.Time
			for day := start; !day.After(end); day = day.AddDate(0, 0, 1) {
				if day.Weekday() != time.Saturday && day.Weekday() != time.Sunday {
					sessions = append(sessions, day)
				}
			}
			return sessions, nil
		})
	return ziplineEngine
}

// useEngine makes msg return ziplineEngine, in place of any engine set
// before.
func useEngine(msg *stream.MockMessage, ziplineEngine engine.Engine) {
	calls := msg.ExpectedCalls[:0]
	for _, call := range msg.ExpectedCalls {
		if call.Method == "Call" && len(call.Arguments) == 2 && call.Arguments[1] == dependency.GetEngineKey {
			continue
		}
		calls = append(calls, call)
	}
	msg.ExpectedCalls = calls
	msg.On("Call", mock.Anything, dependency.GetEngineKey).Return(ziplineEngine, nil)
}

// resetBars gives the suite an empty finance ohlc table. Recreate only
// rebuilds the backtest tables, and the finance module owns this one.
func (test *BacktestCommandTest) resetBars() {
	_, err := test.db.Exec(context.Background(), `
		DROP TABLE IF EXISTS ohlc;
		DROP TABLE IF EXISTS asset;
		CREATE TABLE asset (symbol text primary key, name text);
		CREATE TABLE ohlc (
			symbol TEXT references asset(symbol),
			open numeric NOT NULL, high numeric NOT NULL, low numeric NOT NULL, close numeric NOT NULL,
			volume bigint NOT NULL,
			time timestamp,
			provider text NOT NULL,
			UNIQUE (provider, symbol, time));`)
	test.Require().NoError(err)
}

// storeBars stores a yahoo bar for symbol on every session from first to
// last except skip. Yahoo stamps the 09:30 ET open, 14:30 UTC.
func (test *BacktestCommandTest) storeBars(symbol, first, last string, skip ...string) {
	ctx := context.Background()
	_, err := test.db.Exec(ctx, `INSERT INTO asset (symbol, name) VALUES ($1, $1) ON CONFLICT DO NOTHING`, symbol)
	test.Require().NoError(err)
	skipped := make(map[string]bool, len(skip))
	for _, day := range skip {
		skipped[day] = true
	}
	for _, session := range xnysSessions(afterEaster) {
		day := session.Format(time.DateOnly)
		if day < first || day > last || skipped[day] {
			continue
		}
		_, err = test.db.Exec(ctx, `
			INSERT INTO ohlc (symbol, open, high, low, close, volume, time, provider)
			VALUES ($1, 10, 11, 9, 10, 100, $2, 'yahoo')`, symbol, session.Add(14*time.Hour+30*time.Minute))
		test.Require().NoError(err)
	}
}

// createQuarter stores the backtest that CREATING would have stored, for
// 2024 Q1.
func (test *BacktestCommandTest) createQuarter(symbols ...string) {
	test.createBacktest(quarterEnd, symbols...)
}

func (test *BacktestCommandTest) createBacktest(end time.Time, symbols ...string) {
	test.createPeriod(quarterStart, end, symbols...)
	test.storeBars("OEF", "2024-01-01", end.Format(time.DateOnly))
}

// createPeriod stores the backtest that CREATING would have stored, from
// start to end, with no bars.
func (test *BacktestCommandTest) createPeriod(start, end time.Time, symbols ...string) {
	assets := make([]*backtest.Backtest_Period_Asset, 0, len(symbols))
	for _, symbol := range symbols {
		assets = append(assets, &backtest.Backtest_Period_Asset{Symbol: symbol, Enabled: true})
	}
	backtests := repository.Backtest{Conn: test.db}
	_, err := backtests.Create(context.TODO(), &backtest.Backtest{
		Name:      quarterBacktest,
		Benchmark: "OEF",
		Periods: []*backtest.Backtest_Period{{
			StartDate: pb.GoTimeToDate(start),
			EndDate:   pb.GoTimeToDate(end),
		}},
	})
	test.Require().NoError(err)
	err = backtests.UpdatePeriod(context.TODO(), quarterBacktest, &backtest.Backtest_Period{
		StartDate: pb.GoTimeToDate(start),
		EndDate:   pb.GoTimeToDate(end),
		Assets:    assets,
	})
	test.Require().NoError(err)
}

// downloadQuarter runs DOWNLOADING for 2024 Q1 with a finance stream that
// stored every symbol OK, and an engine whose calendar is XNYS.
func (test *BacktestCommandTest) downloadQuarter() (*engine.MockEngine, error) {
	return test.download(quarterEnd)
}

func (test *BacktestCommandTest) download(end time.Time, answers ...*financeServicePB.DownloadHistoricalDataResponse) (*engine.MockEngine, error) {
	clientStream := new(financeServicePB.MockMarketdata_DownloadHistoricalDataClient[interface{}])
	for _, answer := range answers {
		clientStream.On("Recv", mock.Anything).Return(answer, nil).Once()
	}
	clientStream.On("Recv", mock.Anything).Return(nil, io.EOF)
	test.mdclient.On("DownloadHistoricalData", mock.Anything, mock.Anything).Return(clientStream, nil)

	ziplineEngine := new(engine.MockEngine)
	ziplineEngine.On("Sessions", mock.Anything, mock.Anything, mock.Anything).Return(
		func(_ context.Context, start, end time.Time) ([]time.Time, error) {
			return sessionsBetween(start, end), nil
		})
	msg := test.downloadMessage(quarterBacktest)
	useEngine(msg, ziplineEngine)
	return ziplineEngine, command.DownloadFinancialData(context.TODO(), msg)
}

// requireNothingSaved checks the failed create left no backtest, period or
// asset behind, so the same create can run again.
func (test *BacktestCommandTest) requireNothingSaved() {
	backtests := repository.Backtest{Conn: test.db}
	_, err := backtests.Get(context.TODO(), quarterBacktest)
	test.Require().ErrorIs(err, pgx.ErrNoRows)
	var periods, assets int
	test.Require().NoError(test.db.QueryRow(context.TODO(),
		`SELECT count(*) FROM backtest_period WHERE backtest_name=$1`, quarterBacktest).Scan(&periods))
	test.Require().NoError(test.db.QueryRow(context.TODO(),
		`SELECT count(*) FROM backtest_period_asset WHERE backtest_name=$1`, quarterBacktest).Scan(&assets))
	test.Zero(periods)
	test.Zero(assets)
}

func (test *BacktestCommandTest) requireKept() {
	backtests := repository.Backtest{Conn: test.db}
	_, err := backtests.Get(context.TODO(), quarterBacktest)
	test.Require().NoError(err)
}

// Quant check 1: one missing session fails create, nothing is saved, and the
// message names the symbol and the day.
func (test *BacktestCommandTest) TestMissingSessionOneSymbolOneDay() {
	test.createQuarter("XYZ")
	test.storeBars("XYZ", "2024-01-01", "2024-03-31", "2024-02-14")

	_, err := test.downloadQuarter()
	test.Require().EqualError(err, "Couldn't load data for backtest 'oef-2024q1': yahoo is missing prices for XYZ on "+
		"2024-02-14, a trading day. The backtest wasn't saved. Try another provider with --provider, or change the "+
		"backtest dates, then run `fbull backtest create` again.")
	test.requireNothingSaved()
}

func (test *BacktestCommandTest) TestMissingSessionOneSymbolSeveralDays() {
	test.createQuarter("XYZ")
	test.storeBars("XYZ", "2024-01-01", "2024-03-31", "2024-02-14", "2024-02-20", "2024-02-21")

	_, err := test.downloadQuarter()
	test.Require().EqualError(err, "Couldn't load data for backtest 'oef-2024q1': yahoo is missing prices for XYZ on "+
		"3 trading days: 2024-02-14, 2024-02-20 to 2024-02-21. The backtest wasn't saved. Try another provider with "+
		"--provider, or change the backtest dates, then run `fbull backtest create` again.")
	test.requireNothingSaved()
}

// Quant check 2: two symbols with 1 and 3 missing sessions fail with one
// error that names both and all four days, in the order they were listed.
func (test *BacktestCommandTest) TestMissingSessionsTwoSymbols() {
	test.createQuarter("AAA", "BBB")
	test.storeBars("AAA", "2024-01-01", "2024-03-31", "2024-02-14")
	test.storeBars("BBB", "2024-01-01", "2024-03-31", "2024-02-20", "2024-02-21", "2024-03-05")

	_, err := test.downloadQuarter()
	test.Require().EqualError(err, "Couldn't load data for backtest 'oef-2024q1': yahoo is missing prices on trading "+
		"days for 2 symbols. The backtest wasn't saved.\n"+
		"  AAA: 2024-02-14\n"+
		"  BBB: 2024-02-20 to 2024-02-21, 2024-03-05\n"+
		"Try another provider with --provider, or change the backtest dates, then run `fbull backtest create` again.")
	test.requireNothingSaved()
}

// A run of missing sessions is one range even across a holiday: Friday
// 2024-02-16 and Tuesday 2024-02-20 are sessions in a row (Presidents Day).
func (test *BacktestCommandTest) TestMissingSessionsRangeSpansAHoliday() {
	test.createQuarter("XYZ")
	test.storeBars("XYZ", "2024-01-01", "2024-03-31", "2024-02-16", "2024-02-20")

	_, err := test.downloadQuarter()
	test.Require().ErrorContains(err, "for XYZ on 2 trading days: 2024-02-16 to 2024-02-20. ")
}

// A row stored as 0 before the broker refused non-positive prices is not a
// price, so its day is missing, as the ingester reads it.
func (test *BacktestCommandTest) TestMissingSessionCountsAZeroPricedRow() {
	test.createQuarter("XYZ")
	test.storeBars("XYZ", "2024-01-01", "2024-03-31")
	_, err := test.db.Exec(context.TODO(), `UPDATE ohlc SET open = 0, high = 0, low = 0, close = 0
		WHERE symbol = 'XYZ' AND time::date = '2024-02-14'`)
	test.Require().NoError(err)

	_, err = test.downloadQuarter()
	test.Require().ErrorContains(err, "yahoo is missing prices for XYZ on 2024-02-14, a trading day.")
	test.requireNothingSaved()
}

// unconfirmedBefore and unconfirmedAfter are the reason a gap at the start
// or end of the period fails when the provider did not confirm it.
const (
	unconfirmedBefore = "yahoo didn't confirm it was listed then, so foreverbull treats it as missing data."
	unconfirmedAfter  = "yahoo didn't confirm it was delisted then, so foreverbull treats it as missing data."
	unconfirmedBoth   = "yahoo didn't confirm a listing or delisting, so foreverbull treats it as missing data."
)

// A symbol whose first bar is after the period start fails create, with
// the start-gap copy. No listing date is stored, and the first bar is
// not evidence of one.
func (test *BacktestCommandTest) TestMissingSessionsBeforeTheFirstBarFail() {
	test.createQuarter("NEWCO")
	test.storeBars("NEWCO", "2024-02-15", "2024-03-31")

	ziplineEngine, err := test.downloadQuarter()
	test.Require().EqualError(err, "Couldn't load data for backtest 'oef-2024q1': NEWCO has no prices before "+
		"2024-02-15, and "+unconfirmedBefore+" The backtest wasn't saved. Choose a start on or after 2024-02-15, "+
		"or remove NEWCO, then run `fbull backtest create` again.")
	ziplineEngine.AssertExpectations(test.T())
	test.requireNothingSaved()
}

// A symbol whose last bar is before the period end fails create, with the
// end-gap copy. No delisting is confirmed, and the last bar is not
// evidence of one.
func (test *BacktestCommandTest) TestMissingSessionsAfterTheLastBarFail() {
	test.createQuarter("XYZ")
	test.storeBars("XYZ", "2024-01-01", "2024-03-01")

	_, err := test.downloadQuarter()
	test.Require().EqualError(err, "Couldn't load data for backtest 'oef-2024q1': XYZ has no prices after "+
		"2024-03-01, and "+unconfirmedAfter+" The backtest wasn't saved. Choose an end on or before 2024-03-01, "+
		"or remove XYZ, then run `fbull backtest create` again.")
	test.requireNothingSaved()
}

// A provider that drops the period's last session, as Yahoo's exclusive
// period2 did to 2024-03-28 (Good Friday follows), fails create and names
// the last day it has.
func (test *BacktestCommandTest) TestMissingLastSessionFails() {
	test.createQuarter("XYZ")
	test.storeBars("XYZ", "2024-01-01", "2024-03-27")

	_, err := test.downloadQuarter()
	test.Require().EqualError(err, "Couldn't load data for backtest 'oef-2024q1': XYZ has no prices after "+
		"2024-03-27, and "+unconfirmedAfter+" The backtest wasn't saved. Choose an end on or before 2024-03-27, "+
		"or remove XYZ, then run `fbull backtest create` again.")
	test.requireNothingSaved()
}

// The benchmark is checked like every symbol: OEF without its last session
// fails create too.
func (test *BacktestCommandTest) TestMissingLastSessionOfTheBenchmarkFails() {
	test.createQuarter("XYZ")
	test.storeBars("XYZ", "2024-01-01", "2024-03-31")
	_, err := test.db.Exec(context.TODO(), `DELETE FROM ohlc WHERE symbol = 'OEF' AND time::date = '2024-03-28'`)
	test.Require().NoError(err)

	_, err = test.downloadQuarter()
	test.Require().ErrorContains(err, "OEF has no prices after 2024-03-27, and ")
	test.requireNothingSaved()
}

// Every cause of every symbol is collected before create fails once: a
// late first bar, an early last bar, and a missing day inside a span, with
// the edge reason and the provider or dates step.
func (test *BacktestCommandTest) TestMissingSessionsCollectEveryCause() {
	test.createQuarter("AAA", "NEWCO", "OLDCO")
	test.storeBars("AAA", "2024-01-01", "2024-03-31", "2024-02-14")
	test.storeBars("NEWCO", "2024-02-15", "2024-03-31")
	test.storeBars("OLDCO", "2024-01-01", "2024-03-01")

	_, err := test.downloadQuarter()
	test.Require().EqualError(err, "Couldn't load data for backtest 'oef-2024q1': yahoo is missing prices on trading "+
		"days for 3 symbols. The backtest wasn't saved.\n"+
		"  AAA: 2024-02-14\n"+
		"  NEWCO: no prices before 2024-02-15.\n"+
		"  OLDCO: no prices after 2024-03-01.\n"+
		unconfirmedBoth+" Try another provider with --provider, or change the backtest dates, then run "+
		"`fbull backtest create` again.")
	test.requireNothingSaved()
}

// Gaps at the edges only: the step names the start and end that every
// symbol has prices for.
func (test *BacktestCommandTest) TestMissingSessionsAtBothEdgesNameTheDates() {
	test.createQuarter("NEWCO", "OLDCO")
	test.storeBars("NEWCO", "2024-02-15", "2024-03-20")
	test.storeBars("OLDCO", "2024-01-01", "2024-03-01")

	_, err := test.downloadQuarter()
	test.Require().EqualError(err, "Couldn't load data for backtest 'oef-2024q1': yahoo is missing prices on trading "+
		"days for 2 symbols. The backtest wasn't saved.\n"+
		"  NEWCO: no prices before 2024-02-15 or after 2024-03-20.\n"+
		"  OLDCO: no prices after 2024-03-01.\n"+
		unconfirmedBoth+" Choose a start on or after 2024-02-15 and an end on or before 2024-03-01, or remove "+
		"NEWCO and OLDCO, then run `fbull backtest create` again.")
	test.requireNothingSaved()
}

// Quant check 5: Good Friday 2024-03-29 is not an XNYS session, so having
// no bar that day, between bars on 2024-03-28 and 2024-04-01, is not a
// missing price.
func (test *BacktestCommandTest) TestMissingSessionsSkipGoodFriday() {
	test.createBacktest(afterEaster, "XYZ")
	test.storeBars("XYZ", "2024-01-01", "2024-04-05")
	test.NotContains(xnysSessions(afterEaster), time.Date(2024, 3, 29, 0, 0, 0, 0, time.UTC))

	ziplineEngine, err := test.download(afterEaster)
	test.Require().NoError(err)
	ziplineEngine.AssertExpectations(test.T())
	test.requireKept()
}

// The symbols named are the request symbols. BRK.B is stored as BRKB, and
// the message still says BRK.B.
func (test *BacktestCommandTest) TestMissingSessionNamesTheRequestSymbol() {
	test.createQuarter("BRK.B")
	test.storeBars("BRKB", "2024-01-01", "2024-03-31", "2024-03-05")

	_, err := test.downloadQuarter()
	test.Require().ErrorContains(err, "yahoo is missing prices for BRK.B on 2024-03-05, a trading day.")
}

// A symbol with no stored bar at all fails create at DOWNLOADING, before
// INGESTING runs, even when the finance stream said its download was OK:
// the whole period is uncovered, and the ingest is never asked to notice.
func (test *BacktestCommandTest) TestMissingSessionsNoBarsFail() {
	test.createQuarter("XYZ")

	ziplineEngine, err := test.downloadQuarter()
	test.Require().EqualError(err, "Couldn't load data for backtest 'oef-2024q1': yahoo has no prices for XYZ "+
		"between 2024-01-02 and 2024-03-28. The backtest wasn't saved. Check the symbol and dates, or try another "+
		"provider with --provider, then run `fbull backtest create` again.")
	ziplineEngine.AssertExpectations(test.T())
	test.requireNothingSaved()
}

// A provider that sent a bar with no price fails that
// symbol's download with the days. They join the stored-bar check, so the
// create still fails once, naming every symbol: here BBB from the provider
// (no bars stored) and AAA from its stored bars.
func (test *BacktestCommandTest) TestMissingSessionsJoinTheProviderReport() {
	test.createQuarter("AAA", "BBB")
	test.storeBars("AAA", "2024-01-01", "2024-03-31", "2024-02-14")

	_, err := test.download(quarterEnd, &financeServicePB.DownloadHistoricalDataResponse{
		Symbol: "BBB",
		Status: financeServicePB.DownloadHistoricalDataResponse_STATUS_ERROR,
		Error:  "yahoo sent no price for BBB on 2024-02-20, 2024-02-21, 2024-03-05",
	})
	test.Require().EqualError(err, "Couldn't load data for backtest 'oef-2024q1': yahoo is missing prices on trading "+
		"days for 2 symbols. The backtest wasn't saved.\n"+
		"  AAA: 2024-02-14\n"+
		"  BBB: 2024-02-20 to 2024-02-21, 2024-03-05\n"+
		"Try another provider with --provider, or change the backtest dates, then run `fbull backtest create` again.")
	test.requireNothingSaved()
}

// The provider names the stored ticker; the message names the request
// symbol the finance service answered for.
func (test *BacktestCommandTest) TestMissingSessionFromTheProviderAlone() {
	test.createQuarter("XYZ")

	_, err := test.download(quarterEnd, &financeServicePB.DownloadHistoricalDataResponse{
		Symbol: "XYZ",
		Status: financeServicePB.DownloadHistoricalDataResponse_STATUS_ERROR,
		Error:  "yahoo sent no price for XYZ on 2024-02-14",
	})
	test.Require().EqualError(err, "Couldn't load data for backtest 'oef-2024q1': yahoo is missing prices for XYZ on "+
		"2024-02-14, a trading day. The backtest wasn't saved. Try another provider with --provider, or change the "+
		"backtest dates, then run `fbull backtest create` again.")
	test.requireNothingSaved()
}

// A provider that reported no price on a day that isn't a session (Good
// Friday, here) failed that symbol's download, so create fails naming the
// day rather than ingesting without the symbol. It isn't called a trading
// day.
func (test *BacktestCommandTest) TestMissingSessionReportedOnAHolidayFails() {
	test.createQuarter("XYZ")
	test.storeBars("XYZ", "2024-01-01", "2024-03-31")

	_, err := test.download(quarterEnd, &financeServicePB.DownloadHistoricalDataResponse{
		Symbol: "XYZ",
		Status: financeServicePB.DownloadHistoricalDataResponse_STATUS_ERROR,
		Error:  "yahoo sent no price for XYZ on 2024-03-29",
	})
	test.Require().EqualError(err, "Couldn't load data for backtest 'oef-2024q1': yahoo is missing prices for XYZ on "+
		"2024-03-29. The backtest wasn't saved. Try another provider with --provider, or change the "+
		"backtest dates, then run `fbull backtest create` again.")
	test.requireNothingSaved()
}

// A reported holiday next to a reported session reads as one run, and with
// a second symbol the lead doesn't say trading days.
func (test *BacktestCommandTest) TestMissingSessionsReportedAroundAHoliday() {
	test.createQuarter("AAA", "XYZ")
	test.storeBars("AAA", "2024-01-01", "2024-03-31", "2024-02-14")

	_, err := test.download(quarterEnd, &financeServicePB.DownloadHistoricalDataResponse{
		Symbol: "XYZ",
		Status: financeServicePB.DownloadHistoricalDataResponse_STATUS_ERROR,
		Error:  "yahoo sent no price for XYZ on 2024-03-28, 2024-03-29",
	})
	test.Require().EqualError(err, "Couldn't load data for backtest 'oef-2024q1': yahoo is missing prices for "+
		"2 symbols. The backtest wasn't saved.\n"+
		"  AAA: 2024-02-14\n"+
		"  XYZ: 2024-03-28 to 2024-03-29\n"+
		"Try another provider with --provider, or change the backtest dates, then run `fbull backtest create` again.")
	test.requireNothingSaved()
}

// coverDays stores a yahoo bar on each day for every symbol, and makes the
// download's engine answer those days as the period's sessions, so a test
// about something else has full coverage.
func (test *BacktestCommandTest) coverDays(msg *stream.MockMessage, days []string, symbols ...string) {
	sessions := make([]time.Time, 0, len(days))
	for _, day := range days {
		at, err := time.Parse(time.DateOnly, day)
		test.Require().NoError(err)
		sessions = append(sessions, at)
	}
	for _, symbol := range symbols {
		test.storeBarsOn(symbol, days...)
	}
	ziplineEngine := new(engine.MockEngine)
	ziplineEngine.On("Sessions", mock.Anything, mock.Anything, mock.Anything).Return(sessions, nil)
	useEngine(msg, ziplineEngine)
}

// storeBarsOn stores a yahoo bar for symbol on each day, at 14:30 UTC.
func (test *BacktestCommandTest) storeBarsOn(symbol string, days ...string) {
	ctx := context.Background()
	_, err := test.db.Exec(ctx, `INSERT INTO asset (symbol, name) VALUES ($1, $1) ON CONFLICT DO NOTHING`, symbol)
	test.Require().NoError(err)
	for _, day := range days {
		at, err := time.Parse(time.DateOnly, day)
		test.Require().NoError(err)
		_, err = test.db.Exec(ctx, `
			INSERT INTO ohlc (symbol, open, high, low, close, volume, time, provider)
			VALUES ($1, 10, 11, 9, 10, 100, $2, 'yahoo')`, symbol, at.Add(14*time.Hour+30*time.Minute))
		test.Require().NoError(err)
	}
}

// The span is the symbol's first and last stored bar over all time, clipped
// to the period. Bars before the start mean the first two period sessions
// are missing, not a listing.
func (test *BacktestCommandTest) TestMissingSessionsAtThePeriodStartWithBarsBefore() {
	test.createQuarter("XYZ")
	test.storeBarsOn("XYZ", "2023-12-28", "2023-12-29")
	test.storeBars("XYZ", "2024-01-04", "2024-03-31")

	_, err := test.downloadQuarter()
	test.Require().ErrorContains(err, "yahoo is missing prices for XYZ on 2 trading days: 2024-01-02 to 2024-01-03. ")
	test.requireNothingSaved()
}

// Bars after the end, from an earlier, wider fetch, put stored.Last past
// the period end: the missing last period session is a missing day inside
// the span, not a delisting, and it fails create.
func (test *BacktestCommandTest) TestMissingSessionAtThePeriodEndWithBarsAfter() {
	test.createQuarter("XYZ")
	test.storeBars("XYZ", "2024-01-01", "2024-03-27")
	test.storeBarsOn("XYZ", "2024-04-01")

	_, err := test.downloadQuarter()
	test.Require().EqualError(err, "Couldn't load data for backtest 'oef-2024q1': yahoo is missing prices for XYZ on "+
		"2024-03-28, a trading day. The backtest wasn't saved. Try another provider with --provider, or change the "+
		"backtest dates, then run `fbull backtest create` again.")
	test.requireNothingSaved()
}

// One symbol with no prices at either end is one cause, in one sentence.
func (test *BacktestCommandTest) TestMissingSessionsAtBothEndsOfOneSymbol() {
	test.createQuarter("NEWCO")
	test.storeBars("NEWCO", "2024-02-15", "2024-03-20")

	_, err := test.downloadQuarter()
	test.Require().EqualError(err, "Couldn't load data for backtest 'oef-2024q1': NEWCO has no prices before "+
		"2024-02-15 or after 2024-03-20, and "+unconfirmedBoth+" The backtest wasn't saved. Choose a start on or after "+
		"2024-02-15 and an end on or before 2024-03-20, or remove NEWCO, then run `fbull backtest create` again.")
	test.requireNothingSaved()
}

// When no start and end leave a period every symbol has prices for (A's
// first day is after B's last), only removing them is offered.
func (test *BacktestCommandTest) TestMissingSessionsWithNoCommonPeriodOfferRemovingOnly() {
	test.createQuarter("AAA", "BBB")
	test.storeBars("AAA", "2024-03-01", "2024-03-31")
	test.storeBars("BBB", "2024-01-01", "2024-02-01")

	_, err := test.downloadQuarter()
	test.Require().EqualError(err, "Couldn't load data for backtest 'oef-2024q1': yahoo is missing prices on trading "+
		"days for 2 symbols. The backtest wasn't saved.\n"+
		"  AAA: no prices before 2024-03-01.\n"+
		"  BBB: no prices after 2024-02-01.\n"+
		unconfirmedBoth+" Remove AAA and BBB, then run `fbull backtest create` again.")
	test.requireNothingSaved()
}

// fbIsMeta records FB and META as one instrument's tickers.
func (test *BacktestCommandTest) fbIsMeta() {
	_, err := test.db.Exec(context.TODO(), `
		INSERT INTO instrument (id, isin, name) VALUES ('meta', 'US30303M1027', 'Meta Platforms Inc.');
		INSERT INTO asset_symbol (isin, symbol, from_date, to_date) VALUES
			('US30303M1027', 'FB', '2012-05-18', '2022-06-09'),
			('US30303M1027', 'META', '2022-06-09', NULL);`)
	test.Require().NoError(err)
}

// With no bar under any ticker of an instrument, it fails create, named once
// by its latest ticker.
func (test *BacktestCommandTest) TestMissingSessionsNameAnInstrumentWithNoBarsOnce() {
	test.createQuarter("FB")
	test.fbIsMeta()

	_, err := test.downloadQuarter()
	test.Require().EqualError(err, "Couldn't load data for backtest 'oef-2024q1': yahoo has no prices for "+
		"META (earlier FB) between 2024-01-02 and 2024-03-28. The backtest wasn't saved. Check the symbol and "+
		"dates, or try another provider with --provider, then run `fbull backtest create` again.")
	test.requireNothingSaved()
}

// The tickers of one instrument are read together: Yahoo keeps FB's history
// under META, and those bars cover FB.
func (test *BacktestCommandTest) TestMissingSessionsReadEveryTickerOfAnInstrument() {
	test.createQuarter("FB")
	test.fbIsMeta()
	test.storeBars("META", "2024-01-01", "2024-03-31")
	_, err := test.downloadQuarter()
	test.Require().NoError(err)
	test.requireKept()
}

// A create canceled during the check returns context.Canceled itself,
// unwrapped, like every other canceled create, and never the data service
// copy. The backtest is still removed.
func (test *BacktestCommandTest) TestCanceledMissingSessionCheckReturnsTheContextError() {
	test.createQuarter("XYZ")
	test.storeBars("XYZ", "2024-01-01", "2024-03-31")
	clientStream := new(financeServicePB.MockMarketdata_DownloadHistoricalDataClient[interface{}])
	clientStream.On("Recv", mock.Anything).Return(nil, io.EOF)
	test.mdclient.On("DownloadHistoricalData", mock.Anything, mock.Anything).Return(clientStream, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ziplineEngine := new(engine.MockEngine)
	ziplineEngine.On("Sessions", mock.Anything, quarterStart, quarterEnd).
		Run(func(mock.Arguments) { cancel() }).
		Return(nil, status.Error(codes.Canceled, "context canceled"))
	msg := test.downloadMessage(quarterBacktest)
	useEngine(msg, ziplineEngine)

	err := command.DownloadFinancialData(ctx, msg)
	test.Require().ErrorIs(err, context.Canceled)
	test.Require().Equal(context.Canceled, err)
	test.NotContains(err.Error(), "data service")
	test.requireNothingSaved()
}

// A create canceled during the ingest does the same.
func (test *BacktestCommandTest) TestCanceledIngestReturnsTheContextError() {
	test.createQuarter("AAA")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ziplineEngine := new(engine.MockEngine)
	ziplineEngine.On("Ingest", mock.Anything, mock.Anything, mock.Anything).
		Run(func(mock.Arguments) { cancel() }).
		Return(nil, fmt.Errorf("error ingesting: %w", status.Error(codes.Canceled, "context canceled")))

	err := command.Ingest(ctx, test.ingestMessage(quarterBacktest, ziplineEngine))
	test.Require().ErrorIs(err, context.Canceled)
	test.Require().Equal(context.Canceled, err)
	test.NotContains(err.Error(), "data service")
	test.requireNothingSaved()
}

// Zipline's MissingSessionsError reaches the broker as DATA_LOSS. It reads
// the same as the download check's sentence, and the backtest is removed.
func (test *BacktestCommandTest) TestIngestMissingSessionsUseTheCreateSentence() {
	test.createQuarter("AAA", "BBB")
	ziplineEngine := new(engine.MockEngine)
	ziplineEngine.On("Ingest", mock.Anything, mock.Anything, mock.Anything).Return(nil, fmt.Errorf("error ingesting: %w",
		status.Error(codes.DataLoss, "missing prices on trading days: AAA 2024-02-14; BBB 2024-02-20,2024-02-21,2024-03-05"+
			"\nDetails: MissingSessionsError: missing prices on trading days: …")))
	ziplineEngine.On("Sessions", mock.Anything, quarterStart, quarterEnd).Return(xnysSessions(quarterEnd), nil)

	err := command.Ingest(context.TODO(), test.ingestMessage(quarterBacktest, ziplineEngine))
	test.Require().EqualError(err, "Couldn't load data for backtest 'oef-2024q1': yahoo is missing prices on trading "+
		"days for 2 symbols. The backtest wasn't saved.\n"+
		"  AAA: 2024-02-14\n"+
		"  BBB: 2024-02-20 to 2024-02-21, 2024-03-05\n"+
		"Try another provider with --provider, or change the backtest dates, then run `fbull backtest create` again.")
	test.requireNothingSaved()
}

// Another DATA_LOSS text keeps the ingest failure copy.
func (test *BacktestCommandTest) TestIngestOtherDataLossKeepsTheIngestCopy() {
	test.createQuarter("AAA")
	ziplineEngine := new(engine.MockEngine)
	ziplineEngine.On("Ingest", mock.Anything, mock.Anything, mock.Anything).
		Return(nil, status.Error(codes.DataLoss, "something else"))

	err := command.Ingest(context.TODO(), test.ingestMessage(quarterBacktest, ziplineEngine))
	test.Require().ErrorContains(err, "the data service failed while ingesting prices.")
	ziplineEngine.AssertNotCalled(test.T(), "Sessions", mock.Anything, mock.Anything, mock.Anything)
}

// A DATA_LOSS list the sentence can't name in full falls back to the ingest
// copy, so the list and its symbol count are never short.
func (test *BacktestCommandTest) requireIngestCopyFor(reason string) {
	test.createQuarter("AAA")
	ziplineEngine := new(engine.MockEngine)
	ziplineEngine.On("Ingest", mock.Anything, mock.Anything, mock.Anything).
		Return(nil, status.Error(codes.DataLoss, reason))
	ziplineEngine.On("Sessions", mock.Anything, quarterStart, quarterEnd).Return(xnysSessions(quarterEnd), nil)

	err := command.Ingest(context.TODO(), test.ingestMessage(quarterBacktest, ziplineEngine))
	test.Require().ErrorContains(err, "the data service failed while ingesting prices.")
	test.NotContains(err.Error(), "is missing prices")
	test.requireNothingSaved()
}

// Zipline named a ticker that isn't in the backtest.
func (test *BacktestCommandTest) TestIngestMissingSessionsOfAnotherTickerKeepTheIngestCopy() {
	test.requireIngestCopyFor("missing prices on trading days: AAA 2024-02-14; ZZZ 2024-02-01")
}

// Zipline named a day that isn't a session (Good Friday).
func (test *BacktestCommandTest) TestIngestMissingSessionsOnAHolidayKeepTheIngestCopy() {
	test.requireIngestCopyFor("missing prices on trading days: AAA 2024-02-14,2024-03-29")
}

// reusedTicker records instrument X, OLDX until 2020 and NEWX since. With
// reused set, OLDX is also instrument Y's ticker from 2021: another company.
func (test *BacktestCommandTest) reusedTicker(reused bool) {
	_, err := test.db.Exec(context.TODO(), `
		INSERT INTO instrument (id, isin, name) VALUES ('x', 'XX0000000001', 'X Corp'), ('y', 'YY0000000001', 'Y Corp');
		INSERT INTO asset_symbol (isin, symbol, from_date, to_date) VALUES
			('XX0000000001', 'OLDX', '2010-01-04', '2020-01-02'),
			('XX0000000001', 'NEWX', '2020-01-02', NULL);`)
	test.Require().NoError(err)
	if reused {
		_, err = test.db.Exec(context.TODO(), `
			INSERT INTO asset_symbol (isin, symbol, from_date, to_date) VALUES ('YY0000000001', 'OLDX', '2021-01-04', NULL);`)
		test.Require().NoError(err)
	}
}

// A ticker's bars count for its instrument only inside
// that ticker's own dates. OLDX's 2024 bars are outside X's OLDX dates, so
// they don't fill NEWX's quarter, whether or not the reuse is recorded.
func (test *BacktestCommandTest) TestMissingSessionsCountATickersBarsOnlyInsideItsDates() {
	for _, reused := range []bool{false, true} {
		test.resetBars()
		test.Require().NoError(repository.Recreate(context.Background(), test.db))
		test.createQuarter("NEWX")
		test.reusedTicker(reused)
		test.storeBars("OLDX", "2024-01-01", "2024-03-31")

		_, err := test.downloadQuarter()
		test.Require().Error(err, "reused %v", reused)
		test.Require().Contains(err.Error(), "has no prices for NEWX", "reused %v", reused)
		test.requireNothingSaved()
	}
}

// A symbol held by more than one instrument is never
// grouped. Asked for on its own, OLDX is read alone, and X's bars under
// NEWX don't make it pass.
func (test *BacktestCommandTest) TestMissingSessionsNeverGroupASymbolOfTwoInstruments() {
	test.createQuarter("OLDX")
	test.reusedTicker(true)
	test.storeBars("NEWX", "2024-01-01", "2024-03-31")

	_, err := test.downloadQuarter()
	test.Require().EqualError(err, "Couldn't load data for backtest 'oef-2024q1': yahoo has no prices for OLDX "+
		"between 2024-01-02 and 2024-03-28. The backtest wasn't saved. Check the symbol and dates, or try another "+
		"provider with --provider, then run `fbull backtest create` again.")
	test.requireNothingSaved()
}

// A period with a real XNYS session that hasn't closed yet has nothing
// closed yet. At 08:00 New York on Friday 2026-10-30 that day's session is
// still open, so create fails once, before any provider is asked, and names
// the last session that has closed, Thursday 2026-10-29.
func (test *BacktestCommandTest) TestEmptyPeriodNothingClosedYetFailsBeforeTheDownload() {
	test.fixClock(time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC))
	today := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	test.createPeriod(today, today, "AAPL")

	err := command.DownloadFinancialData(context.TODO(), test.downloadMessage(quarterBacktest))
	test.Require().EqualError(err, "Couldn't load data for backtest 'oef-2024q1': no trading session between "+
		"2026-10-30 and 2026-10-30 has closed yet (sessions count as closed at 16:30 New York time). The backtest "+
		"wasn't saved. Choose a start on or before 2026-10-29, then run `fbull backtest create` again.")
	test.mdclient.AssertNotCalled(test.T(), "DownloadHistoricalData", mock.Anything, mock.Anything)
	test.requireNothingSaved()
}

// A future weekday is a real session that hasn't closed: on Saturday
// 2026-10-31, Monday 2026-11-02 reads as not closed yet, naming Friday
// 2026-10-30.
func (test *BacktestCommandTest) TestEmptyPeriodOnAFutureSessionIsNotClosedYet() {
	monday := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	test.createPeriod(monday, monday, "AAPL")

	err := command.DownloadFinancialData(context.TODO(), test.downloadMessage(quarterBacktest))
	test.Require().EqualError(err, "Couldn't load data for backtest 'oef-2024q1': no trading session between "+
		"2026-11-02 and 2026-11-02 has closed yet (sessions count as closed at 16:30 New York time). The backtest "+
		"wasn't saved. Choose a start on or before 2026-10-30, then run `fbull backtest create` again.")
	test.mdclient.AssertNotCalled(test.T(), "DownloadHistoricalData", mock.Anything, mock.Anything)
	test.requireNothingSaved()
}

// A period after the last completed session with no XNYS session in it,
// such as Saturday 2026-10-31 asked for on that Saturday, has no trading
// day: nothing in it can ever close, so it is not "not closed yet".
func (test *BacktestCommandTest) TestEmptyPeriodOnTodaysWeekendHasNoTradingDay() {
	saturday := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	test.createPeriod(saturday, saturday, "AAPL")

	err := command.DownloadFinancialData(context.TODO(), test.downloadMessage(quarterBacktest))
	test.Require().EqualError(err, "Couldn't load data for backtest 'oef-2024q1': there's no trading session "+
		"between 2026-10-31 and 2026-10-31. The backtest wasn't saved. Choose dates that include a trading day, "+
		"then run `fbull backtest create` again.")
	test.mdclient.AssertNotCalled(test.T(), "DownloadHistoricalData", mock.Anything, mock.Anything)
	test.requireNothingSaved()
}

// A past period with no trading day fails once, before any provider is
// asked, so no no-prices sentence follows.
func (test *BacktestCommandTest) TestEmptyPeriodWithNoTradingDayFailsBeforeTheDownload() {
	test.createPeriod(time.Date(2024, 3, 30, 0, 0, 0, 0, time.UTC), time.Date(2024, 3, 31, 0, 0, 0, 0, time.UTC), "AAPL")

	err := command.DownloadFinancialData(context.TODO(), test.downloadMessage(quarterBacktest))
	test.Require().EqualError(err, "Couldn't load data for backtest 'oef-2024q1': there's no trading session "+
		"between 2024-03-30 and 2024-03-31. The backtest wasn't saved. Choose dates that include a trading day, "+
		"then run `fbull backtest create` again.")
	test.mdclient.AssertNotCalled(test.T(), "DownloadHistoricalData", mock.Anything, mock.Anything)
	test.requireNothingSaved()
}

// The check stops at the last completed session: at 14:00 New York on
// 2024-03-28, that day's session hasn't closed, so its missing bar is not a
// gap, and the calendar is read only to 2024-03-27.
func (test *BacktestCommandTest) TestMissingSessionsStopAtTheLastCompletedSession() {
	test.fixClock(time.Date(2024, 3, 28, 18, 0, 0, 0, time.UTC))
	test.createQuarter("AAPL")
	test.resetBars()
	test.storeBars("OEF", "2024-01-01", "2024-03-27")
	test.storeBars("AAPL", "2024-01-01", "2024-03-27")

	ziplineEngine, err := test.download(time.Date(2024, 3, 27, 0, 0, 0, 0, time.UTC))
	test.Require().NoError(err)
	ziplineEngine.AssertCalled(test.T(), "Sessions", mock.Anything, quarterStart, time.Date(2024, 3, 27, 0, 0, 0, 0, time.UTC))
	ziplineEngine.AssertNotCalled(test.T(), "Sessions", mock.Anything, quarterStart, quarterEnd)
}

// Yahoo keeps a renamed instrument's history under its latest ticker, so
// META's bars dated before META's own start count for the FB/META group.
func (test *BacktestCommandTest) TestMissingSessionsCountTheLatestTickersBarsBeforeItsStart() {
	test.createQuarter("META")
	_, err := test.db.Exec(context.TODO(), `
		INSERT INTO instrument (id, isin, name) VALUES ('m', 'US30303M1027', 'Meta Platforms Inc');
		INSERT INTO asset_symbol (isin, symbol, from_date, to_date) VALUES
			('US30303M1027', 'FB', '2012-05-18', '2024-02-01'),
			('US30303M1027', 'META', '2024-02-01', NULL);`)
	test.Require().NoError(err)
	test.storeBars("META", "2024-01-01", "2024-03-31")

	_, err = test.downloadQuarter()
	test.Require().NoError(err)
}
