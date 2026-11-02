package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/internal/postgres"
	internal_pb "github.com/lhjnilsson/foreverbull/pkg/pb"
	pb "github.com/lhjnilsson/foreverbull/pkg/pb/strategy"
)

const DeploymentTable = `
CREATE TABLE IF NOT EXISTS strategy_deployment (
	id text PRIMARY KEY DEFAULT uuid_generate_v4(),
	strategy_id text NOT NULL REFERENCES strategy(id) ON DELETE CASCADE,
	version_id text NOT NULL REFERENCES strategy_version(id) ON DELETE CASCADE,
	mode int NOT NULL DEFAULT 0,
	backend text NOT NULL,
	calendar_code text NOT NULL,
	symbols text[] NOT NULL DEFAULT '{}',
	schedule_kind int NOT NULL DEFAULT 0,
	dry_run boolean NOT NULL DEFAULT true,
	status int NOT NULL DEFAULT 0,
	last_session_date date,
	error text,
	started_at TIMESTAMPTZ,
	stopped_at TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS strategy_one_running_deployment
	ON strategy_deployment (strategy_id) WHERE status = 1;
`

type Deployment struct {
	Conn postgres.Query
}

func (db *Deployment) Create(
	ctx context.Context,
	versionID, backend, calendarCode string,
	mode pb.Deployment_Mode,
	symbols []string,
	scheduleKind pb.Deployment_ScheduleKind,
	dryRun bool,
) (*pb.Deployment, error) {
	if symbols == nil {
		symbols = []string{}
	}

	var id string
	err := db.Conn.QueryRow(ctx,
		`INSERT INTO strategy_deployment (
			strategy_id, version_id, mode, backend, calendar_code, symbols,
			schedule_kind, dry_run, status, started_at
		)
		SELECT strategy_id, id, $2, $3, $4, $5, $6, $7, $8, NOW()
		FROM strategy_version WHERE id=$1
		RETURNING id`,
		versionID, mode, backend, calendarCode, symbols, scheduleKind, dryRun, pb.Deployment_RUNNING,
	).Scan(&id)
	if err != nil {
		return nil, fmt.Errorf("failed to create strategy deployment: %w", err)
	}

	return db.Get(ctx, id)
}

func (db *Deployment) Get(ctx context.Context, id string) (*pb.Deployment, error) {
	d, err := db.scanOne(ctx, deploymentSelect+` WHERE d.id=$1`, id)
	if err != nil {
		return nil, fmt.Errorf("failed to get strategy deployment: %w", err)
	}

	return d, nil
}

func (db *Deployment) GetRunningByStrategyID(ctx context.Context, strategyID string) (*pb.Deployment, error) {
	if strategyID == "" {
		return nil, fmt.Errorf("strategy id is empty")
	}
	d, err := db.scanOne(ctx, deploymentSelect+` WHERE d.strategy_id=$1 AND d.status=$2`, strategyID, pb.Deployment_RUNNING)
	if err != nil {
		return nil, err
	}
	return d, nil
}

func (db *Deployment) ListByStrategyID(ctx context.Context, strategyID string) ([]*pb.Deployment, error) {
	if strategyID == "" {
		return nil, fmt.Errorf("strategy id is empty")
	}
	rows, err := db.Conn.Query(ctx, deploymentSelect+`
		WHERE d.strategy_id = $1
		ORDER BY d.started_at ASC NULLS LAST, d.id ASC`, strategyID)
	if err != nil {
		return nil, fmt.Errorf("failed to list strategy deployments: %w", err)
	}
	defer rows.Close()
	return parseDeploymentRows(rows)
}

func (db *Deployment) GetRunningByStrategyName(ctx context.Context, strategyName string) (*pb.Deployment, error) {
	d, err := db.scanOne(ctx,
		deploymentSelect+` WHERE s.name=$1 AND d.status=$2`,
		strategyName, pb.Deployment_RUNNING,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get running strategy deployment: %w", err)
	}

	return d, nil
}

func (db *Deployment) List(ctx context.Context, strategyName string) ([]*pb.Deployment, error) {
	if environment.GetAuthEnabled() {
		return nil, fmt.Errorf("list deployments by name is refused while AUTH_ENABLED=true")
	}
	rows, err := db.Conn.Query(ctx, deploymentSelect+`
		WHERE ($1 = '' OR s.name = $1)
		ORDER BY d.started_at ASC NULLS LAST, d.id ASC`,
		strategyName,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list strategy deployments: %w", err)
	}
	defer rows.Close()

	return parseDeploymentRows(rows)
}

func (db *Deployment) ListRunning(ctx context.Context) ([]*pb.Deployment, error) {
	rows, err := db.Conn.Query(ctx, deploymentSelect+`
		WHERE d.status=$1 ORDER BY d.started_at ASC`,
		pb.Deployment_RUNNING,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list running strategy deployments: %w", err)
	}
	defer rows.Close()

	return parseDeploymentRows(rows)
}

