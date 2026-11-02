package api

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/internal/test_helper"
	"github.com/lhjnilsson/foreverbull/pkg/pb/backtest"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Under auth an execution that isn't there is NotFound. A read that fails
// is Internal with the sentence, never NotFound, with auth on or off.
func TestExecutionForListenerTellsMissingFromFailed(t *testing.T) {
	t.Setenv(environment.AuthEnabled, "true")
	session := &backtest.Session{Id: "s-1", Backtest: "spy"}
	missing := &grpcSessionServer{db: test_helper.FailingRowsConn{Err: pgx.ErrNoRows}, session: session}
	_, err := missing.executionForListener(context.Background(), "e-1")
	require.Equal(t, codes.NotFound, status.Code(err))

	broken := &grpcSessionServer{db: test_helper.FailingRowsConn{Err: errors.New("connection lost")}, session: session}
	_, err = broken.executionForListener(context.Background(), "e-1")
	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.Internal, st.Code())
	require.Equal(t, "Couldn't read execution e-1. Run `fbenv status` to check that the environment is running, "+
		"then run `fbull backtest run spy` again.", st.Message())

	t.Setenv(environment.AuthEnabled, "false")
	_, err = broken.executionForListener(context.Background(), "e-1")
	require.Equal(t, codes.Internal, status.Code(err))
}
