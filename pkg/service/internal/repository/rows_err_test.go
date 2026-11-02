package repository_test

import (
	"context"
	"errors"
	"testing"

	"github.com/lhjnilsson/foreverbull/internal/test_helper"
	"github.com/lhjnilsson/foreverbull/pkg/service/internal/repository"
	"github.com/stretchr/testify/require"
)

// A result that breaks off mid-stream is an error, never a shorter list
// and never "not found".
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
		{"instance", func() error { _, err := (&repository.Instance{Conn: conn}).Get(ctx, "i"); return err },
			"failed to read instance: connection lost"},
		{"instances", func() error { _, err := (&repository.Instance{Conn: conn}).List(ctx); return err },
			"failed to read instances: connection lost"},
		{"service", func() error { _, err := (&repository.Service{Conn: conn}).Get(ctx, "img"); return err },
			"failed to read service: connection lost"},
		{"services", func() error { _, err := (&repository.Service{Conn: conn}).List(ctx); return err },
			"failed to read services: connection lost"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.read()
			require.ErrorIs(t, err, broken)
			require.EqualError(t, err, c.want)
		})
	}
}
