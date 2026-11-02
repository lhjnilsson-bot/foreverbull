package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/lhjnilsson/foreverbull/internal/auth"
	"github.com/lhjnilsson/foreverbull/internal/postgres"
	internal_pb "github.com/lhjnilsson/foreverbull/pkg/pb"
	"github.com/lhjnilsson/foreverbull/pkg/pb/backtest"
)

const ExecutionTable = `CREATE TABLE IF NOT EXISTS backtest_execution (
id text PRIMARY KEY DEFAULT uuid_generate_v4 (),
session text REFERENCES session(id) ON DELETE CASCADE,
start_date date NOT NULL,
end_date date NOT NULL,
function_arguments jsonb,
provider text NOT NULL,
notices text[],
CONSTRAINT backtest_execution_provider_check CHECK (provider IN ('yahoo', 'stooq', 'alpaca')));

ALTER TABLE backtest_execution ADD COLUMN IF NOT EXISTS function_arguments jsonb;
ALTER TABLE backtest_execution ADD COLUMN IF NOT EXISTS notices text[];


CREATE TABLE IF NOT EXISTS backtest_execution_period (
	id serial primary key,
	backtest_execution text not null,

	date date not null,
	pnl numeric  not null,
	returns numeric  not null,
	portfolio_value numeric  not null,

	longs_count integer,
	shorts_count integer,
	long_value numeric  not null,
	short_value numeric  not null,
	starting_exposure numeric  not null,
	ending_exposure numeric  not null,
	long_exposure numeric  not null,
	short_exposure numeric  not null,

	capital_used numeric  not null,
	gross_leverage numeric  not null,
	net_leverage numeric  not null,

	starting_value numeric  not null,
	ending_value numeric  not null,
	starting_cash numeric  not null,
	ending_cash numeric  not null,

	max_drawdown numeric  not null,
	max_leverage numeric  not null,
	excess_returns numeric  not null,
	treasury_period_return numeric  not null,
	algorithm_period_return numeric  not null,

	algo_volatility numeric,
	sharpe numeric,
	sortino numeric,

	benchmark_period_return numeric,
	benchmark_volatility numeric,
	alpha numeric,
	beta numeric,
	CONSTRAINT backtest_execution_period_backtest_execution_fkey
		FOREIGN KEY (backtest_execution) REFERENCES backtest_execution (id) ON DELETE CASCADE
);

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conname = 'unique_backtest_execution_period'
        AND conrelid = 'backtest_execution_period'::regclass
    ) THEN
        ALTER TABLE backtest_execution_period ADD CONSTRAINT unique_backtest_execution_period UNIQUE(backtest_execution, date);
    END IF;
END $$;
`

const executionProviderCheck = "backtest_execution_provider_check"

const executionPeriodForeignKey = "backtest_execution_period_backtest_execution_fkey"

