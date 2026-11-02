package stream

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// An unreachable service rides on a command's state as its plain cause and
// comes back from the publisher as ErrServicesUnreachable.
func TestUnreachableStateRoundTrip(t *testing.T) {
	cause := Unreachable(fmt.Errorf("error storing session port: %w", errors.New("connection lost")))
	require.EqualError(t, cause, "error storing session port: connection lost")
	require.ErrorIs(t, cause, ErrServicesUnreachable)

	state := stateError(cause)
	require.EqualError(t, state, "foreverbull's services couldn't reach each other")
	require.ErrorIs(t, commandError(state.Error()), ErrServicesUnreachable)

	other := errors.New("error getting session: no rows")
	require.Equal(t, other, stateError(other))
	require.EqualError(t, commandError(other.Error()), "error getting session: no rows")
}
