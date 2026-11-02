package command_test

import (
	"context"
	"time"

	"github.com/lhjnilsson/foreverbull/internal/stream"
	"github.com/lhjnilsson/foreverbull/pkg/backtest/engine"
	"github.com/lhjnilsson/foreverbull/pkg/backtest/internal/stream/command"
	"github.com/stretchr/testify/mock"
)

// beforeTheClose is 14:00 New York on Thursday 2024-03-28: that day's
// session is open, so the last completed session is 2024-03-27. Good Friday
// 2024-03-29 has no session, so 2024-03-28 is the only one a 2024 Q1
// backtest leaves out.
var beforeTheClose = time.Date(2024, 3, 28, 18, 0, 0, 0, time.UTC)

// createClosedThrough stores a provider's 2024 backtest ending on end, with
// OEF and AAPL priced on every session through last, the last one closed.
func (test *BacktestCommandTest) createClosedThrough(provider string, end time.Time, last string) {
	if provider == "alpaca" {
		test.useAlpacaProfile()
	}
	test.createPriced(provider, end, "AAPL")
	test.storePriced(provider, "OEF", "2024-01-01", last)
	test.storePriced(provider, "AAPL", "2024-01-01", last)
}

// requireNotice checks that DOWNLOADING completed with lines as its
// notice, and kept the backtest.
func (test *BacktestCommandTest) requireNotice(err error, lines ...string) {
	var notice *stream.Notice
	test.Require().ErrorAs(err, &notice)
	test.Equal(lines, notice.Lines)
	test.requireKept()
}

// Every provider's fetch stops at the last completed session, so each
// leaves the end date's open session out and says so, in the same words.
func (test *BacktestCommandTest) TestUnclosedEndSessionNoticeForEveryProvider() {
	for _, provider := range []string{"yahoo", "stooq", "alpaca"} {
		test.Run(provider, func() {
			test.SetupTest()
			test.fixClock(beforeTheClose)
			test.createClosedThrough(provider, quarterEnd, "2024-03-27")

			_, err := test.downloadQuarter()
			test.requireNotice(err, "2024-03-28 isn't included because its trading session hasn't closed yet "+
				"(sessions count as closed at 16:30 New York time).")
		})
	}
}

// From 16:30 New York the end date's session counts as closed, so nothing
// is left out and there is no notice.
func (test *BacktestCommandTest) TestClosedEndSessionHasNoNotice() {
	test.fixClock(time.Date(2024, 3, 28, 20, 30, 0, 0, time.UTC))
	end := time.Date(2024, 3, 28, 0, 0, 0, 0, time.UTC)
	test.createClosedThrough("yahoo", end, "2024-03-28")

	_, err := test.download(end)
	test.Require().NoError(err)
	test.requireKept()
}

// An end date in the past has no notice.
func (test *BacktestCommandTest) TestPastEndDateHasNoNotice() {
	test.createClosedThrough("yahoo", quarterEnd, "2024-03-31")

	_, err := test.downloadQuarter()
	test.Require().NoError(err)
	test.requireKept()
}

// An end on a weekend after the last completed session leaves no session
// out: on Saturday 2024-03-30, Thursday 2024-03-28 has closed and the days
// after it are Good Friday and the weekend.
func (test *BacktestCommandTest) TestEndOnTodaysWeekendHasNoNotice() {
	test.fixClock(time.Date(2024, 3, 30, 5, 0, 0, 0, time.UTC))
	test.createClosedThrough("yahoo", quarterEnd, "2024-03-28")

	_, err := test.downloadQuarter()
	test.Require().NoError(err)
	test.requireKept()
}

// storeSessions stores a yahoo bar for symbol on every session from first
// to last, for a period outside the 2024 quarter.
func (test *BacktestCommandTest) storeSessions(symbol string, first, last time.Time) {
	ctx := context.Background()
	_, err := test.db.Exec(ctx, `INSERT INTO asset (symbol, name) VALUES ($1, $1) ON CONFLICT DO NOTHING`, symbol)
	test.Require().NoError(err)
	for _, session := range sessionsBetween(first, last) {
		_, err = test.db.Exec(ctx, `
			INSERT INTO ohlc (symbol, open, high, low, close, volume, time, provider)
			VALUES ($1, 10, 11, 9, 10, 100, $2, 'yahoo')`, symbol, session.Add(14*time.Hour+30*time.Minute))
		test.Require().NoError(err)
	}
}