func (db *Deployment) UpdateStatus(ctx context.Context, id string, status pb.Deployment_Status, statusErr error) error {
	var errText *string
	if statusErr != nil {
		msg := statusErr.Error()
		errText = &msg
	}

	_, err := db.Conn.Exec(ctx,
		`UPDATE strategy_deployment SET status=$2, error=$3 WHERE id=$1`,
		id, status, errText,
	)
	if err != nil {
		return fmt.Errorf("failed to update deployment status: %w", err)
	}

	return nil
}

// Fail stores cause on the deployment as ERROR and returns cause. The write
// is detached from ctx like Experiment.Fail. When the status can't be
// written, it returns a *StatusNotStoredError naming both.
func (db *Deployment) Fail(ctx context.Context, id string, cause error) error {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), StatusWriteTimeout)
	defer cancel()
	if err := db.UpdateStatus(writeCtx, id, pb.Deployment_ERROR, cause); err != nil {
		return &StatusNotStoredError{Cause: cause, Err: err, what: fmt.Sprintf("deployment %s couldn't be marked as an error", id)}
	}
	return cause
}

func (db *Deployment) Stop(ctx context.Context, id string) error {
	_, err := db.Conn.Exec(ctx,
		`UPDATE strategy_deployment SET status=$2, stopped_at=NOW() WHERE id=$1`,
		id, pb.Deployment_STOPPED,
	)
	if err != nil {
		return fmt.Errorf("failed to stop strategy deployment: %w", err)
	}

	return nil
}

func (db *Deployment) SetLastSessionDate(ctx context.Context, id string, date *internal_pb.Date) error {
	_, err := db.Conn.Exec(ctx,
		`UPDATE strategy_deployment SET last_session_date=$2 WHERE id=$1`,
		id, internal_pb.DateToDateString(date),
	)
	if err != nil {
		return fmt.Errorf("failed to set deployment last session date: %w", err)
	}

	return nil
}

const deploymentSelect = `
SELECT d.id, d.strategy_id, s.name, d.version_id, v.rev, d.mode, d.backend,
	d.calendar_code, d.symbols, d.schedule_kind, d.dry_run, d.status, d.error,
	d.last_session_date, d.started_at, d.stopped_at
FROM strategy_deployment d
JOIN strategy s ON d.strategy_id = s.id
JOIN strategy_version v ON d.version_id = v.id`

func (db *Deployment) scanOne(ctx context.Context, query string, args ...interface{}) (*pb.Deployment, error) {
	d := pb.Deployment{}
	var symbols []string
	var errText *string
	var lastSession pgtype.Date
	var startedAt *time.Time
	var stoppedAt *time.Time

	err := db.Conn.QueryRow(ctx, query, args...).Scan(
		&d.Id, &d.StrategyId, &d.StrategyName, &d.VersionId, &d.VersionRev, &d.Mode, &d.Backend,
		&d.CalendarCode, &symbols, &d.ScheduleKind, &d.DryRun, &d.Status, &errText,
		&lastSession, &startedAt, &stoppedAt,
	)
	if err != nil {
		return nil, err
	}

	fillDeployment(&d, symbols, errText, lastSession, startedAt, stoppedAt)

	return &d, nil
}

func parseDeploymentRows(rows pgx.Rows) ([]*pb.Deployment, error) {
	deployments := []*pb.Deployment{}

	for rows.Next() {
		d := pb.Deployment{}
		var symbols []string
		var errText *string
		var lastSession pgtype.Date
		var startedAt *time.Time
		var stoppedAt *time.Time

		err := rows.Scan(
			&d.Id, &d.StrategyId, &d.StrategyName, &d.VersionId, &d.VersionRev, &d.Mode, &d.Backend,
			&d.CalendarCode, &symbols, &d.ScheduleKind, &d.DryRun, &d.Status, &errText,
			&lastSession, &startedAt, &stoppedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan strategy deployment: %w", err)
		}

		fillDeployment(&d, symbols, errText, lastSession, startedAt, stoppedAt)
		deployments = append(deployments, &d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read strategy deployments: %w", err)
	}

	return deployments, nil
}

func fillDeployment(
	d *pb.Deployment,
	symbols []string,
	errText *string,
	lastSession pgtype.Date,
	startedAt, stoppedAt *time.Time,
) {
	d.Symbols = symbols
	d.Error = errText
	if lastSession.Valid {
		d.LastSessionDate = internal_pb.GoTimeToDate(lastSession.Time)
	}
	if startedAt != nil {
		d.StartedAt = internal_pb.TimeToProtoTimestamp(*startedAt)
	}
	if stoppedAt != nil {
		d.StoppedAt = internal_pb.TimeToProtoTimestamp(*stoppedAt)
	}
}
