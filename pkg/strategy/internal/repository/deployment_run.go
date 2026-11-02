package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/internal/postgres"
	internal_pb "github.com/lhjnilsson/foreverbull/pkg/pb"
	financepb "github.com/lhjnilsson/foreverbull/pkg/pb/finance"
	pb "github.com/lhjnilsson/foreverbull/pkg/pb/strategy"
)

const DeploymentRunTable = `
CREATE TABLE IF NOT EXISTS strategy_deployment_run (
	id text PRIMARY KEY DEFAULT uuid_generate_v4(),
	deployment_id text NOT NULL REFERENCES strategy_deployment(id) ON DELETE CASCADE,
	session_date date NOT NULL,
	started_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	orders jsonb,
	skipped boolean NOT NULL DEFAULT false,
	error text,
	UNIQUE (deployment_id, session_date)
);
`

type DeploymentRun struct {
	Conn postgres.Query
}

func (db *DeploymentRun) Create(
	ctx context.Context,
	deploymentID string,
	sessionDate *internal_pb.Date,
	orders []*financepb.Order,
	skipped bool,
	runErr error,
) (*pb.DeploymentRun, error) {
	ordersJSON, err := ordersToJSON(orders)
	if err != nil {
		return nil, err
	}

	var errText *string
	if runErr != nil {
		msg := runErr.Error()
		errText = &msg
	}

	var id string
	err = db.Conn.QueryRow(ctx,
		`INSERT INTO strategy_deployment_run (deployment_id, session_date, orders, skipped, error)
		VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		deploymentID, internal_pb.DateToDateString(sessionDate), ordersJSON, skipped, errText,
	).Scan(&id)
	if err != nil {
		return nil, fmt.Errorf("failed to create strategy deployment run: %w", err)
	}

	return db.Get(ctx, id)
}

func (db *DeploymentRun) Get(ctx context.Context, id string) (*pb.DeploymentRun, error) {
	r, err := db.scanOne(ctx, deploymentRunSelect+` WHERE r.id=$1`, id)
	if err != nil {
		return nil, fmt.Errorf("failed to get strategy deployment run: %w", err)
	}

	return r, nil
}

func (db *DeploymentRun) List(ctx context.Context, strategyName, deploymentID string) ([]*pb.DeploymentRun, error) {
	if environment.GetAuthEnabled() {
		return nil, fmt.Errorf("list deployment runs by name is refused while AUTH_ENABLED=true")
	}
	rows, err := db.Conn.Query(ctx, deploymentRunSelect+`
		WHERE ($1 = '' OR s.name = $1)
		AND ($2 = '' OR r.deployment_id = $2)
		ORDER BY r.session_date ASC, r.started_at ASC`,
		strategyName, deploymentID,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list strategy deployment runs: %w", err)
	}
	defer rows.Close()

	return parseDeploymentRunRows(rows)
}

// ListByStrategyID lists runs after the caller was authorized for that strategy.
func (db *DeploymentRun) ListByStrategyID(ctx context.Context, strategyID, deploymentID string) ([]*pb.DeploymentRun, error) {
	if strategyID == "" {
		return nil, fmt.Errorf("strategy id is empty")
	}
	rows, err := db.Conn.Query(ctx, deploymentRunSelect+`
		WHERE d.strategy_id = $1
		AND ($2 = '' OR r.deployment_id = $2)
		ORDER BY r.session_date ASC, r.started_at ASC`, strategyID, deploymentID)
	if err != nil {
		return nil, fmt.Errorf("failed to list strategy deployment runs: %w", err)
	}
	defer rows.Close()
	return parseDeploymentRunRows(rows)
}

const deploymentRunSelect = `
SELECT r.id, r.deployment_id, r.session_date, r.started_at, r.orders, r.skipped, r.error
FROM strategy_deployment_run r
JOIN strategy_deployment d ON r.deployment_id = d.id
JOIN strategy s ON d.strategy_id = s.id`

func (db *DeploymentRun) scanOne(ctx context.Context, query string, args ...interface{}) (*pb.DeploymentRun, error) {
	r := pb.DeploymentRun{}
	var sessionDate pgtype.Date
	var startedAt time.Time
	var ordersJSON []byte
	var errText *string

	err := db.Conn.QueryRow(ctx, query, args...).Scan(
		&r.Id, &r.DeploymentId, &sessionDate, &startedAt, &ordersJSON, &r.Skipped, &errText,
	)
	if err != nil {
		return nil, err
	}

	if err := fillDeploymentRun(&r, sessionDate, startedAt, ordersJSON, errText); err != nil {
		return nil, err
	}

	return &r, nil
}

func parseDeploymentRunRows(rows pgx.Rows) ([]*pb.DeploymentRun, error) {
	runs := []*pb.DeploymentRun{}

	for rows.Next() {
		r := pb.DeploymentRun{}
		var sessionDate pgtype.Date
		var startedAt time.Time
		var ordersJSON []byte
		var errText *string

		err := rows.Scan(
			&r.Id, &r.DeploymentId, &sessionDate, &startedAt, &ordersJSON, &r.Skipped, &errText,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan strategy deployment run: %w", err)
		}

		if err := fillDeploymentRun(&r, sessionDate, startedAt, ordersJSON, errText); err != nil {
			return nil, err
		}

		runs = append(runs, &r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read strategy deployment runs: %w", err)
	}

	return runs, nil
}

func fillDeploymentRun(
	r *pb.DeploymentRun,
	sessionDate pgtype.Date,
	startedAt time.Time,
	ordersJSON []byte,
	errText *string,
) error {
	if sessionDate.Valid {
		r.SessionDate = internal_pb.GoTimeToDate(sessionDate.Time)
	}

	r.StartedAt = internal_pb.TimeToProtoTimestamp(startedAt)
	r.Error = errText

	orders, err := ordersFromJSON(ordersJSON)
	if err != nil {
		return err
	}

	r.Orders = orders

	return nil
}

func ordersToJSON(orders []*financepb.Order) ([]byte, error) {
	if orders == nil {
		orders = []*financepb.Order{}
	}

	b, err := json.Marshal(orders)
	if err != nil {
		return nil, fmt.Errorf("failed to encode orders: %w", err)
	}

	return b, nil
}

func ordersFromJSON(raw []byte) ([]*financepb.Order, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	var orders []*financepb.Order
	if err := json.Unmarshal(raw, &orders); err != nil {
		return nil, fmt.Errorf("failed to decode orders: %w", err)
	}

	return orders, nil
}