// MigrateExecutionProvider backfills provider from the backtest, then yahoo
// for any row the join cannot fill, then sets NOT NULL and the provider check.
// The column has no default afterwards, so a new row must name its provider.
// A row that would still fail the check stays. Calling it twice is safe.
// The column is added nullable so an existing row is not stamped yahoo
// before the backfill runs.
func MigrateExecutionProvider(ctx context.Context, conn postgres.Query) error {
	hasBacktest, err := auth.ColumnExists(ctx, conn, "session", "backtest")
	if err != nil {
		return err
	}
	providerNotNull, err := auth.ColumnNotNull(ctx, conn, "backtest_execution", "provider")
	if err != nil {
		return err
	}
	checkExists, err := tableConstraintExists(ctx, conn, "backtest_execution", executionProviderCheck)
	if err != nil {
		return err
	}
	// The name join is gone after the ownership migration. A finished provider
	// column has nothing left to backfill.
	if !hasBacktest && providerNotNull && checkExists {
		return nil
	}

	_, err = conn.Exec(ctx, `ALTER TABLE backtest_execution ADD COLUMN IF NOT EXISTS provider text`)
	if err != nil {
		return fmt.Errorf("backtest_execution: provider column: %w", err)
	}

	if hasBacktest {
		_, err = conn.Exec(ctx, `
			UPDATE backtest_execution e
			SET provider = b.provider
			FROM session s
			JOIN backtest b ON b.name = s.backtest
			WHERE e.session = s.id AND e.provider IS NULL`)
		if err != nil {
			return fmt.Errorf("backtest_execution: backfill provider: %w", err)
		}
	}

	_, err = conn.Exec(ctx, `
		UPDATE backtest_execution
		SET provider = 'yahoo'
		WHERE provider IS NULL`)
	if err != nil {
		return fmt.Errorf("backtest_execution: default provider: %w", err)
	}

	var bad bool
	err = conn.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM backtest_execution
			WHERE provider IS NULL
			   OR provider NOT IN ('yahoo', 'stooq', 'alpaca')
		)`).Scan(&bad)
	if err != nil {
		return fmt.Errorf("backtest_execution: scan provider: %w", err)
	}
	if bad {
		return fmt.Errorf("backtest_execution: rows violate provider IN ('yahoo', 'stooq', 'alpaca')")
	}

	_, err = conn.Exec(ctx, `ALTER TABLE backtest_execution ALTER COLUMN provider DROP DEFAULT`)
	if err != nil {
		return fmt.Errorf("backtest_execution: provider default: %w", err)
	}
	_, err = conn.Exec(ctx, `ALTER TABLE backtest_execution ALTER COLUMN provider SET NOT NULL`)
	if err != nil {
		return fmt.Errorf("backtest_execution: provider not null: %w", err)
	}

	exists, err := tableConstraintExists(ctx, conn, "backtest_execution", executionProviderCheck)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	_, err = conn.Exec(ctx, `
		ALTER TABLE backtest_execution
			ADD CONSTRAINT backtest_execution_provider_check
			CHECK (provider IN ('yahoo', 'stooq', 'alpaca'))`)
	if err != nil {
		return fmt.Errorf("backtest_execution: provider check: %w", err)
	}
	return nil
}

// MigrateExecutionPeriod references backtest_execution_period.backtest_execution
// to backtest_execution(id) ON DELETE CASCADE.
// The unique key on (backtest_execution, date) stays. An orphan row stays.
// Calling it twice is safe.
func MigrateExecutionPeriod(ctx context.Context, conn postgres.Query) error {
	exists, err := tableConstraintExists(ctx, conn, "backtest_execution_period", executionPeriodForeignKey)
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
			FROM backtest_execution_period p
			LEFT JOIN backtest_execution e ON e.id = p.backtest_execution
			WHERE e.id IS NULL
		)`).Scan(&bad)
	if err != nil {
		return fmt.Errorf("backtest_execution_period: scan foreign key: %w", err)
	}
	if bad {
		return fmt.Errorf("backtest_execution_period: rows violate foreign key backtest_execution references backtest_execution(id)")
	}

	_, err = conn.Exec(ctx, `
		ALTER TABLE backtest_execution_period
			ADD CONSTRAINT backtest_execution_period_backtest_execution_fkey
			FOREIGN KEY (backtest_execution) REFERENCES backtest_execution (id) ON DELETE CASCADE`)
	if err != nil {
		return fmt.Errorf("backtest_execution_period: foreign key: %w", err)
	}
	return nil
}

type Execution struct {
	Conn postgres.Query
}

func (db *Execution) Create(ctx context.Context, execution *backtest.Execution, functions []*backtest.ExecutionFunction) (*backtest.Execution, error) {
	var executionID string

	// nil functions leaves the column null so a row matches runs stored before arguments existed.
	// A non-nil slice, including empty, is what CreateExecution persists.
	var arguments []byte
	if functions != nil {
		var err error
		arguments, err = marshalFunctionArguments(functions)
		if err != nil {
			return nil, err
		}
	}

	// provider is copied from the backtest. The column has no default, so a missing backtest fails here.
	// After the ownership migration the join is backtest id, not the name.
	join := "JOIN backtest b ON b.name = s.backtest"
	if authOn() {
		join = "JOIN backtest b ON b.id = s.backtest_id"
	}
	err := db.Conn.QueryRow(ctx,
		`INSERT INTO backtest_execution (session, start_date, end_date, function_arguments, provider)
		VALUES ($1, $2, $3, $4, (
			SELECT b.provider
			FROM session s
			`+join+`
			WHERE s.id = $1
		)) RETURNING id`,
		execution.Session,
		internal_pb.DateToDateString(execution.StartDate),
		internal_pb.DateToDateString(execution.EndDate),
		arguments,
	).Scan(&executionID)
	if err != nil {
		return nil, fmt.Errorf("failed to create execution: %w", err)
	}

	return db.Get(ctx, executionID)
}

