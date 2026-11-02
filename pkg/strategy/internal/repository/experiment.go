package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/lhjnilsson/foreverbull/internal/auth"
	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/internal/postgres"
	internal_pb "github.com/lhjnilsson/foreverbull/pkg/pb"
	"github.com/lhjnilsson/foreverbull/pkg/pb/backtest"
	pb "github.com/lhjnilsson/foreverbull/pkg/pb/strategy"
)

const ExperimentTable = `
CREATE TABLE IF NOT EXISTS strategy_experiment (
	id text PRIMARY KEY DEFAULT uuid_generate_v4(),
	strategy_id text NOT NULL REFERENCES strategy(id) ON DELETE CASCADE,
	version_id text NOT NULL REFERENCES strategy_version(id) ON DELETE CASCADE,
	backtest_name text NOT NULL,
	execution_id text REFERENCES backtest_execution(id),
	status int NOT NULL DEFAULT 0,
	error text,
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	completed_at TIMESTAMPTZ,
	CONSTRAINT strategy_experiment_backtest_name_fkey
		FOREIGN KEY (backtest_name) REFERENCES backtest (name)
);
`

const experimentBacktestForeignKey = "strategy_experiment_backtest_name_fkey"

// MigrateExperimentBacktest references strategy_experiment.backtest_name to backtest(name).
// A row whose backtest is missing stays. Calling it twice is safe.
func MigrateExperimentBacktest(ctx context.Context, conn postgres.Query) error {
	hasName, err := auth.ColumnExists(ctx, conn, "strategy_experiment", "backtest_name")
	if err != nil {
		return err
	}
	if !hasName {
		return nil
	}
	idRef, err := experimentReferencesBacktestID(ctx, conn)
	if err != nil {
		return err
	}
	if idRef {
		return nil
	}

	exists, err := strategyConstraintExists(ctx, conn, "strategy_experiment", experimentBacktestForeignKey)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	var bad bool
	err = conn.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM strategy_experiment e
			LEFT JOIN backtest b ON b.name = e.backtest_name
			WHERE b.name IS NULL
		)`).Scan(&bad)
	if err != nil {
		return fmt.Errorf("strategy_experiment: scan foreign key: %w", err)
	}
	if bad {
		return fmt.Errorf("strategy_experiment: rows violate foreign key backtest_name references backtest(name)")
	}

	_, err = conn.Exec(ctx, `
		ALTER TABLE strategy_experiment
			ADD CONSTRAINT strategy_experiment_backtest_name_fkey
			FOREIGN KEY (backtest_name) REFERENCES backtest (name)`)
	if err != nil {
		return fmt.Errorf("strategy_experiment: foreign key: %w", err)
	}
	return nil
}

func experimentReferencesBacktestID(ctx context.Context, conn postgres.Query) (bool, error) {
	var exists bool
	err := conn.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM pg_constraint c
			JOIN pg_class src ON src.oid = c.conrelid
			JOIN pg_namespace sn ON sn.oid = src.relnamespace
			JOIN pg_class dst ON dst.oid = c.confrelid
			JOIN LATERAL unnest(c.conkey) AS src_col(attnum) ON true
			JOIN pg_attribute sa ON sa.attrelid = src.oid AND sa.attnum = src_col.attnum
			JOIN LATERAL unnest(c.confkey) AS dst_col(attnum) ON true
			JOIN pg_attribute da ON da.attrelid = dst.oid AND da.attnum = dst_col.attnum
			WHERE c.contype = 'f'
			  AND sn.nspname = current_schema()
			  AND src.relname = 'strategy_experiment'
			  AND dst.relname = 'backtest'
			  AND sa.attname = 'backtest_id'
			  AND da.attname = 'id'
		)`).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("strategy_experiment: read backtest_id foreign key: %w", err)
	}
	return exists, nil
}

