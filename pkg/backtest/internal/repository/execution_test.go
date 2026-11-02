package repository_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/internal/test_helper"
	"github.com/lhjnilsson/foreverbull/pkg/backtest/internal/repository"
	"github.com/lhjnilsson/foreverbull/pkg/finance/supplier"
	"github.com/lhjnilsson/foreverbull/pkg/pb"
	"github.com/lhjnilsson/foreverbull/pkg/pb/backtest"
	"github.com/stretchr/testify/suite"
	"google.golang.org/protobuf/proto"
)

type ExecutionTest struct {
	suite.Suite

	conn *pgxpool.Pool

	storedBacktest *backtest.Backtest
	storedSession  *backtest.Session
	execution      *backtest.Execution

	uut repository.Execution
}

func (test *ExecutionTest) SetupSuite() {
	test_helper.SetupEnvironment(test.T(), &test_helper.Containers{
		Postgres: true,
	})
}

func (test *ExecutionTest) SetupTest() {
	var err error
	test.conn, err = pgxpool.New(context.Background(), environment.GetPostgresURL())
	test.Require().NoError(err)

	err = repository.Recreate(context.Background(), test.conn)
	test.Require().NoError(err)

	ctx := context.Background()
	backtests := &repository.Backtest{Conn: test.conn}
	b := backtest.Backtest{
		Name: "backtest",
		Periods: []*backtest.Backtest_Period{
			{
				StartDate: &pb.Date{Year: 2024, Month: 1, Day: 1},
				EndDate:   &pb.Date{Year: 2024, Month: 12, Day: 31},
			},
		},
	}
	_, err = backtests.Create(ctx, &b)
	test.Require().NoError(err)
	test.storedBacktest, err = backtests.Get(ctx, "backtest")
	test.Require().NoError(err)

	sessions := repository.Session{Conn: test.conn}

	test.storedSession, err = sessions.Create(ctx, "backtest")
	test.Require().NoError(err)

	test.uut = repository.Execution{Conn: test.conn}
	test.execution = &backtest.Execution{
		Session:   test.storedSession.Id,
		StartDate: b.Periods[0].StartDate,
		EndDate:   b.Periods[0].EndDate,
	}
}

func (test *ExecutionTest) TearDownTest() {
}

func TestExecutions(t *testing.T) {
	suite.Run(t, new(ExecutionTest))
}

func (test *ExecutionTest) TestCreate() {
	executions := repository.Execution{Conn: test.conn}
	ctx := context.Background()

	execution, err := executions.Create(ctx, test.execution, nil)
	test.Require().NoError(err)
	test.NotNil(execution.Id)
	test.Equal(test.storedSession.Id, execution.Session)
	test.Equal(test.storedBacktest.Periods[0].StartDate, execution.StartDate)
	test.Equal(test.storedBacktest.Periods[0].EndDate, execution.EndDate)
}

func (test *ExecutionTest) TestGet() {
	executions := repository.Execution{Conn: test.conn}
	ctx := context.Background()
	execution, err := executions.Create(ctx, test.execution, nil)
	test.Require().NoError(err)
	execution, err = executions.Get(ctx, execution.Id)
	test.Require().NoError(err)
	test.NotNil(execution.Id)
	test.Equal(test.storedSession.Id, execution.Session)
	test.Equal(test.execution.StartDate, execution.StartDate)
	test.Equal(test.execution.EndDate, execution.EndDate)
}

func (test *ExecutionTest) TestGetPeriods() {
	executions := repository.Execution{Conn: test.conn}
	ctx := context.Background()
	execution, err := executions.Create(ctx, test.execution, nil)
	test.Require().NoError(err)
	periods, err := executions.GetPeriods(ctx, execution.Id)
	test.Require().NoError(err)
	test.NotNil(periods)
}

func (test *ExecutionTest) TestUpdateSimulationDetails() {
	executions := repository.Execution{Conn: test.conn}
	ctx := context.Background()
	execution, err := executions.Create(ctx, test.execution, nil)
	test.Require().NoError(err)
	// e.StartDate = internal_pb.TimeToProtoTimestamp(&common_pb.Date{Year: 2024, Month: 01, Day: 01})
	// e.EndDate = internal_pb.TimeToProtoTimestamp(&common_pb.Date{Year: 2024, Month: 01, Day: 01})
	err = executions.UpdateSimulationDetails(ctx, execution)
	test.Require().NoError(err)
	execution, err = executions.Get(ctx, execution.Id)
	test.Require().NoError(err)
	test.NotNil(execution.Id)
	test.Equal(test.storedSession.Id, execution.Session)
}

