package marketdata

import (
	"fmt"
	"time"

	// The broker image may have no zoneinfo; embed America/New_York.
	_ "time/tzdata"

	"github.com/rs/zerolog/log"
)

// xnysZone is the exchange's time zone. LoadLocation cannot fail with the
// embedded zone database.
var xnysZone = mustLoadLocation("America/New_York")

// sessionFinalAt is when, in New York wall time, a session counts as
// completed: 30 minutes after the regular 16:00 close, so the closing
// auction has printed and providers have revised the day's bar into its
// final daily bar.
const sessionFinalAt = 16*time.Hour + 30*time.Minute

func mustLoadLocation(name string) *time.Location {
	location, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return location
}

// clockNow is now, or the time a test fixed.
func clockNow(now func() time.Time) time.Time {
	if now != nil {
		return now()
	}
	return time.Now()
}

// lastCompletedSessionEnd clamps a fetch end so that today's unfinished
// XNYS session is never asked for. A session counts as completed at
// 16:30 America/New_York (sessionFinalAt), not at the 16:00 close. Before
// that, the fetch ends at midnight New York time, so the last bar is the
// previous session. Two cases need no calendar:
//   - A holiday or weekend has no session, so the provider sends no bar.
//   - On an early-close day (13:00), the session counts as completed only
//     at 16:30. That leaves a finished session out for three and a half
//     hours. It never lets an unfinished one in.
//
// Every provider clamps with it: Yahoo, Alpaca and Stooq. A provider whose
// end is inclusive, or counted in days, stops just before the limit when it
// applies, so a bar stamped at midnight New York today is left out too.
func lastCompletedSessionEnd(end, now time.Time) time.Time {
	local := now.In(xnysZone)
	year, month, day := local.Date()
	finalAt := time.Date(year, month, day, int(sessionFinalAt/time.Hour),
		int(sessionFinalAt%time.Hour/time.Minute), 0, 0, xnysZone)
	limit := now
	if local.Before(finalAt) {
		limit = time.Date(year, month, day, 0, 0, 0, 0, xnysZone)
	}
	if end.After(limit) {
		return limit
	}
	return end
}

// completedFetchEnd is the clamped end of a fetch from start. It is false
// when nothing completed is left to fetch, because the clamped end is not
// after start, such as a fetch for today alone before the close. The
// provider is then not asked, and the fetch has no bars rather than an
// error.
func completedFetchEnd(start, end, now time.Time) (time.Time, bool) {
	until := lastCompletedSessionEnd(end, now)
	return until, until.After(start)
}

// inclusiveFetchEnd is the exclusive end of a fetch through end's date:
// midnight New York after that date. The end date is inclusive for every
// provider. Yahoo's period2 is exclusive and stamps a daily bar at the
// 09:30 open, and Alpaca stamps a daily bar at midnight New York (04:00 or
// 05:00 UTC), so an end at 00:00 UTC on the date left that day's bar out of
// both. Stooq's d2 is an inclusive date and does not need it. The date is
// end's UTC date, as Stooq reads it, since a backtest's end is a date at
// midnight UTC. lastCompletedSessionEnd clamps the result afterwards.
func inclusiveFetchEnd(end time.Time) time.Time {
	year, month, day := end.UTC().Date()
	return time.Date(year, month, day+1, 0, 0, 0, 0, xnysZone)
}

// lastFetchedSession is a time on the New York date of the last session a
// fetch through end may return: end's date, or the last completed session
// when that is earlier. A nil end is open, through the last completed
// session. A bar after it is dropped with afterLastSession.
func lastFetchedSession(end *time.Time, now time.Time) time.Time {
	last := lastCompletedSession(now)
	if end == nil {
		return last
	}
	year, month, day := end.UTC().Date()
	endDay := time.Date(year, month, day, 0, 0, 0, 0, xnysZone)
	if endDay.Before(last) {
		return endDay
	}
	return last
}

// lastCompletedSession is a time on the New York date of the last
// completed XNYS session as of now: today from 16:30 New York
// (sessionFinalAt), yesterday before. A weekend or holiday date has no
// session, so no bar is on it.
func lastCompletedSession(now time.Time) time.Time {
	return lastCompletedSessionEnd(now, now).Add(-time.Nanosecond).In(xnysZone)
}

// LastCompletedSessionDate is the New York date of the last completed XNYS
// session as of now, as midnight UTC on that date, the way a backtest date
// is read: today from 16:30 New York (sessionFinalAt), yesterday before. It
// can be a weekend or holiday date, which has no session.
func LastCompletedSessionDate(now time.Time) time.Time {
	year, month, day := lastCompletedSession(now).Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

// NoClosedSessionReason says that no session from start to end, both
// dates, has closed yet, and when a session counts as closed, read from
// sessionFinalAt: "no trading session between 2026-10-31 and 2026-10-31
// has closed yet (sessions count as closed at 16:30 New York time)". Every
// message that says nothing has closed yet uses it, so the time it names is
// the one the fetches stop at.
func NoClosedSessionReason(start, end time.Time) string {
	return fmt.Sprintf("no trading session between %s and %s has closed yet "+
		"(sessions count as closed at %d:%02d New York time)",
		start.UTC().Format(time.DateOnly), end.UTC().Format(time.DateOnly),
		int(sessionFinalAt/time.Hour), int(sessionFinalAt%time.Hour/time.Minute))
}

// afterLastSession is true for a bar whose session date comes after last,
// the date of the last session the fetch may return, and logs the drop at
// debug level. Each date is read in its own location: Stooq's rows are
// dates at midnight UTC, Yahoo's and Alpaca's bars are read in New York.
// Asking a provider for completed sessions only is not enough: Stooq can
// send rows past d2, Yahoo's chart API sometimes sends today's live bar
// when period2 is earlier, and Alpaca could too. Every provider drops such
// a bar with it rather than store an unfinished session as final. last is
// also never after the fetch's end date (lastFetchedSession), so a bar past
// the end asked for is not stored either.
func afterLastSession(provider, symbol string, session, last time.Time) bool {
	sessionYear, sessionMonth, sessionDay := session.Date()
	lastYear, lastMonth, lastDay := last.Date()
	sessionDate := time.Date(sessionYear, sessionMonth, sessionDay, 0, 0, 0, 0, time.UTC)
	lastDate := time.Date(lastYear, lastMonth, lastDay, 0, 0, 0, 0, time.UTC)
	if !sessionDate.After(lastDate) {
		return false
	}
	log.Debug().
		Str("provider", provider).
		Str("symbol", symbol).
		Str("session", sessionDate.Format(time.DateOnly)).
		Str("last_session", lastDate.Format(time.DateOnly)).
		Msg("dropped a bar after the last session of the fetch")
	return true
}
