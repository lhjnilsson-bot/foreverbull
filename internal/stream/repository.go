package stream

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lhjnilsson/foreverbull/internal/stream/pb"
	"github.com/rs/zerolog/log"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func CreateTables(ctx context.Context, conn *pgxpool.Pool) error {
	return withSchemaLock(ctx, conn, func(c *pgxpool.Conn) error {
		if _, err := c.Exec(ctx, table); err != nil {
			return fmt.Errorf("failed to create table: %w", err)
		}
		return createOccurredAtIndex(ctx, c)
	})
}

// schemaLockKey is the pg_advisory_lock key brokers hold while they create
// the stream tables and occurredAtIndex. The value is arbitrary but fixed,
// so every broker version takes the same lock.
const schemaLockKey int64 = 0x666231313673

// withSchemaLock runs fn on one pooled connection while it holds the
// session advisory lock schemaLockKey. Brokers that start together then
// create the tables and the index one after another: without it, one sees
// the other's index while it is still building (indisvalid false) and
// drops it, or the two concurrent builds deadlock. The lock is a session
// lock outside any transaction, as CREATE INDEX CONCURRENTLY can't run in
// one, and it is released on the same connection.
//
// The lock is taken with pg_try_advisory_lock every schemaLockPoll, not a
// blocking pg_advisory_lock: a session blocked in pg_advisory_lock is a
// statement holding a snapshot, and CREATE INDEX CONCURRENTLY waits for
// every older snapshot to end, so the holder's build and the waiter
// deadlock (Postgres aborts one with 40P01). Between tries the waiter holds
// no snapshot.
//
// The broker's start passes context.Background(), so the wait has its own
// bound, schemaLockWait: a holder that is stuck fails the start with
// "another broker holds it" and the holder's pid in the log, rather than a
// silent wait the startupProbe kills. Only the wait is bounded; fn, and so
// the index build, runs on ctx.
func withSchemaLock(ctx context.Context, pool *pgxpool.Pool, fn func(*pgxpool.Conn) error) (err error) {
	c, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("failed to acquire a connection for the schema lock: %w", err)
	}
	defer c.Release()
	waitCtx, cancel := context.WithTimeout(ctx, schemaLockWait)
	err = trySchemaLock(waitCtx, c)
	cancel()
	if err != nil {
		return err
	}
	defer func() {
		// The unlock runs even when ctx is done, so the lock doesn't stay
		// held on a connection that goes back to the pool.
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), schemaUnlockTimeout)
		defer cancel()
		var unlocked bool
		unlockErr := c.QueryRow(unlockCtx, `SELECT pg_advisory_unlock($1)`, schemaLockKey).Scan(&unlocked)
		switch {
		case unlockErr != nil:
			unlockErr = fmt.Errorf("failed to release the schema lock: %w", unlockErr)
		case !unlocked:
			unlockErr = errors.New("failed to release the schema lock: it wasn't held")
		default:
			return
		}
		// Closing the connection ends its session and so any lock it may
		// still hold; the pool then drops it on Release instead of handing
		// it out with the lock.
		if closeErr := c.Conn().Close(unlockCtx); closeErr != nil {
			unlockErr = errors.Join(unlockErr, fmt.Errorf("failed to close the schema lock's connection: %w", closeErr))
		}
		err = errors.Join(err, unlockErr)
	}()
	return fn(c)
}

// schemaLockPoll is how often withSchemaLock tries the lock while another
// broker holds it.
const schemaLockPoll = 100 * time.Millisecond

// schemaLockWaitMax is how long withSchemaLock waits for another broker's
// schema lock: below the chart's 120s broker startupProbe, so the start
// fails with its own cause before the probe kills the pod.
const schemaLockWaitMax = 90 * time.Second

// schemaLockWait is the wait withSchemaLock uses, schemaLockWaitMax outside
// tests.
var schemaLockWait = schemaLockWaitMax

// schemaUnlockTimeout bounds the unlock, which runs on a context that
// outlives the caller's.
const schemaUnlockTimeout = 5 * time.Second