func strategyConstraintExists(ctx context.Context, conn postgres.Query, table, name string) (bool, error) {
	var exists bool
	err := conn.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM pg_constraint c
			JOIN pg_class t ON t.oid = c.conrelid
			JOIN pg_namespace n ON n.oid = t.relnamespace
			WHERE n.nspname = current_schema()
			  AND t.relname = $1
			  AND c.conname = $2
		)`, table, name).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("%s: read constraint %s: %w", table, name, err)
	}
	return exists, nil
}

type Experiment struct {
	Conn postgres.Query
}

func (db *Experiment) Create(ctx context.Context, versionID, backtestName string) (*pb.Experiment, error) {
	if environment.GetAuthEnabled() {
		return nil, fmt.Errorf("create experiment by backtest name is refused while AUTH_ENABLED=true")
	}
	var id string
	err := db.Conn.QueryRow(ctx,
		`INSERT INTO strategy_experiment (strategy_id, version_id, backtest_name, status)
		SELECT strategy_id, id, $2, $3 FROM strategy_version WHERE id=$1
		RETURNING id`,
		versionID, backtestName, pb.Experiment_QUEUED,
	).Scan(&id)
	if err != nil {
		return nil, fmt.Errorf("failed to create strategy experiment: %w", err)
	}

	return db.Get(ctx, id)
}

func (db *Experiment) Get(ctx context.Context, id string) (*pb.Experiment, error) {
	query := experimentSelect + ` WHERE e.id=$1`
	if environment.GetAuthEnabled() {
		query = experimentSelectOwned + ` WHERE e.id=$1`
	}
	e, err := db.scanOne(ctx, query, id)
	if err != nil {
		return nil, fmt.Errorf("failed to get strategy experiment: %w", err)
	}

	return e, nil
}

func (db *Experiment) List(ctx context.Context, strategyName, versionID, backtestName string) ([]*pb.Experiment, error) {
	if environment.GetAuthEnabled() {
		return nil, fmt.Errorf("list experiments by name is refused while AUTH_ENABLED=true")
	}
	query := experimentSelect + `
		WHERE ($1 = '' OR s.name = $1)
		AND ($2 = '' OR e.version_id = $2)
		AND ($3 = '' OR e.backtest_name = $3)
		ORDER BY e.created_at ASC`
	rows, err := db.Conn.Query(ctx, query, strategyName, versionID, backtestName)
	if err != nil {
		return nil, fmt.Errorf("failed to list strategy experiments: %w", err)
	}
	defer rows.Close()

	return parseExperimentRows(rows)
}

func (db *Experiment) UpdateStatus(ctx context.Context, id string, status pb.Experiment_Status, statusErr error) error {
	var errText *string
	if statusErr != nil {
		msg := statusErr.Error()
		errText = &msg
	}

	query := `UPDATE strategy_experiment SET status=$2, error=$3 WHERE id=$1`
	if status == pb.Experiment_COMPLETED || status == pb.Experiment_FAILED {
		query = `UPDATE strategy_experiment SET status=$2, error=$3, completed_at=NOW() WHERE id=$1`
	}

	_, err := db.Conn.Exec(ctx, query, id, status, errText)
	if err != nil {
		return fmt.Errorf("failed to update experiment status: %w", err)
	}

	return nil
}

// StatusWriteTimeout bounds the write that records a failure. That write
// runs on a context detached from the caller's, so a request the client
// cancelled still leaves the row failed instead of running.
const StatusWriteTimeout = 10 * time.Second

// StatusNotStoredError is a failure whose status couldn't be written: the
// row still reads as it did. Its text names the cause and the write.
type StatusNotStoredError struct {
	Cause error
	Err   error
	// what names the row and the status it couldn't get, such as
	// "experiment e-1 couldn't be marked failed".
	what string
}

func (e *StatusNotStoredError) Error() string {
	return fmt.Sprintf("%v; %s: %v", e.Cause, e.what, e.Err)
}

func (e *StatusNotStoredError) Unwrap() []error { return []error{e.Cause, e.Err} }

// StatusNotStored reports whether err is a failure whose status couldn't be
// written (a *StatusNotStoredError).
func StatusNotStored(err error) bool {
	var notStored *StatusNotStoredError
	return errors.As(err, &notStored)
}

// Fail stores cause on the experiment as FAILED and returns cause. When the
// status can't be written, it returns a *StatusNotStoredError naming both.
func (db *Experiment) Fail(ctx context.Context, id string, cause error) error {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), StatusWriteTimeout)
	defer cancel()
	if err := db.UpdateStatus(writeCtx, id, pb.Experiment_FAILED, cause); err != nil {
		return &StatusNotStoredError{Cause: cause, Err: err, what: fmt.Sprintf("experiment %s couldn't be marked failed", id)}
	}
	return cause
}

// Complete stores the experiment as COMPLETED on a context detached from
// ctx and bounded by StatusWriteTimeout, like Fail, so a client that went
// away after the run doesn't leave the row running.
func (db *Experiment) Complete(ctx context.Context, id string) error {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), StatusWriteTimeout)
	defer cancel()
	return db.UpdateStatus(writeCtx, id, pb.Experiment_COMPLETED, nil)
}

// SetExecutionID records the run's execution on the experiment. Like
// Complete, it writes on a context detached from ctx and bounded by
// StatusWriteTimeout, so a client that left during the run doesn't lose it.
func (db *Experiment) SetExecutionID(ctx context.Context, id, executionID string) error {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), StatusWriteTimeout)
	defer cancel()
	_, err := db.Conn.Exec(writeCtx,
		`UPDATE strategy_experiment SET execution_id=$2 WHERE id=$1`,
		id, executionID,
	)
	if err != nil {
		return fmt.Errorf("failed to set experiment execution id: %w", err)
	}

	return nil
}

func (db *Experiment) GetLastPeriod(ctx context.Context, executionID string) (*backtest.Period, error) {
	if executionID == "" {
		return nil, nil
	}

	var (
		periodDate            time.Time
		portfolioValue        float64
		maxDrawdown           float64
		sharpe                *float64
		algorithmPeriodReturn float64
		benchmarkPeriodReturn *float64
	)

	err := db.Conn.QueryRow(ctx, `
		SELECT date, portfolio_value, max_drawdown, sharpe, algorithm_period_return, benchmark_period_return
		FROM backtest_execution_period
		WHERE backtest_execution=$1
		ORDER BY date DESC
		LIMIT 1`, executionID,
	).Scan(&periodDate, &portfolioValue, &maxDrawdown, &sharpe, &algorithmPeriodReturn, &benchmarkPeriodReturn)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get last execution period: %w", err)
	}

	return &backtest.Period{
		Date:                  internal_pb.GoTimeToDate(periodDate),
		PortfolioValue:        portfolioValue,
		MaxDrawdown:           maxDrawdown,
		Sharpe:                sharpe,
		AlgorithmPeriodReturn: algorithmPeriodReturn,
		BenchmarkPeriodReturn: benchmarkPeriodReturn,
	}, nil
}

// ListByStrategyID lists experiments for one strategy after the caller was authorized.
func (db *Experiment) ListByStrategyID(ctx context.Context, strategyID, versionID, backtestID string) ([]*pb.Experiment, error) {
	if strategyID == "" {
		return nil, fmt.Errorf("strategy id is empty")
	}
	rows, err := db.Conn.Query(ctx, experimentSelectOwned+`
		WHERE e.strategy_id = $1
		AND ($2 = '' OR e.version_id = $2)
		AND ($3 = '' OR e.backtest_id = $3)
		ORDER BY e.created_at ASC`, strategyID, versionID, backtestID)
	if err != nil {
		return nil, fmt.Errorf("failed to list strategy experiments: %w", err)
	}
	defer rows.Close()
	return parseExperimentRows(rows)
}

// BacktestID is the backtest stored on the experiment. It is empty before the ownership migration.
func (db *Experiment) BacktestID(ctx context.Context, experimentID string) (string, error) {
	var id *string
	err := db.Conn.QueryRow(ctx, `SELECT backtest_id FROM strategy_experiment WHERE id = $1`, experimentID).Scan(&id)
	if err != nil {
		return "", err
	}
	if id == nil {
		return "", nil
	}
	return *id, nil
}

// CreateForBacktest inserts an experiment against one backtest id.
func (db *Experiment) CreateForBacktest(ctx context.Context, versionID, backtestID string) (*pb.Experiment, error) {
	if backtestID == "" {
		return nil, fmt.Errorf("backtest id is empty")
	}
	var id string
	err := db.Conn.QueryRow(ctx, `
		INSERT INTO strategy_experiment (strategy_id, version_id, backtest_id, status)
		SELECT strategy_id, id, $2, $3 FROM strategy_version WHERE id=$1
		RETURNING id`,
		versionID, backtestID, pb.Experiment_QUEUED,
	).Scan(&id)
	if err != nil {
		return nil, fmt.Errorf("failed to create strategy experiment: %w", err)
	}
	return db.Get(ctx, id)
}

const experimentSelectOwned = `
SELECT e.id, e.strategy_id, s.name, e.version_id, v.rev, b.name,
	e.execution_id, e.status, e.error, e.created_at, e.completed_at,
	p.portfolio_value, p.max_drawdown, p.sharpe, p.algorithm_period_return, p.benchmark_period_return
