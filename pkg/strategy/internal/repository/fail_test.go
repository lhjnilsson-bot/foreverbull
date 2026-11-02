package repository_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lhjnilsson/foreverbull/internal/test_helper"
	"github.com/lhjnilsson/foreverbull/pkg/strategy/internal/repository"
	"github.com/stretchr/testify/require"
)

// A failure whose status can't be stored names the cause and the write
// that failed; neither is dropped, and it reads as StatusNotStored. A
// stored failure is the cause itself.
func TestFailNamesCauseAndStatusWrite(t *testing.T) {
	ctx := context.Background()
	cause := errors.New("zipline exploded")
	conn := test_helper.FailingRowsConn{Err: errors.New("connection lost")}

	err := (&repository.Experiment{Conn: conn}).Fail(ctx, "exp-1", cause)
	require.ErrorIs(t, err, cause)
	require.True(t, repository.StatusNotStored(err))
	require.EqualError(t, err,
		"zipline exploded; experiment exp-1 couldn't be marked failed: failed to update experiment status: connection lost")

	err = (&repository.Deployment{Conn: conn}).Fail(ctx, "dep-1", cause)
	require.ErrorIs(t, err, cause)
	require.True(t, repository.StatusNotStored(err))
	require.EqualError(t, err,
		"zipline exploded; deployment dep-1 couldn't be marked as an error: failed to update deployment status: connection lost")
}

// ctxConn records the context Exec ran on: whether it was already done
// and its deadline.
type ctxConn struct {
	test_helper.FailingRowsConn
	execErr     error
	deadline    time.Time
	hasDeadline bool
}

func (c *ctxConn) Exec(ctx context.Context, _ string, _ ...interface{}) (pgconn.CommandTag, error) {
	c.execErr = ctx.Err()
	c.deadline, c.hasDeadline = ctx.Deadline()
	return pgconn.CommandTag{}, c.execErr
}

// Complete writes COMPLETED on a context detached from the caller's and
// bounded by StatusWriteTimeout, so a client that went away after the run
// doesn't leave the row running.
func TestCompleteWritesOnDetachedContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	conn := &ctxConn{}

	require.NoError(t, (&repository.Experiment{Conn: conn}).Complete(ctx, "exp-1"))
	require.NoError(t, conn.execErr)
	require.True(t, conn.hasDeadline)
	require.WithinDuration(t, time.Now().Add(repository.StatusWriteTimeout), conn.deadline, time.Second)
}

// A failure that was stored returns its cause, which isn't StatusNotStored.
func TestFailStoredReturnsCause(t *testing.T) {
	cause := errors.New("zipline exploded")
	err := (&repository.Experiment{Conn: &ctxConn{}}).Fail(context.Background(), "exp-1", cause)
	require.Same(t, cause, err)
	require.False(t, repository.StatusNotStored(err))
}

// SetExecutionID writes on a detached, bounded context too.
func TestSetExecutionIDWritesOnDetachedContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	conn := &ctxConn{}

	require.NoError(t, (&repository.Experiment{Conn: conn}).SetExecutionID(ctx, "exp-1", "exe-1"))
	require.NoError(t, conn.execErr)
	require.True(t, conn.hasDeadline)
	require.WithinDuration(t, time.Now().Add(repository.StatusWriteTimeout), conn.deadline, time.Second)
}