func (test *ExecutionTest) TestList() {
	executions := repository.Execution{Conn: test.conn}
	ctx := context.Background()

	_, err := executions.Create(ctx, test.execution, nil)
	test.Require().NoError(err)

	_, err = executions.Create(ctx, test.execution, nil)
	test.Require().NoError(err)

	storedExecutions, err := executions.List(ctx)
	test.Require().NoError(err)
	test.Len(storedExecutions, 2)
}

func (test *ExecutionTest) TestListBySession() {
	sessions := repository.Session{Conn: test.conn}

	ctx := context.Background()
	session1, err := sessions.Create(ctx, "backtest")
	test.Require().NoError(err)

	session2, err := sessions.Create(ctx, "backtest")
	test.Require().NoError(err)

	executions := repository.Execution{Conn: test.conn}
	test.execution.Session = session1.Id
	_, err = executions.Create(ctx, test.execution, nil)
	test.Require().NoError(err)

	storedExecutions, err := executions.ListBySession(ctx, session1.Id)
	test.Require().NoError(err)
	test.Len(storedExecutions, 1)

	storedExecutions, err = executions.ListBySession(ctx, session2.Id)
	test.Require().NoError(err)
	test.Empty(storedExecutions)
}

func (test *ExecutionTest) TestListByBacktest() {
	sessions := repository.Session{Conn: test.conn}

	ctx := context.Background()
	session, err := sessions.Create(ctx, "backtest")
	test.Require().NoError(err)
	test.execution.Session = session.Id

	executions := repository.Execution{Conn: test.conn}
	_, err = executions.Create(ctx, test.execution, nil)
	test.Require().NoError(err)

	storedExecutions, err := executions.ListByBacktest(ctx, "backtest")
	test.Require().NoError(err)
	test.Len(storedExecutions, 1)
}

func (test *ExecutionTest) TestCreateStoresFunctionArguments() {
	executions := repository.Execution{Conn: test.conn}
	ctx := context.Background()
	want := []*backtest.ExecutionFunction{
		{
			Name: "rebalance",
			Parameters: []*backtest.ExecutionFunction_Parameter{
				{Key: "fastperiod", Value: "12", ValueType: "int"},
				{Key: "oversold", Value: "", ValueType: "int"},
			},
		},
		{Name: "screen", Parameters: []*backtest.ExecutionFunction_Parameter{}},
	}

	execution, err := executions.Create(ctx, test.execution, want)
	test.Require().NoError(err)

	got, err := executions.GetFunctions(ctx, execution.Id)
	test.Require().NoError(err)
	test.Len(got, len(want))
	for i := range want {
		test.True(proto.Equal(want[i], got[i]))
	}
}

func (test *ExecutionTest) TestGetFunctionsEmptyWhenUnset() {
	ctx := context.Background()
	var id string
	err := test.conn.QueryRow(ctx,
		`INSERT INTO backtest_execution (session, start_date, end_date, provider) VALUES ($1, $2, $3, 'stooq') RETURNING id`,
		test.storedSession.Id, "2024-01-01", "2024-12-31").Scan(&id)
	test.Require().NoError(err)

	got, err := test.uut.GetFunctions(ctx, id)
	test.Require().NoError(err)
	test.NotNil(got)
	test.Empty(got)

	created, err := test.uut.Create(ctx, test.execution, nil)
	test.Require().NoError(err)
	got, err = test.uut.GetFunctions(ctx, created.Id)
	test.Require().NoError(err)
	test.NotNil(got)
	test.Empty(got)
}

func (test *ExecutionTest) TestCreateCopiesBacktestProvider() {
	ctx := context.Background()
	backtests := &repository.Backtest{Conn: test.conn}
	sessions := repository.Session{Conn: test.conn}
	executions := repository.Execution{Conn: test.conn}

	for _, provider := range []string{"stooq", "alpaca"} {
		name := provider + "-bt"
		_, err := backtests.Create(ctx, &backtest.Backtest{Name: name, Provider: provider})
		test.Require().NoError(err)
		session, err := sessions.Create(ctx, name)
		test.Require().NoError(err)

		created, err := executions.Create(ctx, &backtest.Execution{
			Session:   session.Id,
			StartDate: test.execution.StartDate,
			EndDate:   test.execution.EndDate,
		}, nil)
		test.Require().NoError(err)

		var stored string
		err = test.conn.QueryRow(ctx, `SELECT provider FROM backtest_execution WHERE id=$1`, created.Id).Scan(&stored)
		test.Require().NoError(err)
		test.Equal(provider, stored)
	}

	created, err := executions.Create(ctx, test.execution, nil)
	test.Require().NoError(err)
	var stored string
	err = test.conn.QueryRow(ctx, `SELECT provider FROM backtest_execution WHERE id=$1`, created.Id).Scan(&stored)
	test.Require().NoError(err)
	test.Equal("yahoo", stored)
}