// GetFunctions returns the worker arguments stored at create time.
// A null column (runs created before the column existed) is an empty list, not an error and not a default algorithm.
func (db *Execution) GetFunctions(ctx context.Context, executionID string) ([]*backtest.ExecutionFunction, error) {
	var raw []byte
	err := db.Conn.QueryRow(ctx,
		`SELECT function_arguments FROM backtest_execution WHERE id=$1`, executionID).Scan(&raw)
	if err != nil {
		return nil, fmt.Errorf("failed to get execution functions: %w", err)
	}

	functions, err := unmarshalFunctionArguments(raw)
	if err != nil {
		return nil, err
	}

	return functions, nil
}

// GetProvider returns the price provider stored on the execution at create time.
// Empty string means a legacy row before the column existed (caller may treat as unknown).
func (db *Execution) GetProvider(ctx context.Context, executionID string) (string, error) {
	var provider sql.NullString
	err := db.Conn.QueryRow(ctx,
		`SELECT provider FROM backtest_execution WHERE id=$1`, executionID).Scan(&provider)
	if err != nil {
		return "", fmt.Errorf("failed to get execution provider: %w", err)
	}
	if !provider.Valid {
		return "", nil
	}
	return provider.String, nil
}

// SetNotices stores the lines the run summary shows, such as members left
// out after their last price. An empty list clears them.
func (db *Execution) SetNotices(ctx context.Context, executionID string, notices []string) error {
	if notices == nil {
		notices = []string{}
	}
	tag, err := db.Conn.Exec(ctx,
		`UPDATE backtest_execution SET notices=$2 WHERE id=$1`, executionID, notices)
	if err != nil {
		return fmt.Errorf("failed to store execution notices: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("failed to store execution notices: %w", pgx.ErrNoRows)
	}
	return nil
}

// GetNotices returns the stored run notices. A run stored before notices
// existed, or one with nothing to report, returns an empty list.
func (db *Execution) GetNotices(ctx context.Context, executionID string) ([]string, error) {
	var notices []string
	err := db.Conn.QueryRow(ctx,
		`SELECT COALESCE(notices, '{}') FROM backtest_execution WHERE id=$1`, executionID).Scan(&notices)
	if err != nil {
		return nil, fmt.Errorf("failed to get execution notices: %w", err)
	}
	if notices == nil {
		return []string{}, nil
	}
	return notices, nil
}

func (db *Execution) StorePeriods(ctx context.Context, execution string, periods []*backtest.Period) error {
	for _, period := range periods {
		_, err := db.Conn.Exec(ctx,
			`INSERT INTO backtest_execution_period(
				backtest_execution, date, pnl, returns, portfolio_value,
				longs_count, shorts_count, long_value, short_value, starting_exposure, ending_exposure, long_exposure, short_exposure,
				capital_used, gross_leverage, net_leverage,
				starting_value, ending_value, starting_cash, ending_cash,
				max_drawdown, max_leverage, excess_returns, treasury_period_return, algorithm_period_return,
				algo_volatility, sharpe, sortino,
				benchmark_period_return, benchmark_volatility, alpha, beta)
			VALUES($1, $2, $3, $4, $5,
				$6, $7, $8, $9, $10, $11, $12, $13,
				$14, $15, $16,
				$17, $18, $19, $20,
				$21, $22, $23, $24, $25,
				$26, $27, $28,
				$29, $30, $31, $32)`,
			execution, internal_pb.DateToDateString(period.Date), period.PNL, period.Returns, period.PortfolioValue,
			period.LongsCount, period.ShortsCount, period.LongValue, period.ShortValue, period.StartingExposure, period.EndingExposure, period.LongExposure, period.ShortExposure,
			period.CapitalUsed, period.GrossLeverage, period.NetLeverage,
			period.StartingValue, period.EndingValue, period.StartingCash, period.EndingCash,
			period.MaxDrawdown, period.MaxLeverage, period.ExcessReturn, period.TreasuryPeriodReturn, period.AlgorithmPeriodReturn,
			period.AlgoVolatility, period.Sharpe, period.Sortino,
			period.BenchmarkPeriodReturn, period.BenchmarkVolatility, period.Alpha, period.Beta,
		)
		if err != nil {
			return fmt.Errorf("error creating period result: %w", err)
		}
	}

	return nil
}

func (db *Execution) Get(ctx context.Context, executionId string) (*backtest.Execution, error) {
	if authOn() {
		return db.getJoined(ctx, executionId)
	}
	execution := backtest.Execution{}
	start := time.Time{}
	end := pgtype.Date{}

	err := db.Conn.QueryRow(ctx,
		`SELECT backtest_execution.id, session.backtest, backtest_execution.session, start_date, end_date  
		FROM backtest_execution
		INNER JOIN session ON backtest_execution.session=session.id
		WHERE backtest_execution.id=$1`, executionId).Scan(
		&execution.Id, &execution.Backtest, &execution.Session, &start, &end,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get execution: %w", err)
	}

	execution.StartDate = internal_pb.GoTimeToDate(start)
	if end.Valid {
		execution.EndDate = internal_pb.GoTimeToDate(end.Time)
	}

	return &execution, nil
}

func (db *Execution) GetPeriods(ctx context.Context, executionId string) ([]*backtest.Period, error) {
	rows, err := db.Conn.Query(ctx,
		`SELECT date, pnl, returns, portfolio_value, longs_count, shorts_count,
		long_value, short_value, starting_exposure, ending_exposure, long_exposure, short_exposure, capital_used, gross_leverage,
		net_leverage, starting_value, ending_value, starting_cash, ending_cash, max_drawdown,
		max_leverage, excess_returns, treasury_period_return, algorithm_period_return,
		algo_volatility, sharpe, sortino, benchmark_period_return, benchmark_volatility,
		alpha, beta FROM backtest_execution_period WHERE backtest_execution=$1 ORDER BY DATE ASC`, executionId)
	if err != nil {
		return nil, fmt.Errorf("failed to get periods: %w", err)
	}

	periods := make([]*backtest.Period, 0)
	defer rows.Close()
	for rows.Next() {
		period := backtest.Period{}
		periodDate := pgtype.Date{}
		pnl := sql.NullFloat64{}
		returns := sql.NullFloat64{}
		portfolioValue := sql.NullFloat64{}
		longsCount := sql.NullInt32{}
		shortsCount := sql.NullInt32{}
		longValue := sql.NullFloat64{}
		shortValue := sql.NullFloat64{}
		startingExposure := sql.NullFloat64{}
		endingExposure := sql.NullFloat64{}
		longExposure := sql.NullFloat64{}
		shortExposure := sql.NullFloat64{}
		capitalUsed := sql.NullFloat64{}
		grossLeverage := sql.NullFloat64{}
		netLeverage := sql.NullFloat64{}
		startingValue := sql.NullFloat64{}
		endingValue := sql.NullFloat64{}
		startingCash := sql.NullFloat64{}
		endingCash := sql.NullFloat64{}
		maxDrawdown := sql.NullFloat64{}
		maxLeverage := sql.NullFloat64{}
		excessReturn := sql.NullFloat64{}
		treasuryPeriodReturn := sql.NullFloat64{}
		algorithmPeriodReturn := sql.NullFloat64{}
		algoVolatility := sql.NullFloat64{}
		sharpe := sql.NullFloat64{}
		sortino := sql.NullFloat64{}
		benchmarkPeriodReturn := sql.NullFloat64{}
		benchmarkVolatility := sql.NullFloat64{}
		alpha := sql.NullFloat64{}
		beta := sql.NullFloat64{}

		err = rows.Scan(&periodDate, &pnl, &returns, &portfolioValue, &longsCount, &shortsCount,
			&longValue, &shortValue, &startingExposure, &endingExposure, &longExposure, &shortExposure, &capitalUsed, &grossLeverage,
			&netLeverage, &startingValue, &endingValue, &startingCash, &endingCash, &maxDrawdown,
			&maxLeverage, &excessReturn, &treasuryPeriodReturn, &algorithmPeriodReturn,
			&algoVolatility, &sharpe, &sortino, &benchmarkPeriodReturn, &benchmarkVolatility,
			&alpha, &beta)
		if err != nil {
			return nil, fmt.Errorf("failed to scan period: %w", err)
		}
		period.Date = internal_pb.GoTimeToDate(periodDate.Time)
		if pnl.Valid {
			period.PNL = pnl.Float64
		}
		if returns.Valid {
			period.Returns = returns.Float64
		}
		if portfolioValue.Valid {
			period.PortfolioValue = portfolioValue.Float64
		}
		if longsCount.Valid {
			period.LongsCount = longsCount.Int32
		}
		if shortsCount.Valid {
			period.ShortsCount = shortsCount.Int32
		}
		if longValue.Valid {
			period.LongValue = longValue.Float64
		}
		if shortValue.Valid {
			period.ShortValue = shortValue.Float64
		}
		if startingExposure.Valid {
			period.StartingExposure = startingExposure.Float64
		}
		if endingExposure.Valid {
			period.EndingExposure = endingExposure.Float64
		}
		if longExposure.Valid {
			period.LongExposure = longExposure.Float64
		}
		if shortExposure.Valid {
			period.ShortExposure = shortExposure.Float64
		}
		if capitalUsed.Valid {
			period.CapitalUsed = capitalUsed.Float64
		}
		if grossLeverage.Valid {
			period.GrossLeverage = grossLeverage.Float64
		}
		if netLeverage.Valid {
			period.NetLeverage = netLeverage.Float64
		}
		if startingValue.Valid {
			period.StartingValue = startingValue.Float64
		}
		if endingValue.Valid {
			period.EndingValue = endingValue.Float64
		}
		if startingCash.Valid {
			period.StartingCash = startingCash.Float64
		}
		if endingCash.Valid {
			period.EndingCash = endingCash.Float64
		}
		if maxDrawdown.Valid {
			period.MaxDrawdown = maxDrawdown.Float64
		}
		if maxLeverage.Valid {
			period.MaxLeverage = maxLeverage.Float64
		}
		if excessReturn.Valid {
			period.ExcessReturn = excessReturn.Float64
		}
		if treasuryPeriodReturn.Valid {
			period.TreasuryPeriodReturn = treasuryPeriodReturn.Float64
		}
		if algorithmPeriodReturn.Valid {
			period.AlgorithmPeriodReturn = algorithmPeriodReturn.Float64
		}
		if algoVolatility.Valid {
			period.AlgoVolatility = &algoVolatility.Float64
		}
		if sharpe.Valid {
			period.Sharpe = &sharpe.Float64
		}
		if sortino.Valid {
			period.Sortino = &sortino.Float64
		}
		if benchmarkPeriodReturn.Valid {
			period.BenchmarkPeriodReturn = &benchmarkPeriodReturn.Float64
		}
		if benchmarkVolatility.Valid {
			period.BenchmarkVolatility = &benchmarkVolatility.Float64
		}
		if alpha.Valid {
			period.Alpha = &alpha.Float64
		}
		if beta.Valid {
			period.Beta = &beta.Float64
		}

		periods = append(periods, &period)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read execution periods: %w", err)
	}

	return periods, nil
}

func (db *Execution) UpdateSimulationDetails(ctx context.Context, e *backtest.Execution) error {
	_, err := db.Conn.Exec(ctx,
		`UPDATE backtest_execution SET start_date=$1, end_date=$2 WHERE id=$3`, internal_pb.DateToDateString(e.StartDate),
		internal_pb.DateToDateString(e.EndDate), e.Id)
	if err != nil {
		return fmt.Errorf("failed to update execution: %w", err)
	}

	return nil
}

func (db *Execution) parseRows(rows pgx.Rows) ([]*backtest.Execution, error) {
	executions := make([]*backtest.Execution, 0)

	var err error
	for rows.Next() {
		execution := backtest.Execution{}
		result := backtest.Period{}
		resultDate := pgtype.Date{}
		start := time.Time{}
		end := pgtype.Date{}

		pnl := sql.NullFloat64{}
		returns := sql.NullFloat64{}
		portfolioValue := sql.NullFloat64{}
		longsCount := sql.NullInt32{}
		shortsCount := sql.NullInt32{}
		longValue := sql.NullFloat64{}
		shortValue := sql.NullFloat64{}
		startingExposure := sql.NullFloat64{}
		endingExposure := sql.NullFloat64{}
		longExposure := sql.NullFloat64{}
		shortExposure := sql.NullFloat64{}
		capitalUsed := sql.NullFloat64{}
		grossLeverage := sql.NullFloat64{}
		netLeverage := sql.NullFloat64{}
		startingValue := sql.NullFloat64{}
		endingValue := sql.NullFloat64{}
		startingCash := sql.NullFloat64{}
		endingCash := sql.NullFloat64{}
		maxDrawdown := sql.NullFloat64{}
		maxLeverage := sql.NullFloat64{}
		excessReturn := sql.NullFloat64{}
		treasuryPeriodReturn := sql.NullFloat64{}
		algorithmPeriodReturn := sql.NullFloat64{}
		algoVolatility := sql.NullFloat64{}
		sharpe := sql.NullFloat64{}
		sortino := sql.NullFloat64{}
		benchmarkPeriodReturn := sql.NullFloat64{}
		benchmarkVolatility := sql.NullFloat64{}
		alpha := sql.NullFloat64{}
		beta := sql.NullFloat64{}

		err = rows.Scan(&execution.Id, &execution.Session, &execution.Backtest, &start, &end,
			&resultDate, &pnl, &returns, &portfolioValue, &longsCount, &shortsCount,
			&longValue, &shortValue, &startingExposure, &endingExposure, &longExposure,
			&shortExposure, &capitalUsed, &grossLeverage, &netLeverage, &startingValue,
			&endingValue, &startingCash, &endingCash, &maxDrawdown, &maxLeverage,
			&excessReturn, &treasuryPeriodReturn, &algorithmPeriodReturn, &algoVolatility,
			&sharpe, &sortino, &benchmarkPeriodReturn, &benchmarkVolatility, &alpha, &beta)
		if err != nil {
			return nil, fmt.Errorf("failed to scan execution: %w", err)
		}

		execution.StartDate = internal_pb.GoTimeToDate(start)
		if end.Valid {
			execution.EndDate = internal_pb.GoTimeToDate(end.Time)
		}

		result.Date = internal_pb.GoTimeToDate(resultDate.Time)
		if pnl.Valid {
			result.PNL = pnl.Float64
		}
		if returns.Valid {
			result.Returns = returns.Float64
		}
		if portfolioValue.Valid {
			result.PortfolioValue = portfolioValue.Float64
		}
		if longsCount.Valid {
			result.LongsCount = longsCount.Int32
		}
		if shortsCount.Valid {
			result.ShortsCount = shortsCount.Int32
		}
		if longValue.Valid {
			result.LongValue = longValue.Float64
		}
		if shortValue.Valid {
			result.ShortValue = shortValue.Float64
		}
		if startingExposure.Valid {
			result.StartingExposure = startingExposure.Float64
		}
		if endingExposure.Valid {
			result.EndingExposure = endingExposure.Float64
		}
		if longExposure.Valid {
			result.LongExposure = longExposure.Float64
		}
		if shortExposure.Valid {
			result.ShortExposure = shortExposure.Float64
		}
		if capitalUsed.Valid {
			result.CapitalUsed = capitalUsed.Float64
		}
		if grossLeverage.Valid {
			result.GrossLeverage = grossLeverage.Float64
		}
		if netLeverage.Valid {
			result.NetLeverage = netLeverage.Float64
		}
		if startingValue.Valid {
			result.StartingValue = startingValue.Float64
		}
		if endingValue.Valid {
			result.EndingValue = endingValue.Float64
		}
		if startingCash.Valid {
			result.StartingCash = startingCash.Float64
		}
		if endingCash.Valid {
			result.EndingCash = endingCash.Float64
		}
		if maxDrawdown.Valid {
			result.MaxDrawdown = maxDrawdown.Float64
		}
		if maxLeverage.Valid {
			result.MaxLeverage = maxLeverage.Float64
		}
		if excessReturn.Valid {
			result.ExcessReturn = excessReturn.Float64
		}
		if treasuryPeriodReturn.Valid {
			result.TreasuryPeriodReturn = treasuryPeriodReturn.Float64
		}
		if algorithmPeriodReturn.Valid {
			result.AlgorithmPeriodReturn = algorithmPeriodReturn.Float64
		}
		if algoVolatility.Valid {
			result.AlgoVolatility = &algoVolatility.Float64
		}
		if sharpe.Valid {
			result.Sharpe = &sharpe.Float64
		}
		if sortino.Valid {
			result.Sortino = &sortino.Float64
		}
		if benchmarkPeriodReturn.Valid {
			result.BenchmarkPeriodReturn = &benchmarkPeriodReturn.Float64
		}
		if benchmarkVolatility.Valid {
			result.BenchmarkVolatility = &benchmarkVolatility.Float64
		}
		if alpha.Valid {
			result.Alpha = &alpha.Float64
		}
		if beta.Valid {
			result.Beta = &beta.Float64
		}
		execution.Result = &result
		executions = append(executions, &execution)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read executions: %w", err)
	}

	return executions, nil
}

func (db *Execution) List(ctx context.Context) ([]*backtest.Execution, error) {
	if authOn() {
		return nil, fmt.Errorf("list executions is refused while %s=true", "AUTH_ENABLED")
	}
	rows, err := db.Conn.Query(ctx,
		`SELECT backtest_execution.id, session.id, session.backtest, backtest_execution.start_date, backtest_execution.end_date,
		ep.date, ep.pnl, ep.returns, ep.portfolio_value, ep.longs_count, ep.shorts_count,
		ep.long_value, ep.short_value, ep.starting_exposure, ep.ending_exposure, ep.long_exposure, ep.short_exposure,
		ep.capital_used, ep.gross_leverage, ep.net_leverage,
		ep.starting_value, ep.ending_value, ep.starting_cash, ep.ending_cash,
		ep.max_drawdown, ep.max_leverage, ep.excess_returns, ep.treasury_period_return, ep.algorithm_period_return,
		ep.algo_volatility, ep.sharpe, ep.sortino,
		ep.benchmark_period_return, ep.benchmark_volatility, ep.alpha, ep.beta
		FROM backtest_execution INNER JOIN session ON backtest_execution.session=session.id
		LEFT JOIN (
			SELECT backtest_execution, date, pnl, returns, portfolio_value, longs_count, shorts_count,
			long_value, short_value, starting_exposure, ending_exposure, long_exposure, short_exposure, capital_used, gross_leverage,
			net_leverage, starting_value, ending_value, starting_cash, ending_cash, max_drawdown,
			max_leverage, excess_returns, treasury_period_return, algorithm_period_return,
			algo_volatility, sharpe, sortino, benchmark_period_return, benchmark_volatility,
			alpha, beta FROM backtest_execution_period ORDER BY date DESC LIMIT 1
		) as ep ON backtest_execution.id=ep.backtest_execution`)
	if err != nil {
		return nil, fmt.Errorf("failed to list executions: %w", err)
	}

	defer rows.Close()

	return db.parseRows(rows)
}

func (db *Execution) ListBySession(ctx context.Context, session string) ([]*backtest.Execution, error) {
	if authOn() {
		return db.listJoined(ctx, "WHERE session.id = $1", []any{session})
	}
	rows, err := db.Conn.Query(ctx,
		`SELECT backtest_execution.id, session.id, session.backtest, backtest_execution.start_date, backtest_execution.end_date,
		ep.date, ep.pnl, ep.returns, ep.portfolio_value, ep.longs_count, ep.shorts_count,
		ep.long_value, ep.short_value, ep.starting_exposure, ep.ending_exposure, ep.long_exposure, ep.short_exposure,
		ep.capital_used, ep.gross_leverage, ep.net_leverage,
		ep.starting_value, ep.ending_value, ep.starting_cash, ep.ending_cash,
		ep.max_drawdown, ep.max_leverage, ep.excess_returns, ep.treasury_period_return, ep.algorithm_period_return,
		ep.algo_volatility, ep.sharpe, ep.sortino,
		ep.benchmark_period_return, ep.benchmark_volatility, ep.alpha, ep.beta
		FROM backtest_execution
		INNER JOIN session ON backtest_execution.session=session.id
		LEFT JOIN (
			SELECT backtest_execution, date, pnl, returns, portfolio_value, longs_count, shorts_count,
			long_value, short_value, starting_exposure, ending_exposure, long_exposure, short_exposure, capital_used, gross_leverage,
			net_leverage, starting_value, ending_value, starting_cash, ending_cash, max_drawdown,
			max_leverage, excess_returns, treasury_period_return, algorithm_period_return,
			algo_volatility, sharpe, sortino, benchmark_period_return, benchmark_volatility,
			alpha, beta FROM backtest_execution_period ORDER BY date DESC LIMIT 1
		) as ep ON backtest_execution.id=ep.backtest_execution
		WHERE session=$1`, session)
	if err != nil {
		return nil, fmt.Errorf("failed to list executions: %w", err)
	}

	defer rows.Close()

	return db.parseRows(rows)
}

func (db *Execution) ListByBacktest(ctx context.Context, backtest string) ([]*backtest.Execution, error) {
	if authOn() {
		return nil, fmt.Errorf("list executions by name is refused while %s=true", "AUTH_ENABLED")
	}
	rows, err := db.Conn.Query(ctx,
		`SELECT backtest_execution.id, session.id, session.backtest, backtest_execution.start_date, backtest_execution.end_date,
		ep.date, ep.pnl, ep.returns, ep.portfolio_value, ep.longs_count, ep.shorts_count,
		ep.long_value, ep.short_value, ep.starting_exposure, ep.ending_exposure, ep.long_exposure, ep.short_exposure,
		ep.capital_used, ep.gross_leverage, ep.net_leverage,
		ep.starting_value, ep.ending_value, ep.starting_cash, ep.ending_cash,
		ep.max_drawdown, ep.max_leverage, ep.excess_returns, ep.treasury_period_return, ep.algorithm_period_return,
		ep.algo_volatility, ep.sharpe, ep.sortino,
		ep.benchmark_period_return, ep.benchmark_volatility, ep.alpha, ep.beta
		FROM backtest_execution
		LEFT JOIN (
			SELECT backtest_execution, date, pnl, returns, portfolio_value, longs_count, shorts_count,
			long_value, short_value, starting_exposure, ending_exposure, long_exposure, short_exposure, capital_used, gross_leverage,
			net_leverage, starting_value, ending_value, starting_cash, ending_cash, max_drawdown,
			max_leverage, excess_returns, treasury_period_return, algorithm_period_return,
			algo_volatility, sharpe, sortino, benchmark_period_return, benchmark_volatility,
			alpha, beta FROM backtest_execution_period ORDER BY date DESC LIMIT 1
		) as ep ON backtest_execution.id=ep.backtest_execution
		INNER JOIN session ON backtest_execution.session=session.id
		INNER JOIN backtest ON session.backtest=backtest.name
		WHERE backtest.name=$1`, backtest)
	if err != nil {
		return nil, fmt.Errorf("failed to list executions: %w", err)
	}

	defer rows.Close()

	return db.parseRows(rows)
}

type storedParameter struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	ValueType string `json:"value_type"`
}

