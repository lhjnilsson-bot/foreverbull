package repository_test

import (
	"context"
	"errors"
	"testing"

	"github.com/lhjnilsson/foreverbull/internal/test_helper"
	"github.com/lhjnilsson/foreverbull/pkg/finance/internal/repository"
	commonpb "github.com/lhjnilsson/foreverbull/pkg/pb"
	"github.com/stretchr/testify/require"
)

// A result that breaks off mid-stream is an error, never a shorter list. A
// short calendar would make a missing session look like a holiday.
func TestListFailsWhenRowsBreak(t *testing.T) {
	ctx := context.Background()
	broken := errors.New("connection lost")
	conn := test_helper.FailingRowsConn{Err: broken}
	day := &commonpb.Date{Year: 2024, Month: 1, Day: 2}

	cases := []struct {
		name string
		list func() error
		want string
	}{
		{"assets", func() error { _, err := (&repository.Asset{Conn: conn}).List(ctx); return err },
			"failed to read assets: connection lost"},
		{"assets by symbol", func() error {
			_, err := (&repository.Asset{Conn: conn}).ListBySymbols(ctx, []string{"AAPL"})
			return err
		}, "failed to read assets: connection lost"},
		{"calendar dates", func() error {
			_, err := (&repository.Calendar{Conn: conn}).ListCalendarDates(ctx, "XNYS", day, day)
			return err
		}, "failed to read calendar dates: connection lost"},
		{"trading sessions", func() error { _, err := (&repository.Trading{Conn: conn}).List(ctx); return err },
			"failed to read trading sessions: connection lost"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.list()
			require.ErrorIs(t, err, broken)
			require.EqualError(t, err, c.want)
		})
	}
}
