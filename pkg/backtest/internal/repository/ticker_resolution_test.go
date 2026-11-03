package repository_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/lhjnilsson/foreverbull/internal/auth"
	"github.com/lhjnilsson/foreverbull/pkg/backtest/internal/repository"
	"github.com/lhjnilsson/foreverbull/pkg/pb/backtest"
)

func resolutionLines(resolutions []*backtest.Backtest_TickerResolution) []string {
	lines := make([]string, 0, len(resolutions))
	for _, resolution := range resolutions {
		lines = append(lines, resolution.GetSymbol()+">"+resolution.GetReadAs())
	}
	return lines
}

// A stored resolution comes back on Get and List, ordered by symbol, and an
// empty save clears it. No day is stored. A backtest that was never stored is ErrNoRows, not a
// silent no-op.
func TestTickerResolutionsByName(t *testing.T) {
	t.Setenv("AUTH_ENABLED", "false")
	pool := newBacktestPool(t)
	ctx := context.Background()
	resetBacktests(t, pool)
	backtests := repository.Backtest{Conn: pool}
	_, err := backtests.Create(ctx, &backtest.Backtest{Name: "oef", Benchmark: "SPY", Provider: "yahoo"})
	require.NoError(t, err)

	stored, err := backtests.Get(ctx, "oef")
	require.NoError(t, err)
	require.Empty(t, stored.GetTickerResolutions())

	require.NoError(t, backtests.SaveTickerResolutions(ctx, "oef", []repository.TickerResolution{
		{Symbol: "TWTR", ReadAs: "X"},
		{Symbol: "FB", ReadAs: "META"},
	}))
	var raw string
	require.NoError(t, pool.QueryRow(ctx, `SELECT ticker_resolutions::text FROM backtest WHERE name = 'oef'`).Scan(&raw))
	require.JSONEq(t, `[{"symbol":"FB","read_as":"META"},{"symbol":"TWTR","read_as":"X"}]`, raw)
	stored, err = backtests.Get(ctx, "oef")
	require.NoError(t, err)
	want := []string{"FB>META", "TWTR>X"}
	require.Equal(t, want, resolutionLines(stored.GetTickerResolutions()))

	listed, err := backtests.List(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Equal(t, want, resolutionLines(listed[0].GetTickerResolutions()))

	require.NoError(t, backtests.SaveTickerResolutions(ctx, "oef", nil))
	stored, err = backtests.Get(ctx, "oef")
	require.NoError(t, err)
	require.Empty(t, stored.GetTickerResolutions())

	err = backtests.SaveTickerResolutions(ctx, "missing", []repository.TickerResolution{
		{Symbol: "FB", ReadAs: "META"},
	})
	require.ErrorIs(t, err, pgx.ErrNoRows)
}

// Each incomplete resolution is refused before the write, and nothing is
// stored.
func TestTickerResolutionsRefuseIncompleteOnes(t *testing.T) {
	t.Setenv("AUTH_ENABLED", "false")
	pool := newBacktestPool(t)
	ctx := context.Background()
	resetBacktests(t, pool)
	backtests := repository.Backtest{Conn: pool}
	_, err := backtests.Create(ctx, &backtest.Backtest{Name: "oef", Benchmark: "SPY", Provider: "yahoo"})
	require.NoError(t, err)

	cases := []struct {
		name        string
		resolutions []repository.TickerResolution
	}{
		{"no symbol", []repository.TickerResolution{{ReadAs: "META"}}},
		{"no later ticker", []repository.TickerResolution{{Symbol: "FB"}}},
		{"same ticker", []repository.TickerResolution{{Symbol: "FB", ReadAs: "FB"}}},
		{"padded symbol", []repository.TickerResolution{{Symbol: " FB", ReadAs: "META"}}},
		{"symbol twice", []repository.TickerResolution{
			{Symbol: "FB", ReadAs: "META"},
			{Symbol: "FB", ReadAs: "METB"},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := backtests.SaveTickerResolutions(ctx, "oef", c.resolutions)
			require.ErrorIs(t, err, repository.ErrInvalidTickerResolution)
			stored, err := backtests.Get(ctx, "oef")
			require.NoError(t, err)
			require.Empty(t, stored.GetTickerResolutions())
		})
	}
}

// A row whose stored list was edited into something create would refuse
// fails the read instead of showing a wrong line.
func TestTickerResolutionsFailTheReadOfABadRow(t *testing.T) {
	t.Setenv("AUTH_ENABLED", "false")
	pool := newBacktestPool(t)
	ctx := context.Background()
	resetBacktests(t, pool)
	backtests := repository.Backtest{Conn: pool}
	_, err := backtests.Create(ctx, &backtest.Backtest{Name: "oef", Benchmark: "SPY", Provider: "yahoo"})
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `UPDATE backtest SET ticker_resolutions = '[{"symbol":"FB","read_as":"FB"}]'`)
	require.NoError(t, err)
	_, err = backtests.Get(ctx, "oef")
	require.ErrorIs(t, err, repository.ErrInvalidTickerResolution)
	_, err = backtests.List(ctx)
	require.ErrorIs(t, err, repository.ErrInvalidTickerResolution)

	// One symbol listed twice fails the read, as it fails the write.
	_, err = pool.Exec(ctx, `UPDATE backtest SET ticker_resolutions = '[{"symbol":"FB","read_as":"META"},{"symbol":"FB","read_as":"METB"}]'`)
	require.NoError(t, err)
	_, err = backtests.Get(ctx, "oef")
	require.ErrorIs(t, err, repository.ErrInvalidTickerResolution)
	_, err = backtests.List(ctx)
	require.ErrorIs(t, err, repository.ErrInvalidTickerResolution)

	// A row stored out of order is read back ordered by symbol.
	_, err = pool.Exec(ctx, `UPDATE backtest SET ticker_resolutions = '[{"symbol":"TWTR","read_as":"X"},{"symbol":"FB","read_as":"META"}]'`)
	require.NoError(t, err)
	stored, err := backtests.Get(ctx, "oef")
	require.NoError(t, err)
	require.Equal(t, []string{"FB>META", "TWTR>X"}, resolutionLines(stored.GetTickerResolutions()))

	// A resolution names no day, so a stored one fails the read rather
	// than being dropped or shown.
	_, err = pool.Exec(ctx, `UPDATE backtest SET ticker_resolutions = '[{"symbol":"FB","read_as":"META","from_date":"2022-06-09"}]'`)
	require.NoError(t, err)
	_, err = backtests.Get(ctx, "oef")
	require.ErrorIs(t, err, repository.ErrInvalidTickerResolution)

	_, err = pool.Exec(ctx, `UPDATE backtest SET ticker_resolutions = '{}'`)
	require.Error(t, err, "the column holds an array")
}

