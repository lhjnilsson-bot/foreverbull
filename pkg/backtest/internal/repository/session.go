package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/lhjnilsson/foreverbull/internal/auth"
	"github.com/lhjnilsson/foreverbull/internal/postgres"
	pb "github.com/lhjnilsson/foreverbull/pkg/pb/backtest"
)

const SessionTable = `
CREATE TABLE IF NOT EXISTS session (
	id text PRIMARY KEY DEFAULT uuid_generate_v4 (),
	backtest text NOT NULL REFERENCES backtest(name) ON DELETE CASCADE,
	port integer);
`

// MigrateSessionBacktest sets session.backtest NOT NULL.
// A null backtest stays. Once backtest_id is NOT NULL, or the name column is
// gone, this returns without reading the old column. Calling it twice is safe.
func MigrateSessionBacktest(ctx context.Context, conn postgres.Query) error {
	hasName, err := auth.ColumnExists(ctx, conn, "session", "backtest")
	if err != nil {
		return err
	}
	if !hasName {
		return nil
	}
	idNotNull, err := auth.ColumnNotNull(ctx, conn, "session", "backtest_id")
	if err != nil {
		return err
	}
	if idNotNull {
		return nil
	}

	var bad bool
	err = conn.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM session WHERE backtest IS NULL
		)`).Scan(&bad)
	if err != nil {
		return fmt.Errorf("session: scan backtest: %w", err)
	}
	if bad {
		return fmt.Errorf("session: rows violate backtest NOT NULL")
	}

	_, err = conn.Exec(ctx, `ALTER TABLE session ALTER COLUMN backtest SET NOT NULL`)
	if err != nil {
		return fmt.Errorf("session: backtest not null: %w", err)
	}
	return nil
}

type Session struct {
	Conn postgres.Query
}

func (db *Session) Create(ctx context.Context, backtest string) (*pb.Session, error) {
	if authOn() {
		return nil, fmt.Errorf("create session by name is refused while AUTH_ENABLED=true")
	}
	var sessionID string

	err := db.Conn.QueryRow(ctx, `INSERT INTO session (backtest) VALUES ($1) RETURNING id`,
		backtest).Scan(&sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to create session: %w", err)
	}

	return db.Get(ctx, sessionID)
}

func (db *Session) Get(ctx context.Context, sessionID string) (*pb.Session, error) {
	if authOn() {
		return db.getJoined(ctx, sessionID)
	}
	session := pb.Session{}
	err := db.Conn.QueryRow(ctx,
		`SELECT id, backtest, port FROM session WHERE id=$1`, sessionID).Scan(
		&session.Id, &session.Backtest, &session.Port,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get session: %w", err)
	}

	return &session, nil
}

func (db *Session) UpdatePort(ctx context.Context, sessionID string, port int) error {
	_, err := db.Conn.Exec(ctx, `UPDATE session SET port=$1 WHERE id=$2`, port, sessionID)
	if err != nil {
		return fmt.Errorf("failed to update session port: %w", err)
	}

	return nil
}

func (db *Session) parseRows(rows pgx.Rows) ([]*pb.Session, error) {
	sessions := []*pb.Session{}
	for rows.Next() {
		session := pb.Session{}
		err := rows.Scan(
			&session.Id, &session.Backtest, &session.Port)
		if err != nil {
			return nil, fmt.Errorf("failed to scan: %w", err)
		}
		sessions = append(sessions, &session)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read sessions: %w", err)
	}
	return sessions, nil
}

func (db *Session) List(ctx context.Context) ([]*pb.Session, error) {
	rows, err := db.Conn.Query(ctx,
		`SELECT id, backtest, port
		FROM session`)
	if err != nil {
		return nil, fmt.Errorf("failed to list sessions: %w", err)
	}

	defer rows.Close()

	return db.parseRows(rows)
}

func (db *Session) ListByBacktest(ctx context.Context, backtest string) ([]*pb.Session, error) {
	rows, err := db.Conn.Query(ctx,
		`SELECT id, backtest, port
		FROM session
		WHERE backtest=$1`, backtest)
	if err != nil {
		return nil, fmt.Errorf("failed to list sessions: %w", err)
	}

	defer rows.Close()

	return db.parseRows(rows)
}
