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

	err = sessionRunError("spy", errors.New("no ingestions found"))
	require.EqualError(t, err, "error publishing session run command: no ingestions found")
}
