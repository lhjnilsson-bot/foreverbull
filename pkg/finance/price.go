package finance

import (
	"time"

	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/pkg/finance/internal/suppliers/marketdata"
)

// ValidatePriceSource checks a backtest price source before download.
// yahoo and stooq only check the name. alpaca resolves a profile from the
// profiles file and fails when that profile cannot be chosen.
// It does not construct a Yahoo client and does not download bars.
func ValidatePriceSource(provider, profile string) error {
	return marketdata.ValidatePriceSource(provider, profile, environment.GetTradingProfilesFile())
}

// StoredSymbol is the one name a listed share class is stored under, however
// a universe file spells it: BRK.B, BRK-B, BRK/B and BRKB all store as BRKB.
// Every other symbol, including an exchange suffix such as VOD.L, is
// returned exactly as typed.
func StoredSymbol(symbol string) string {
	return marketdata.StoredSymbol(symbol)
}

// LastCompletedSessionDate is the date of the last completed XNYS session as
// of now, at midnight UTC: today from 16:30 America/New_York, yesterday
// before. Every provider's fetch stops there, so a check of stored bars
// stops there too.
func LastCompletedSessionDate(now time.Time) time.Time {
	return marketdata.LastCompletedSessionDate(now)
}

// NoClosedSessionReason says that no session from start to end has closed
// yet, naming the New York time a session counts as closed at, the same
// time every fetch stops at: "no trading session between 2026-10-31 and
// 2026-10-31 has closed yet (sessions count as closed at 16:30 New York
// time)". Use it wherever that is said, rather than writing it again.
func NoClosedSessionReason(start, end time.Time) string {
	return marketdata.NoClosedSessionReason(start, end)
}

// UnclosedSessionsNotice says that the sessions from first to last were
// left out of a backtest because they haven't closed yet, with the same
// closing time as NoClosedSessionReason: "2026-10-30 isn't included because
// its trading session hasn't closed yet (sessions count as closed at 16:30
// New York time)."
func UnclosedSessionsNotice(first, last time.Time) string {
	return marketdata.UnclosedSessionsNotice(first, last)
}
