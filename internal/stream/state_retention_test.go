package stream

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/internal/stream/pb"
	"github.com/lhjnilsson/foreverbull/internal/test_helper"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestStateRetentionMatchesTheStream(t *testing.T) {
	require.Equal(t, MaxAge, StateRetention,
		"a message_states row is kept as long as the stream keeps its message")
	require.Equal(t, time.Hour, statePruneInterval)
	require.Equal(t, 1_000_000, statePruneBatch*statePruneBatches)
}

// prunerDB is a fresh message_states table with a pruner whose clock is now.
func prunerDB(t *testing.T, now time.Time) (*pgxpool.Pool, *statePruner) {
	t.Helper()
	test_helper.SetupEnvironment(t, &test_helper.Containers{Postgres: true})
	db, err := pgxpool.New(context.Background(), environment.GetPostgresURL())
	require.NoError(t, err)
	t.Cleanup(db.Close)
	require.NoError(t, RecreateTables(context.Background(), db))
	pruner := newStatePruner(NewRepository(db))
	pruner.now = func() time.Time { return now }
	return db, pruner
}

func addStates(t *testing.T, db *pgxpool.Pool, messageID string, at time.Time, n int) {
	t.Helper()
	repo := NewRepository(db)
	for range n {
		require.NoError(t, repo.CreateMessageState(context.Background(), messageID, &pb.State{
			State:      pb.State_STATE_COMPLETED,
			OccurredAt: timestamppb.New(at),
		}))
	}
}

func countStates(t *testing.T, db *pgxpool.Pool, messageID string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(context.Background(),
		`SELECT count(*) FROM message_states WHERE message_id=$1`, messageID).Scan(&n))
	return n
}

func TestPruneRemovesStatesPastTheStreamRetention(t *testing.T) {
	now := time.Now()
	db, pruner := prunerDB(t, now)
	pruner.batch = 2
	addStates(t, db, "expired", now.Add(-StateRetention-time.Minute), 5)
	addStates(t, db, "kept", now.Add(-StateRetention+time.Minute), 3)

	deleted, err := pruner.prune(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(5), deleted)
	require.Zero(t, countStates(t, db, "expired"))
	require.Equal(t, 3, countStates(t, db, "kept"))
}

func TestPruneDeletesAtMostItsBatchesPerRun(t *testing.T) {
	now := time.Now()
	db, pruner := prunerDB(t, now)
	pruner.batch = 2
	pruner.batches = 2
	addStates(t, db, "expired", now.Add(-48*time.Hour), 7)

	deleted, err := pruner.prune(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(4), deleted)
	require.Equal(t, 3, countStates(t, db, "expired"))

	deleted, err = pruner.prune(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(3), deleted)
	require.Zero(t, countStates(t, db, "expired"))
}

func TestPruneRunKeepsPruningEveryInterval(t *testing.T) {
	now := time.Now()
	db, pruner := prunerDB(t, now)
	pruner.interval = 50 * time.Millisecond
	addStates(t, db, "first", now.Add(-48*time.Hour), 1)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- pruner.run(ctx) }()

	require.Eventually(t, func() bool { return countStates(t, db, "first") == 0 },
		5*time.Second, 20*time.Millisecond, "the first prune runs at once")
	addStates(t, db, "later", now.Add(-48*time.Hour), 1)
	require.Eventually(t, func() bool { return countStates(t, db, "later") == 0 },
		5*time.Second, 20*time.Millisecond, "the next prune runs after the interval")

	cancel()
	require.NoError(t, <-done, "stopping is not a failure")
}

func TestPruneRunReturnsTheFailure(t *testing.T) {
	db, pruner := prunerDB(t, time.Now())
	_, err := db.Exec(context.Background(), `DROP TABLE message_states`)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- pruner.run(context.Background()) }()
	select {
	case err := <-done:
		require.ErrorContains(t, err, "error pruning message states")
		require.ErrorContains(t, err, "message_states")
	case <-time.After(10 * time.Second):
		t.Fatal("a failed prune must end run with its error")
	}
}