// trySchemaLock takes schemaLockKey on c, trying every schemaLockPoll until
// it gets it or ctx ends. The first failed try logs a warning with the
// holder's pid.
func trySchemaLock(ctx context.Context, c *pgxpool.Conn) error {
	ticker := time.NewTicker(schemaLockPoll)
	defer ticker.Stop()
	for tries := 0; ; tries++ {
		var locked bool
		if err := c.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, schemaLockKey).Scan(&locked); err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("failed to take the schema lock: another broker holds it: %w", err)
			}
			return fmt.Errorf("failed to take the schema lock: %w", err)
		}
		if locked {
			return nil
		}
		if tries == 0 {
			logSchemaLockHolder(ctx, c)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("failed to take the schema lock: another broker holds it: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// logSchemaLockHolder warns that another session holds schemaLockKey, with
// its pid from pg_locks. A bigint advisory key is split into classid (high
// 32 bits) and objid (low 32 bits), with objsubid 1. A failed lookup is
// logged with the warning; the wait goes on either way.
func logSchemaLockHolder(ctx context.Context, c *pgxpool.Conn) {
	event := log.Warn().Dur("wait", schemaLockWait).Int64("key", schemaLockKey)
	var pid int32
	err := c.QueryRow(ctx,
		`SELECT pid FROM pg_locks WHERE locktype = 'advisory' AND granted
		AND classid = ($1::bigint >> 32)::oid AND objid = ($1::bigint & 4294967295)::oid AND objsubid = 1
		LIMIT 1`, schemaLockKey).Scan(&pid)
	switch {
	case err == nil:
		event = event.Int32("holder_pid", pid)
	case errors.Is(err, pgx.ErrNoRows):
		// The holder let go between the try and the lookup.
	default:
		event = event.AnErr("lookup_error", err)
	}
	event.Msg("another broker holds the schema lock; waiting for it")
}

// occurredAtIndex serves the prune's scan of message_states by occurred_at.
const occurredAtIndex = "message_states_occurred_at_idx"

// createOccurredAtIndex builds occurredAtIndex without locking out writes:
// CONCURRENTLY, in its own statement, as it can't run in a transaction. On a
// large message_states the build takes a while, and the broker's startup
// waits for it. A build that was interrupted leaves an invalid index that
// IF NOT EXISTS would keep, so that one is dropped and built again. The
// caller holds the schema lock, so an invalid index is never another
// broker's build in progress.
func createOccurredAtIndex(ctx context.Context, conn *pgxpool.Conn) error {
	var valid bool
	err := conn.QueryRow(ctx,
		`SELECT i.indisvalid FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
		WHERE c.relname = $1 AND c.relnamespace = current_schema()::regnamespace`, occurredAtIndex).Scan(&valid)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return fmt.Errorf("failed to read index %s: %w", occurredAtIndex, err)
	case valid:
		return nil
	default:
		if _, err := conn.Exec(ctx, `DROP INDEX CONCURRENTLY IF EXISTS `+occurredAtIndex); err != nil {
			return fmt.Errorf("failed to drop invalid index %s: %w", occurredAtIndex, err)
		}
	}
	if _, err := conn.Exec(ctx,
		`CREATE INDEX CONCURRENTLY IF NOT EXISTS `+occurredAtIndex+` ON message_states (occurred_at)`); err != nil {
		return fmt.Errorf("failed to create index %s: %w", occurredAtIndex, err)
	}
	return nil
}

func RecreateTables(ctx context.Context, conn *pgxpool.Pool) error {
	if _, err := conn.Exec(ctx, `DROP TABLE IF EXISTS message_states;`); err != nil {
		return fmt.Errorf("failed to drop table: %w", err)
	}

	if _, err := conn.Exec(ctx, `DROP TABLE IF EXISTS message;`); err != nil {
		return fmt.Errorf("failed to drop table: %w", err)
	}

	return CreateTables(ctx, conn)
}

const table = `
CREATE TABLE IF NOT EXISTS message (
	id text PRIMARY KEY,
	module text NOT NULL,
	component text NOT NULL,
	method text NOT NULL,
	payload BYTEA,

	orchestration_name text,
	orchestration_id text,
	orchestration_step text,
	orchestration_step_number integer,
	orchestration_fallback_step boolean
);

CREATE TABLE IF NOT EXISTS message_states (
	id serial PRIMARY KEY,
	message_id text,
	state int NOT NULL,
	error text,
	occurred_at TIMESTAMPTZ NOT NULL
);
`

type Repository struct {
	db *pgxpool.Pool
}

func NewRepository(db *pgxpool.Pool) *Repository {
	return &Repository{db: db}
}

func (r *Repository) CreateMessage(ctx context.Context, msg *pb.Message) error {
	_, err := r.db.Exec(ctx,
		`INSERT INTO message (id, module, component, method, payload,
	orchestration_name, orchestration_id, orchestration_step, orchestration_step_number, orchestration_fallback_step)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`, msg.Id,
		msg.Module, msg.Component, msg.Method, msg.Payload,
		msg.OrchestrationName, msg.OrchestrationId, msg.OrchestrationStep, msg.OrchestrationStepNumber, msg.OrchestrationFallbackStep)
	if err != nil {
		return fmt.Errorf("failed to insert message: %w", err)
	}
	return nil
}

func (r *Repository) CreateMessageState(ctx context.Context, messageID string, state *pb.State) error {
	_, err := r.db.Exec(ctx,
		`INSERT INTO message_states (message_id, state, error, occurred_at)
		VALUES ($1, $2, $3, $4)`, messageID, state.State, state.Error, state.OccurredAt.AsTime())
	if err != nil {
		return fmt.Errorf("failed to insert message state: %w", err)
	}
	return nil
}

// DeleteStatesBefore deletes at most limit message_states rows that occurred
// before cutoff, oldest first, and returns how many it deleted. The limit
// keeps each delete a short statement on a large backlog.
func (r *Repository) DeleteStatesBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	if limit <= 0 {
		return 0, fmt.Errorf("delete limit must be above zero, got %d", limit)
	}
	tag, err := r.db.Exec(ctx,
		`DELETE FROM message_states WHERE id IN (
			SELECT id FROM message_states WHERE occurred_at < $1
			ORDER BY occurred_at LIMIT $2)`, cutoff, limit)
	if err != nil {
		return 0, fmt.Errorf("failed to delete message states before %s: %w", cutoff.Format(time.RFC3339), err)
	}
	return tag.RowsAffected(), nil
}

