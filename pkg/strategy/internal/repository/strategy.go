package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/internal/postgres"
	internal_pb "github.com/lhjnilsson/foreverbull/pkg/pb"
	pb "github.com/lhjnilsson/foreverbull/pkg/pb/strategy"
)

const StrategyTable = `
CREATE TABLE IF NOT EXISTS strategy (
	id text PRIMARY KEY DEFAULT uuid_generate_v4(),
	name text NOT NULL UNIQUE,
	description text,
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
`

type Strategy struct {
	Conn postgres.Query
}

func (db *Strategy) Create(ctx context.Context, name string, description *string) (*pb.Strategy, error) {
	if environment.GetAuthEnabled() {
		return nil, fmt.Errorf("create strategy by name is refused while AUTH_ENABLED=true")
	}
	_, err := db.Conn.Exec(ctx,
		`INSERT INTO strategy (name, description) VALUES ($1, $2)`,
		name, description,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create strategy: %w", err)
	}

	return db.Get(ctx, name)
}

func (db *Strategy) Get(ctx context.Context, name string) (*pb.Strategy, error) {
	if environment.GetAuthEnabled() {
		return nil, fmt.Errorf("get strategy by name is refused while AUTH_ENABLED=true")
	}
	s, err := db.scanOne(ctx,
		`SELECT id, name, description, created_at FROM strategy WHERE name=$1`, name)
	if err != nil {
		return nil, fmt.Errorf("failed to get strategy: %w", err)
	}

	return s, nil
}

func (db *Strategy) GetByID(ctx context.Context, id string) (*pb.Strategy, error) {
	s, err := db.scanOne(ctx,
		`SELECT id, name, description, created_at FROM strategy WHERE id=$1`, id)
	if err != nil {
		return nil, fmt.Errorf("failed to get strategy: %w", err)
	}

	return s, nil
}

func (db *Strategy) List(ctx context.Context) ([]*pb.Strategy, error) {
	if environment.GetAuthEnabled() {
		return nil, fmt.Errorf("list strategies is refused while AUTH_ENABLED=true")
	}
	rows, err := db.Conn.Query(ctx,
		`SELECT id, name, description, created_at FROM strategy ORDER BY created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("failed to list strategies: %w", err)
	}
	defer rows.Close()

	return parseStrategyRows(rows)
}

func (db *Strategy) Delete(ctx context.Context, name string) error {
	if err := db.guardNameDelete(); err != nil {
		return err
	}
	_, err := db.Conn.Exec(ctx, `DELETE FROM strategy WHERE name=$1`, name)
	if err != nil {
		return fmt.Errorf("failed to delete strategy: %w", err)
	}

	return nil
}

func (db *Strategy) scanOne(ctx context.Context, query string, args ...interface{}) (*pb.Strategy, error) {
	s := pb.Strategy{}
	var description *string
	var createdAt time.Time

	err := db.Conn.QueryRow(ctx, query, args...).Scan(&s.Id, &s.Name, &description, &createdAt)
	if err != nil {
		return nil, err
	}

	s.Description = description
	s.CreatedAt = internal_pb.TimeToProtoTimestamp(createdAt)

	return &s, nil
}

func parseStrategyRows(rows pgx.Rows) ([]*pb.Strategy, error) {
	strategies := []*pb.Strategy{}

	for rows.Next() {
		s := pb.Strategy{}
		var description *string
		var createdAt time.Time

		err := rows.Scan(&s.Id, &s.Name, &description, &createdAt)
		if err != nil {
			return nil, fmt.Errorf("failed to scan strategy: %w", err)
		}

		s.Description = description
		s.CreatedAt = internal_pb.TimeToProtoTimestamp(createdAt)
		strategies = append(strategies, &s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read strategies: %w", err)
	}

	return strategies, nil
}
