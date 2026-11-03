package stream

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/internal/test_helper"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

// syncBuffer is a log sink the lock wait and the test can share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Split(strings.TrimSpace(b.buf.String()), "\n")
}

// A start that finds the schema lock held warns once with the holder's pid
// and fails at schemaLockWait with its own cause, even on a context without
// a deadline, as the broker's start passes.
func TestCreateTablesFailsWhenTheSchemaLockWaitEnds(t *testing.T) {
	test_helper.SetupEnvironment(t, &test_helper.Containers{Postgres: true})
	db, err := pgxpool.New(context.Background(), environment.GetPostgresURL())
	require.NoError(t, err)
	t.Cleanup(db.Close)
	require.NoError(t, RecreateTables(context.Background(), db))

	holder, err := db.Acquire(context.Background())
	require.NoError(t, err)
	defer holder.Release()
	var holderPID int32
	require.NoError(t, holder.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&holderPID))
	_, err = holder.Exec(context.Background(), `SELECT pg_advisory_lock($1)`, schemaLockKey)
	require.NoError(t, err)
	defer func() {
		_, err := holder.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, schemaLockKey)
		require.NoError(t, err)
	}()

	wait := schemaLockWait
	schemaLockWait = 500 * time.Millisecond
	t.Cleanup(func() { schemaLockWait = wait })
	logs := &syncBuffer{}
	logger := log.Logger
	log.Logger = zerolog.New(logs)
	t.Cleanup(func() { log.Logger = logger })

	start := time.Now()
	err = CreateTables(context.Background(), db)
	require.ErrorContains(t, err, "failed to take the schema lock: another broker holds it")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(start), 5*time.Second, "the wait ends at schemaLockWait")

	var warnings []map[string]any
	for _, line := range logs.lines() {
		entry := map[string]any{}
		require.NoError(t, json.Unmarshal([]byte(line), &entry), line)
		if entry["message"] == "another broker holds the schema lock; waiting for it" {
			warnings = append(warnings, entry)
		}
	}
	require.Len(t, warnings, 1, "the wait warns once, on the first failed try")
	require.Equal(t, "warn", warnings[0]["level"])
	require.InDelta(t, float64(holderPID), warnings[0]["holder_pid"], 0)
	require.NotContains(t, warnings[0], "lookup_error")
}

func TestSchemaLockWaitIsBelowTheStartupProbe(t *testing.T) {
	require.Equal(t, 90*time.Second, schemaLockWaitMax)
	require.Equal(t, schemaLockWaitMax, schemaLockWait)
	require.Less(t, schemaLockWaitMax, 120*time.Second, "the chart's broker startupProbe allows 120s")
}

// An unlock that fails closes the connection, so no pooled connection keeps
// the session lock, and the failure is returned.
func TestAFailedSchemaUnlockClosesTheConnection(t *testing.T) {
	test_helper.SetupEnvironment(t, &test_helper.Containers{Postgres: true})
	db, err := pgxpool.New(context.Background(), environment.GetPostgresURL())
	require.NoError(t, err)
	t.Cleanup(db.Close)

	err = withSchemaLock(context.Background(), db, func(c *pgxpool.Conn) error {
		var pid int32
		if err := c.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			return err
		}
		// The session ends under the lock, so the unlock can't run on it.
		_, err := db.Exec(context.Background(), `SELECT pg_terminate_backend($1)`, pid)
		return err
	})
	require.ErrorContains(t, err, "failed to release the schema lock")

	var locked bool
	require.NoError(t, db.QueryRow(context.Background(), `SELECT pg_try_advisory_lock($1)`, schemaLockKey).Scan(&locked))
	require.True(t, locked, "no session may keep the schema lock")
	_, err = db.Exec(context.Background(), `SELECT pg_advisory_unlock_all()`)
	require.NoError(t, err)
}