func (r *Repository) GetMessage(ctx context.Context, messageID string) (*pb.Message, []*pb.State, error) {
	msg := pb.Message{}
	states := []*pb.State{}
	err := r.db.QueryRow(ctx,
		`SELECT module, component, method, payload, 
		orchestration_name, orchestration_id, orchestration_step, orchestration_step_number, 
		orchestration_fallback_step FROM message WHERE id=$1`, messageID).Scan(
		&msg.Module, &msg.Component, &msg.Method, &msg.Payload,
		&msg.OrchestrationName, &msg.OrchestrationId, &msg.OrchestrationStep, &msg.OrchestrationStepNumber,
		&msg.OrchestrationFallbackStep)
	if err != nil {
		return nil, states, fmt.Errorf("failed to query message: %w", err)
	}

	stateRows, err := r.db.Query(ctx,
		`SELECT state, error, occurred_at FROM message_states WHERE message_id=$1
		ORDER BY occurred_at DESC`, messageID)
	if err != nil {
		return nil, states, fmt.Errorf("failed to query message: %w", err)
	}
	defer stateRows.Close()
	for stateRows.Next() {
		state := pb.State{}
		t := time.Time{}
		err := stateRows.Scan(&state.State, &state.Error, &t)
		if err != nil {
			return nil, states, fmt.Errorf("failed to scan message: %w", err)
		}
		state.OccurredAt = timestamppb.New(t)
		states = append(states, &state)
	}

	return &msg, states, nil
}