func (test *ExecutionTest) TestMigrateExecutionProviderUpgradesOldTable() {
	ctx := context.Background()
	_, err := test.conn.Exec(ctx, `DROP TABLE IF EXISTS backtest_execution_period`)
	test.Require().NoError(err)
	_, err = test.conn.Exec(ctx, `DROP TABLE IF EXISTS backtest_execution`)
	test.Require().NoError(err)
	_, err = test.conn.Exec(ctx, `
		CREATE TABLE backtest_execution (
			id text PRIMARY KEY DEFAULT uuid_generate_v4(),
			session text REFERENCES session(id) ON DELETE CASCADE,
			start_date date NOT NULL,
			end_date date NOT NULL
		)`)
	test.Require().NoError(err)

	_, err = test.conn.Exec(ctx, `
		INSERT INTO backtest (name, benchmark, provider) VALUES ('stooq-bt', 'SPY', 'stooq')`)
	test.Require().NoError(err)
	var sessionID string
	err = test.conn.QueryRow(ctx, `INSERT INTO session (backtest) VALUES ('stooq-bt') RETURNING id`).Scan(&sessionID)
	test.Require().NoError(err)

	var copiedID, unmatchedID string
	err = test.conn.QueryRow(ctx, `
		INSERT INTO backtest_execution (session, start_date, end_date)
		VALUES ($1, '2024-01-01', '2024-12-31') RETURNING id`, sessionID).Scan(&copiedID)
	test.Require().NoError(err)
	err = test.conn.QueryRow(ctx, `
		INSERT INTO backtest_execution (session, start_date, end_date)
		VALUES (NULL, '2024-01-01', '2024-12-31') RETURNING id`).Scan(&unmatchedID)
	test.Require().NoError(err)

	test.Require().NoError(repository.MigrateExecutionProvider(ctx, test.conn))
	test.Require().NoError(repository.MigrateExecutionProvider(ctx, test.conn))

	var copied, unmatched string
	err = test.conn.QueryRow(ctx, `SELECT provider FROM backtest_execution WHERE id=$1`, copiedID).Scan(&copied)
	test.Require().NoError(err)
	err = test.conn.QueryRow(ctx, `SELECT provider FROM backtest_execution WHERE id=$1`, unmatchedID).Scan(&unmatched)
	test.Require().NoError(err)
	test.Equal("stooq", copied)
	test.Equal("yahoo", unmatched)

	// The yahoo backfill is for old rows only. A new row without a provider fails.
	_, err = test.conn.Exec(ctx, `
		INSERT INTO backtest_execution (session, start_date, end_date)
		VALUES ($1, '2024-01-01', '2024-12-31')`, sessionID)
	test.Require().ErrorContains(err, "provider")

	_, err = test.conn.Exec(ctx, `
		INSERT INTO backtest_execution (session, start_date, end_date, provider)
		VALUES ($1, '2024-01-01', '2024-12-31', 'nasdaq')`, sessionID)
	test.Require().ErrorContains(err, "backtest_execution")
}

// SetupTest creates the backtest with an empty provider. The execution copies the stored provider,
// so it is supplier.DefaultProvider.
func (test *ExecutionTest) TestCreateWithEmptyBacktestProviderStoresDefaultProvider() {
	ctx := context.Background()
	created, err := test.uut.Create(ctx, test.execution, nil)
	test.Require().NoError(err)

	var backtestProvider, executionProvider string
	err = test.conn.QueryRow(ctx, `SELECT provider FROM backtest WHERE name = $1`, test.storedBacktest.Name).Scan(&backtestProvider)
	test.Require().NoError(err)
	err = test.conn.QueryRow(ctx, `SELECT provider FROM backtest_execution WHERE id = $1`, created.Id).Scan(&executionProvider)
	test.Require().NoError(err)
	test.Equal("yahoo", backtestProvider)
	test.Equal("yahoo", executionProvider)
	test.Equal(supplier.DefaultProvider, executionProvider)
}

// A row that omits provider must fail loudly rather than be stored as yahoo.
func (test *ExecutionTest) TestInsertWithoutProviderFails() {
	ctx := context.Background()
	_, err := test.conn.Exec(ctx, `
		INSERT INTO backtest_execution (session, start_date, end_date)
		VALUES ($1, '2024-01-01', '2024-12-31')`, test.storedSession.Id)
	test.Require().ErrorContains(err, "provider")

	var count int
	err = test.conn.QueryRow(ctx, `SELECT count(*) FROM backtest_execution`).Scan(&count)
	test.Require().NoError(err)
	test.Equal(0, count)
}

