package api

import (
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
