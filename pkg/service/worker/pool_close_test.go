package worker

import (
	"errors"
	"testing"

	"github.com/lhjnilsson/foreverbull/internal/socket"
	"github.com/stretchr/testify/require"
)

// A socket that fails to close doesn't keep the other one open, and both
// failures are returned.
func TestPoolCloseClosesBothSockets(t *testing.T) {
	requester := new(socket.MockRequester)
	requester.On("Close").Return(errors.New("requester busy"))
	replier := new(socket.MockReplier)
	replier.On("Close").Return(errors.New("replier busy"))
	p := &pool{Socket: requester, NamespaceSocket: replier}

	err := p.Close()
	require.EqualError(t, err, "error closing socket: requester busy\nerror closing namespace socket: replier busy")
	replier.AssertCalled(t, "Close")
}