func (test *ExecutionTest) TestMigrateExecutionProviderLeavesInvalidProvider() {
	ctx := context.Background()
	_, err := test.conn.Exec(ctx, `ALTER TABLE backtest_execution DROP CONSTRAINT backtest_execution_provider_check`)
	test.Require().NoError(err)
	var id string
	err = test.conn.QueryRow(ctx, `
		INSERT INTO backtest_execution (session, start_date, end_date, provider)
		VALUES ($1, '2024-01-01', '2024-12-31', 'nasdaq') RETURNING id`, test.storedSession.Id).Scan(&id)
	test.Require().NoError(err)

	err = repository.MigrateExecutionProvider(ctx, test.conn)
	test.Require().ErrorContains(err, "backtest_execution")

	var provider string
	err = test.conn.QueryRow(ctx, `SELECT provider FROM backtest_execution WHERE id=$1`, id).Scan(&provider)
	test.Require().NoError(err)
	test.Equal("nasdaq", provider)
}

func (test *ExecutionTest) TestPeriodCascadeAndUnique() {
	ctx := context.Background()
	test.Require().NoError(repository.MigrateExecutionPeriod(ctx, test.conn))
	test.Require().NoError(repository.MigrateExecutionPeriod(ctx, test.conn))

	executions := repository.Execution{Conn: test.conn}
	created, err := executions.Create(ctx, test.execution, nil)
	test.Require().NoError(err)
	period := &backtest.Period{Date: &pb.Date{Year: 2024, Month: 1, Day: 2}}
	test.Require().NoError(executions.StorePeriods(ctx, created.Id, []*backtest.Period{period}))
	err = executions.StorePeriods(ctx, created.Id, []*backtest.Period{period})
	test.Require().ErrorContains(err, "unique_backtest_execution_period")

	_, err = test.conn.Exec(ctx, `
		INSERT INTO backtest_execution_period (
			backtest_execution, date, pnl, returns, portfolio_value,
			long_value, short_value, starting_exposure, ending_exposure, long_exposure, short_exposure,
			capital_used, gross_leverage, net_leverage,
			starting_value, ending_value, starting_cash, ending_cash,
			max_drawdown, max_leverage, excess_returns, treasury_period_return, algorithm_period_return
		) VALUES (
			'missing-execution', '2024-01-03', 0, 0, 0,
			0, 0, 0, 0, 0, 0,
			0, 0, 0,
			0, 0, 0, 0,
			0, 0, 0, 0, 0
		)`)
	test.Require().ErrorContains(err, "backtest_execution_period")

	_, err = test.conn.Exec(ctx, `DELETE FROM backtest_execution WHERE id=$1`, created.Id)
	test.Require().NoError(err)
	var n int
	err = test.conn.QueryRow(ctx, `SELECT COUNT(*) FROM backtest_execution_period WHERE backtest_execution=$1`, created.Id).Scan(&n)
	test.Require().NoError(err)
	test.Zero(n)
}

func (test *ExecutionTest) TestMigrateExecutionPeriodLeavesOrphans() {
	ctx := context.Background()
	_, err := test.conn.Exec(ctx, `DROP TABLE backtest_execution_period`)
	test.Require().NoError(err)
	_, err = test.conn.Exec(ctx, `
		CREATE TABLE backtest_execution_period (
			id serial PRIMARY KEY,
			backtest_execution text NOT NULL,
			date date NOT NULL
		)`)
	test.Require().NoError(err)
	_, err = test.conn.Exec(ctx, `
		INSERT INTO backtest_execution_period (backtest_execution, date)
		VALUES ('missing-execution', '2024-01-02')`)
	test.Require().NoError(err)

	err = repository.MigrateExecutionPeriod(ctx, test.conn)
	test.Require().ErrorContains(err, "backtest_execution_period")

	var execution string
	err = test.conn.QueryRow(ctx, `SELECT backtest_execution FROM backtest_execution_period`).Scan(&execution)
	test.Require().NoError(err)
	test.Equal("missing-execution", execution)
}

func (test *ExecutionTest) TestNoticesRoundTrip() {
	ctx := context.Background()
	execution, err := test.uut.Create(ctx, test.execution, nil)
	test.Require().NoError(err)

	// A run stored before notices existed, or with nothing to report, reads empty.
	notices, err := test.uut.GetNotices(ctx, execution.Id)
	test.Require().NoError(err)
	test.Empty(notices)

	line := "3 symbols were delisted after their last price on 2026-03-31 and were left out: ABC, DEF, GHI."
	test.Require().NoError(test.uut.SetNotices(ctx, execution.Id, []string{line}))
	notices, err = test.uut.GetNotices(ctx, execution.Id)
	test.Require().NoError(err)
	test.Equal([]string{line}, notices)

	test.Require().NoError(test.uut.SetNotices(ctx, execution.Id, nil))
	notices, err = test.uut.GetNotices(ctx, execution.Id)
	test.Require().NoError(err)
	test.Empty(notices)
}

func (test *ExecutionTest) TestSetNoticesUnknownExecution() {
	err := test.uut.SetNotices(context.Background(), "00000000-0000-0000-0000-000000000000", []string{"x"})
	test.Require().Error(err)
}
