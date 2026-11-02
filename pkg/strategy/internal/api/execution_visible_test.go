package api

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/lhjnilsson/foreverbull/internal/auth"
	"github.com/lhjnilsson/foreverbull/internal/test_helper"
	"github.com/stretchr/testify/require"
)

// A failed read of the execution's backtest is an error. Only a missing row
// means the caller can't see it.
func TestExecutionBacktestVisible(t *testing.T) {
	ctx := context.Background()
	account := auth.Account{ID: "acc-1", Role: auth.RoleEditor}

	visible, err := executionBacktestVisible(ctx, test_helper.FailingRowsConn{Err: pgx.ErrNoRows}, account, "exec-1")
	require.NoError(t, err)
	require.False(t, visible)

	broken := errors.New("connection lost")
	visible, err = executionBacktestVisible(ctx, test_helper.FailingRowsConn{Err: broken}, account, "exec-1")
	require.ErrorIs(t, err, broken)
	require.EqualError(t, err, "read backtest of execution exec-1: connection lost")
	require.False(t, visible)
}