// With AUTH_ENABLED the resolutions are saved and read by backtest id. The
// column is on the backtest row, so the move to the id primary key keeps a
// stored list and CreateTables still runs afterwards.
func TestTickerResolutionsByID(t *testing.T) {
	pool := newBacktestPool(t)
	ctx := context.Background()
	resetBacktests(t, pool)
	seedBacktests(t, pool)
	t.Setenv("AUTH_ENABLED", "false")
	named := repository.Backtest{Conn: pool}
	require.NoError(t, named.SaveTickerResolutions(ctx, "spy", []repository.TickerResolution{
		{Symbol: "FB", ReadAs: "META"},
	}))

	admin := insertBacktestAccount(t, pool, auth.RoleAdmin)
	copier := &memIngest{keys: map[string]bool{"spy": true, "qqq": true}}
	require.NoError(t, repository.MigrateBacktestOwner(ctx, pool, admin, copier))
	require.NoError(t, repository.CreateTables(ctx, pool))
	t.Setenv("AUTH_ENABLED", "true")

	owned := repository.Backtest{Conn: pool}
	spy, err := owned.IDByOwnerAndName(ctx, admin, "spy")
	require.NoError(t, err)
	stored, err := owned.GetByID(ctx, spy)
	require.NoError(t, err)
	require.Equal(t, []string{"FB>META"}, resolutionLines(stored.GetTickerResolutions()))

	qqq, err := owned.IDByOwnerAndName(ctx, admin, "qqq")
	require.NoError(t, err)
	require.NoError(t, owned.SaveTickerResolutionsByID(ctx, qqq, []repository.TickerResolution{
		{Symbol: "TWTR", ReadAs: "X"},
	}))
	stored, err = owned.GetByID(ctx, qqq)
	require.NoError(t, err)
	require.Equal(t, []string{"TWTR>X"}, resolutionLines(stored.GetTickerResolutions()))

	require.Error(t, owned.SaveTickerResolutions(ctx, "qqq", nil), "by name is refused with AUTH_ENABLED")
	require.ErrorIs(t, owned.SaveTickerResolutionsByID(ctx, "00000000-0000-0000-0000-000000000000", nil), pgx.ErrNoRows)
}

// A backtest stored before the column existed gets an empty list on the next
// start, and running the migration twice is safe.
func TestMigrateTickerResolutionsKeepsExistingBacktests(t *testing.T) {
	t.Setenv("AUTH_ENABLED", "false")
	pool := newBacktestPool(t)
	ctx := context.Background()
	resetBacktests(t, pool)
	seedBacktests(t, pool)
	_, err := pool.Exec(ctx, `ALTER TABLE backtest DROP COLUMN ticker_resolutions`)
	require.NoError(t, err)

	require.NoError(t, repository.MigrateTickerResolutions(ctx, pool))
	require.NoError(t, repository.MigrateTickerResolutions(ctx, pool))
	backtests := repository.Backtest{Conn: pool}
	stored, err := backtests.Get(ctx, "spy")
	require.NoError(t, err)
	require.Empty(t, stored.GetTickerResolutions())
}
