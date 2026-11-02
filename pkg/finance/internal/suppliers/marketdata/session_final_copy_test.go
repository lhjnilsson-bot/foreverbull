package marketdata

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Both sentences read the closing time from the duration they are given,
// never from a literal, so a change to sessionFinalAt changes both.
func TestUnclosedCopyFollowsSessionFinalAt(t *testing.T) {
	t.Parallel()
	day := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		finalAt time.Duration
		clause  string
	}{
		{sessionFinalAt, "(sessions count as closed at 16:30 New York time)"},
		{17*time.Hour + 5*time.Minute, "(sessions count as closed at 17:05 New York time)"},
		{16 * time.Hour, "(sessions count as closed at 16:00 New York time)"},
	} {
		require.Equal(t, "no trading session between 2026-10-30 and 2026-10-30 has closed yet "+tc.clause,
			noClosedSessionReason(day, day, tc.finalAt))
		require.Equal(t, "2026-10-30 isn't included because its trading session hasn't closed yet "+tc.clause+".",
			unclosedSessionsNotice(day, day, tc.finalAt))
	}
}