type storedFunction struct {
	Name       string            `json:"name"`
	Parameters []storedParameter `json:"parameters"`
}

func marshalFunctionArguments(functions []*backtest.ExecutionFunction) ([]byte, error) {
	stored := make([]storedFunction, 0, len(functions))
	for _, fn := range functions {
		if fn == nil {
			continue
		}
		params := make([]storedParameter, 0, len(fn.GetParameters()))
		for _, p := range fn.GetParameters() {
			if p == nil {
				continue
			}
			params = append(params, storedParameter{
				Key:       p.GetKey(),
				Value:     p.GetValue(),
				ValueType: p.GetValueType(),
			})
		}
		stored = append(stored, storedFunction{Name: fn.GetName(), Parameters: params})
	}

	encoded, err := json.Marshal(stored)
	if err != nil {
		return nil, fmt.Errorf("failed to encode execution functions: %w", err)
	}

	return encoded, nil
}

func unmarshalFunctionArguments(raw []byte) ([]*backtest.ExecutionFunction, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return []*backtest.ExecutionFunction{}, nil
	}

	var stored []storedFunction
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, fmt.Errorf("failed to decode execution functions: %w", err)
	}

	functions := make([]*backtest.ExecutionFunction, 0, len(stored))
	for _, fn := range stored {
		params := make([]*backtest.ExecutionFunction_Parameter, 0, len(fn.Parameters))
		for _, p := range fn.Parameters {
			params = append(params, &backtest.ExecutionFunction_Parameter{
				Key:       p.Key,
				Value:     p.Value,
				ValueType: p.ValueType,
			})
		}
		functions = append(functions, &backtest.ExecutionFunction{
			Name:       fn.Name,
			Parameters: params,
		})
	}

	return functions, nil
}
