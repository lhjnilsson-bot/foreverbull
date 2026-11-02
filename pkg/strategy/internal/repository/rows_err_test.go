package repository_test

import (
	"context"
	"errors"
	"testing"

	"github.com/lhjnilsson/foreverbull/internal/test_helper"
	"github.com/lhjnilsson/foreverbull/pkg/strategy/internal/repository"
	"github.com/stretchr/testify/require"
)

// A result that breaks off mid-stream is an error, never a shorter list.
func TestListFailsWhenRowsBreak(t *testing.T) {
	ctx := context.Background()
	broken := errors.New("connection lost")
	conn := test_helper.FailingRowsConn{Err: broken}

	cases := []struct {
		name string
		list func() error
		want string
	}{
		{"strategies", func() error { _, err := (&repository.Strategy{Conn: conn}).List(ctx); return err },
			"failed to read strategies: connection lost"},
		{"versions", func() error { _, err := (&repository.Version{Conn: conn}).ListByStrategy(ctx, "s"); return err },
			"failed to read strategy versions: connection lost"},
		{"experiments", func() error { _, err := (&repository.Experiment{Conn: conn}).List(ctx, "", "", ""); return err },
			"failed to read strategy experiments: connection lost"},
		{"deployments", func() error { _, err := (&repository.Deployment{Conn: conn}).ListRunning(ctx); return err },
			"failed to read strategy deployments: connection lost"},
		{"deployment runs", func() error { _, err := (&repository.DeploymentRun{Conn: conn}).List(ctx, "s", ""); return err },
			"failed to read strategy deployment runs: connection lost"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.list()
			require.ErrorIs(t, err, broken)
			require.EqualError(t, err, c.want)
		})
	}
}
