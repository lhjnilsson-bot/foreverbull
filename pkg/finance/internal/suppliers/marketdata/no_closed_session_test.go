package marketdata_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lhjnilsson/foreverbull/pkg/finance/internal/suppliers/marketdata"
)

// The reason names the dates asked for and the time a session counts as
// closed, the 16:30 New York time every fetch stops at.
func TestNoClosedSessionReason(t *testing.T) {
	t.Parallel()
	day := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	require.Equal(t, "no trading session between 2026-10-31 and 2026-10-31 has closed yet "+
		"(sessions count as closed at 16:30 New York time)", marketdata.NoClosedSessionReason(day, day))
	require.Equal(t, "no trading session between 2026-10-30 and 2026-11-02 has closed yet "+
		"(sessions count as closed at 16:30 New York time)",
		marketdata.NoClosedSessionReason(day.AddDate(0, 0, -1), day.AddDate(0, 0, 2)))
}

// The notice names the left-out session in the agreed words, and a run of
// sessions as a range, with the same closing time as the reason above.
func TestUnclosedSessionsNotice(t *testing.T) {
	t.Parallel()
	friday := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	require.Equal(t, "2026-10-30 isn't included because its trading session hasn't closed yet "+
		"(sessions count as closed at 16:30 New York time).", marketdata.UnclosedSessionsNotice(friday, friday))
	require.Equal(t, "2026-10-28 to 2026-10-30 aren't included because their trading sessions haven't "+
		"closed yet (sessions count as closed at 16:30 New York time).",
		marketdata.UnclosedSessionsNotice(friday.AddDate(0, 0, -2), friday))
}