// A backtest file can end after today. Every session the cap leaves out is
// named, as a range from the first of them to the last session on or
// before the end date, never a weekend or holiday end date itself.
func (test *BacktestCommandTest) TestUnclosedSessionsNoticeNamesTheRange() {
	test.Run("through the end date's session", func() {
		test.SetupTest()
		// 14:00 New York on Wednesday 2026-10-28: Tuesday has closed, and
		// Wednesday to Friday 2026-10-30, the end date, haven't.
		test.fixClock(time.Date(2026, 10, 28, 18, 0, 0, 0, time.UTC))
		first := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
		test.createPeriod(first, time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC), "AAPL")
		test.storeSessions("OEF", first, time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC))
		test.storeSessions("AAPL", first, time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC))

		_, err := test.download(time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC))
		test.requireNotice(err, "2026-10-28 to 2026-10-30 aren't included because their trading sessions "+
			"haven't closed yet (sessions count as closed at 16:30 New York time).")
	})
	test.Run("end on a weekend past the first read", func() {
		test.SetupTest()
		test.fixClock(beforeTheClose)
		// Saturday 2024-04-27 is more than recentSessionDays after
		// 2024-03-28, and the last session on or before it is Friday.
		end := time.Date(2024, 4, 27, 0, 0, 0, 0, time.UTC)
		test.createClosedThrough("yahoo", end, "2024-03-27")

		_, err := test.download(end)
		test.requireNotice(err, "2024-03-28 to 2024-04-26 aren't included because their trading sessions "+
			"haven't closed yet (sessions count as closed at 16:30 New York time).")
	})
	test.Run("end on a session past the first read", func() {
		test.SetupTest()
		test.fixClock(beforeTheClose)
		end := time.Date(2024, 12, 31, 0, 0, 0, 0, time.UTC)
		test.createClosedThrough("yahoo", end, "2024-03-27")

		_, err := test.download(end)
		test.requireNotice(err, "2024-03-28 to 2024-12-31 aren't included because their trading sessions "+
			"haven't closed yet (sessions count as closed at 16:30 New York time).")
	})
}

// calendarEndEngine answers like a Zipline pod whose calendar ends on
// 2027-11-02: XNYS weekdays up to there, and a CalendarEndError for any read
// with a start or an end past it, the two ways the calendar refuses one.
func calendarEndEngine() *engine.MockEngine {
	lastSession := time.Date(2027, 11, 2, 0, 0, 0, 0, time.UTC)
	ziplineEngine := new(engine.MockEngine)
	ziplineEngine.On("Sessions", mock.Anything, mock.Anything, mock.Anything).Return(
		func(_ context.Context, start, end time.Time) ([]time.Time, error) {
			if start.After(lastSession) || end.After(lastSession) {
				return nil, &engine.CalendarEndError{Calendar: "XNYS", LastSession: lastSession}
			}
			return sessionsBetween(start, end), nil
		})
	return ziplineEngine
}

// A backtest file that ends after the calendar's last trading day fails
// create with a plain sentence naming both dates, before any provider is
// asked, and nothing is saved. Both reads past the calendar count: one whose
// start is past it (an end more than recentSessionDays past) and one whose
// end alone is.
func (test *BacktestCommandTest) TestEndPastTheCalendarFailsWithAPlainSentence() {
	for name, end := range map[string]time.Time{
		"start past the calendar": time.Date(2027, 12, 17, 0, 0, 0, 0, time.UTC),
		"end past the calendar":   time.Date(2027, 11, 10, 0, 0, 0, 0, time.UTC),
	} {
		test.Run(name, func() {
			test.SetupTest()
			test.fixClock(time.Date(2026, 11, 2, 18, 0, 0, 0, time.UTC))
			first := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
			test.createPeriod(first, end, "AAPL")
			test.storeSessions("OEF", first, time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC))
			test.storeSessions("AAPL", first, time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC))
			msg := test.downloadMessage(quarterBacktest)
			useEngine(msg, calendarEndEngine())

			err := command.DownloadFinancialData(context.TODO(), msg)
			test.Require().EqualError(err, "Couldn't load data for backtest 'oef-2024q1': it ends on "+
				end.Format(time.DateOnly)+", after the last trading day in foreverbull's market calendar "+
				"(2027-11-02). The backtest wasn't saved. Choose an end on or before 2027-11-02, then run "+
				"`fbull backtest create` again.")
			test.NotContains(err.Error(), "Details")
			test.NotContains(err.Error(), "data service")
			test.mdclient.AssertNotCalled(test.T(), "DownloadHistoricalData", mock.Anything, mock.Anything)
			test.requireNothingSaved()
		})
	}
}

// A period wholly after the calendar's last trading day has no closed
// session, and the read for one still to close goes past the calendar: it
// fails with the same plain sentence, before any provider is asked.
func (test *BacktestCommandTest) TestPeriodPastTheCalendarFailsWithAPlainSentence() {
	test.fixClock(time.Date(2026, 11, 2, 18, 0, 0, 0, time.UTC))
	// The calendar's last session is 2027-11-02.
	start := time.Date(2027, 12, 2, 0, 0, 0, 0, time.UTC)
	end := time.Date(2027, 12, 12, 0, 0, 0, 0, time.UTC)
	test.createPeriod(start, end, "AAPL")
	msg := test.downloadMessage(quarterBacktest)
	useEngine(msg, calendarEndEngine())

	err := command.DownloadFinancialData(context.TODO(), msg)
	test.Require().EqualError(err, "Couldn't load data for backtest 'oef-2024q1': it ends on 2027-12-12, after "+
		"the last trading day in foreverbull's market calendar (2027-11-02). The backtest wasn't saved. Choose an "+
		"end on or before 2027-11-02, then run `fbull backtest create` again.")
	test.NotContains(err.Error(), "Details")
	test.NotContains(err.Error(), "data service")
	test.mdclient.AssertNotCalled(test.T(), "DownloadHistoricalData", mock.Anything, mock.Anything)
	test.requireNothingSaved()
}
