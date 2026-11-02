package command

import (
	"context"
	"errors"
	"testing"

	"github.com/lhjnilsson/foreverbull/internal/stream"
	"github.com/lhjnilsson/foreverbull/internal/test_helper"
	"github.com/lhjnilsson/foreverbull/pkg/backtest/internal/repository"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// A session whose port can't be stored fails the command: no client could
// reach its server.
func TestPublishSessionPortFails(t *testing.T) {
	sessions := &repository.Session{Conn: test_helper.FailingRowsConn{Err: errors.New("connection lost")}}
	err := publishSessionPort(context.Background(), sessions, grpc.NewServer(), "s-1", 27000)
	require.EqualError(t, err, "error storing session port: failed to update session port: connection lost")
	require.ErrorIs(t, err, stream.ErrServicesUnreachable)
}
