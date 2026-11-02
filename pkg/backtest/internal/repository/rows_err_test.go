package repository_test

import (
	"context"
	"errors"
	"testing"

	"github.com/lhjnilsson/foreverbull/internal/test_helper"
	"github.com/lhjnilsson/foreverbull/pkg/backtest/internal/repository"
	"github.com/stretchr/testify/require"
)

// A result that breaks off mid-stream is an error, never a shorter list,
// fewer periods, or "not found".
func TestReadFailsWhenRowsBreak(t *testing.T) {
	t.Setenv("AUTH_ENABLED", "false")
	ctx := context.Background()
	broken := errors.New("connection lost")
	conn := test_helper.FailingRowsConn{Err: broken}

	cases := []struct {
		name string
		read func() error
		want string
	}{
		{"backtest", func() error { _, err := (&repository.Backtest{Conn: conn}).Get(ctx, "b"); return err },
			"failed to read backtest: connection lost"},
		{"backtests", func() error { _, err := (&repository.Backtest{Conn: conn}).List(ctx); return err },
			"failed to read backtests: connection lost"},
		{"sessions", func() error { _, err := (&repository.Session{Conn: conn}).List(ctx); return err },
			"failed to read sessions: connection lost"},
		{"execution periods", func() error { _, err := (&repository.Execution{Conn: conn}).GetPeriods(ctx, "e"); return err },
			"failed to read execution periods: connection lost"},
		{"executions", func() error { _, err := (&repository.Execution{Conn: conn}).List(ctx); return err },
			"failed to read executions: connection lost"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.read()
			require.ErrorIs(t, err, broken)
			require.EqualError(t, err, c.want)
		})
	}
}
