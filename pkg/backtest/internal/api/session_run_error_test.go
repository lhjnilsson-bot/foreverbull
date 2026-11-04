package api

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/lhjnilsson/foreverbull/internal/stream"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A session run whose services couldn't reach each other is named for the
// user; any other failure stays internal.
func TestSessionRunError(t *testing.T) {
	err := sessionRunError("spy", fmt.Errorf("wait: %w", stream.ErrServicesUnreachable))
	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.Unavailable, st.Code())
	require.Equal(t, "Couldn't run backtest 'spy': foreverbull's services couldn't reach each other. "+
		"Run `fbenv status` to check that the environment is running, then run `fbull backtest run spy` again.", st.Message())

	err = sessionRunError("spy", fmt.Errorf("wait: %w", stream.ErrStateNotRecorded))
	st, ok = status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.ResourceExhausted, st.Code())
	require.Equal(t, "Couldn't start the session for backtest 'spy': foreverbull's task queue is full, so it couldn't record the task's progress. "+
		"Old tasks leave the queue after 24 hours, so run `fbull backtest run spy` again once they have. "+
		"Restarting fbenv won't clear the queue.", st.Message())

	err = sessionRunError("spy", errors.New("no ingestions found"))
	require.EqualError(t, err, "error publishing session run command: no ingestions found")
}

// An engine error while reading the result reaches fbull only as the fixed
// sentence, never as the engine's own text.
func TestResultUnreadableErrorHidesTheEngineText(t *testing.T) {
	err := resultUnreadableError("e-1", status.Error(codes.Unknown,
		"error getting result: Exception calling application: a reported metric has no value"))
	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.FailedPrecondition, st.Code())
	require.Equal(t, "The backtest finished, but foreverbull couldn't read its result. "+
		"This is a problem in foreverbull, not in your algo. "+
		"Please report it at https://github.com/lhjnilsson/foreverbull/issues with this message.", st.Message())
	require.NotContains(t, st.Message(), "metric")
}

// A stop Zipline names is passed on as written, so a dividend the run didn't
// pay doesn't tell the user to try again; any other failure is "stopped early".
func TestRunFailureErrorPassesZiplinesSentenceOn(t *testing.T) {
	unpaid := "The backtest run stopped: foreverbull didn't pay a dividend on its ex-date. " +
		"This is a problem in foreverbull, not in your algo. " +
		"Please report it at https://github.com/lhjnilsson/foreverbull/issues with this message."
	err := runFailureError(fmt.Errorf("get current period: %w", status.Error(codes.FailedPrecondition, unpaid)))
	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.FailedPrecondition, st.Code())
	require.Equal(t, unpaid, st.Message())

	for _, crash := range []error{
		status.Error(codes.Unknown, "Exception calling application: dividends of 48.500000 were not paid"),
		status.Error(codes.Unavailable, "connection refused"),
		errors.New("error processing orders"),
	} {
		st, ok := status.FromError(runFailureError(crash))
		require.True(t, ok)
		require.Equal(t, codes.FailedPrecondition, st.Code())
		require.Equal(t, "The backtest run stopped early. Try again, or run `fbenv status`.", st.Message())
	}
}

// Only the engine's own failure to read the result is the report sentence. A
// lost connection, a cancel or a deadline is returned as before.
func TestGetResultErrorTellsTheEngineFromTheConnection(t *testing.T) {
	engine := fmt.Errorf("error getting result: %w",
		status.Error(codes.Internal, "engine result error: a reported metric has no value"))
	st, ok := status.FromError(getResultError("e-1", engine))
	require.True(t, ok)
	require.Equal(t, codes.FailedPrecondition, st.Code())
	require.Equal(t, resultUnreadable, st.Message())

	for _, kept := range []error{
		status.Error(codes.Unavailable, "connection reset by peer"),
		status.Error(codes.Canceled, "context canceled"),
		status.Error(codes.DeadlineExceeded, "context deadline exceeded"),
		status.Error(codes.Internal, "stream terminated by RST_STREAM with error code: INTERNAL_ERROR"),
		context.Canceled,
		context.DeadlineExceeded,
	} {
		err := getResultError("e-1", kept)
		require.EqualError(t, err, "error getting result: "+kept.Error())
		require.ErrorIs(t, err, kept)
		require.Equal(t, status.Code(kept), status.Code(err))
	}
}
