package command

import (
	"context"
	"testing"
	"time"

	"github.com/lhjnilsson/foreverbull/pkg/backtest/engine"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// A period that starts long after the last closed session is not read
// from the calendar, which ends about a year ahead.
func TestHasUnclosedSessionPastCalendarEnd(t *testing.T) {
	last := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	eng := engine.NewMockEngine(t)
	notClosed, err := hasUnclosedSession(context.Background(), eng,
		time.Date(2028, 1, 3, 0, 0, 0, 0, time.UTC), time.Date(2028, 1, 31, 0, 0, 0, 0, time.UTC), last)
	require.NoError(t, err)
	require.True(t, notClosed)
}

// A period that starts within two weeks of the last closed session reads
// at most recentSessionDays from its start.
func TestHasUnclosedSessionReadsTwoWeeks(t *testing.T) {
	last := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	start := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	eng := engine.NewMockEngine(t)
	eng.On("Sessions", mock.Anything, start, start.AddDate(0, 0, recentSessionDays)).
		Return([]time.Time{time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)}, nil)
	notClosed, err := hasUnclosedSession(context.Background(), eng, start, start.AddDate(1, 0, 0), last)
	require.NoError(t, err)
	require.True(t, notClosed)
}