FROM strategy_experiment e
JOIN strategy s ON e.strategy_id = s.id
JOIN strategy_version v ON e.version_id = v.id
JOIN backtest b ON b.id = e.backtest_id
LEFT JOIN LATERAL (
	SELECT portfolio_value, max_drawdown, sharpe, algorithm_period_return, benchmark_period_return
	FROM backtest_execution_period
	WHERE backtest_execution = e.execution_id
	ORDER BY date DESC
	LIMIT 1
) p ON true`

const experimentSelect = `
SELECT e.id, e.strategy_id, s.name, e.version_id, v.rev, e.backtest_name,
	e.execution_id, e.status, e.error, e.created_at, e.completed_at,
	p.portfolio_value, p.max_drawdown, p.sharpe, p.algorithm_period_return, p.benchmark_period_return
FROM strategy_experiment e
JOIN strategy s ON e.strategy_id = s.id
JOIN strategy_version v ON e.version_id = v.id
LEFT JOIN LATERAL (
	SELECT portfolio_value, max_drawdown, sharpe, algorithm_period_return, benchmark_period_return
	FROM backtest_execution_period
	WHERE backtest_execution = e.execution_id
	ORDER BY date DESC
	LIMIT 1
) p ON true`

func (db *Experiment) scanOne(ctx context.Context, query string, args ...interface{}) (*pb.Experiment, error) {
	e := pb.Experiment{}
	var executionID *string
	var errText *string
	var createdAt time.Time
	var completedAt *time.Time
	var portfolioValue *float64
	var maxDrawdown *float64
	var sharpe *float64
	var algorithmPeriodReturn *float64
	var benchmarkPeriodReturn *float64

	err := db.Conn.QueryRow(ctx, query, args...).Scan(
		&e.Id, &e.StrategyId, &e.StrategyName, &e.VersionId, &e.VersionRev, &e.BacktestName,
		&executionID, &e.Status, &errText, &createdAt, &completedAt,
		&portfolioValue, &maxDrawdown, &sharpe, &algorithmPeriodReturn, &benchmarkPeriodReturn,
	)
	if err != nil {
		return nil, err
	}

	fillExperiment(&e, executionID, errText, createdAt, completedAt,
		experimentStats(portfolioValue, maxDrawdown, sharpe, algorithmPeriodReturn, benchmarkPeriodReturn))

	return &e, nil
}

func parseExperimentRows(rows pgx.Rows) ([]*pb.Experiment, error) {
	experiments := []*pb.Experiment{}

	for rows.Next() {
		e := pb.Experiment{}
		var executionID *string
		var errText *string
		var createdAt time.Time
		var completedAt *time.Time
		var portfolioValue *float64
		var maxDrawdown *float64
		var sharpe *float64
		var algorithmPeriodReturn *float64
		var benchmarkPeriodReturn *float64

		err := rows.Scan(
			&e.Id, &e.StrategyId, &e.StrategyName, &e.VersionId, &e.VersionRev, &e.BacktestName,
			&executionID, &e.Status, &errText, &createdAt, &completedAt,
			&portfolioValue, &maxDrawdown, &sharpe, &algorithmPeriodReturn, &benchmarkPeriodReturn,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan strategy experiment: %w", err)
		}

		fillExperiment(&e, executionID, errText, createdAt, completedAt,
			experimentStats(portfolioValue, maxDrawdown, sharpe, algorithmPeriodReturn, benchmarkPeriodReturn))
		experiments = append(experiments, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read strategy experiments: %w", err)
	}

	return experiments, nil
}

func fillExperiment(e *pb.Experiment, executionID, errText *string, createdAt time.Time, completedAt *time.Time, stats *pb.ExperimentStats) {
	e.ExecutionId = executionID
	e.Error = errText
	e.CreatedAt = internal_pb.TimeToProtoTimestamp(createdAt)
	if completedAt != nil {
		e.CompletedAt = internal_pb.TimeToProtoTimestamp(*completedAt)
	}
	e.Stats = stats
}

func experimentStats(portfolioValue, maxDrawdown, sharpe, algorithmPeriodReturn, benchmarkPeriodReturn *float64) *pb.ExperimentStats {
	if portfolioValue == nil && maxDrawdown == nil && algorithmPeriodReturn == nil {
		return nil
	}

	stats := &pb.ExperimentStats{
		Sharpe:                sharpe,
		BenchmarkPeriodReturn: benchmarkPeriodReturn,
	}
	if portfolioValue != nil {
		stats.PortfolioValue = *portfolioValue
	}
	if maxDrawdown != nil {
		stats.MaxDrawdown = *maxDrawdown
	}
	if algorithmPeriodReturn != nil {
		stats.AlgorithmPeriodReturn = *algorithmPeriodReturn
	}

	return stats
}
