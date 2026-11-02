package command_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/internal/stream"
	"github.com/lhjnilsson/foreverbull/internal/test_helper"
	pb "github.com/lhjnilsson/foreverbull/pkg/pb/strategy"
	"github.com/lhjnilsson/foreverbull/pkg/strategy/internal/repository"
	ss "github.com/lhjnilsson/foreverbull/pkg/strategy/internal/stream"
	"github.com/lhjnilsson/foreverbull/pkg/strategy/internal/stream/command"
	"github.com/lhjnilsson/foreverbull/pkg/strategy/internal/stream/dependency"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/suite"
)

type ExperimentCommandTest struct {
	suite.Suite

	conn *pgxpool.Pool
}

func TestExperimentCommand(t *testing.T) {
	suite.Run(t, new(ExperimentCommandTest))
}

func (test *ExperimentCommandTest) SetupSuite() {
	test_helper.SetupEnvironment(test.T(), &test_helper.Containers{
		Postgres: true,
	})
}

func (test *ExperimentCommandTest) SetupTest() {
	var err error
	test.conn, err = pgxpool.New(context.Background(), environment.GetPostgresURL())
	test.Require().NoError(err)
	test.Require().NoError(repository.Recreate(context.Background(), test.conn))
}

func (test *ExperimentCommandTest) seedExperiment(backtestName string) *pb.Experiment {
	test.T().Helper()
	ctx := context.Background()
	_, err := test.conn.Exec(ctx, `INSERT INTO service (image, status) VALUES ($1, $2)`, "algo:local", 2)
	test.Require().NoError(err)

	strategies := repository.Strategy{Conn: test.conn}
	_, err = strategies.Create(ctx, "mean-rev", nil)
	test.Require().NoError(err)

	versions := repository.Version{Conn: test.conn}
	version, err := versions.Create(ctx, "mean-rev", "algo:local", nil)
	test.Require().NoError(err)

	_, err = test.conn.Exec(ctx,
		`INSERT INTO backtest (name, benchmark) VALUES ($1, $2) ON CONFLICT (name) DO NOTHING`,
		backtestName, "SPY")
	test.Require().NoError(err)

	experiments := repository.Experiment{Conn: test.conn}
	experiment, err := experiments.Create(ctx, version.Id, backtestName)
	test.Require().NoError(err)
	return experiment
}

func (test *ExperimentCommandTest) insertExecution(backtestName string) string {
	test.T().Helper()
	ctx := context.Background()
	_, err := test.conn.Exec(ctx, `INSERT INTO backtest (name, benchmark) VALUES ($1, $2) ON CONFLICT (name) DO NOTHING`, backtestName, "SPY")
	test.Require().NoError(err)

	var sessionID string
	err = test.conn.QueryRow(ctx, `INSERT INTO session (backtest) VALUES ($1) RETURNING id`, backtestName).Scan(&sessionID)
	test.Require().NoError(err)

	var executionID string
	err = test.conn.QueryRow(ctx,
		`INSERT INTO backtest_execution (session, start_date, end_date) VALUES ($1, $2, $3) RETURNING id`,
		sessionID, "2020-01-01", "2020-12-31",
	).Scan(&executionID)
	test.Require().NoError(err)
	return executionID
}

func (test *ExperimentCommandTest) TestExperimentRunSuccess() {
	experiment := test.seedExperiment("spy")
	executionID := test.insertExecution("spy")

	launcher := new(ss.MockExperimentLauncher)
	launcher.On("Run", mock.Anything, mock.MatchedBy(func(in ss.RunInput) bool {
		return in.ExperimentID == experiment.Id && in.BacktestName == "spy"
	})).Return(&ss.RunOutput{ExecutionID: executionID}, nil)

	msg := new(stream.MockMessage)
	msg.On("ParsePayload", mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		cmd := args.Get(0).(*ss.ExperimentRunCommand)
		cmd.ExperimentID = experiment.Id
	})
	msg.On("MustGet", stream.DBDep).Return(test.conn)
	msg.On("MustGet", dependency.ExperimentLauncherDep).Return(launcher)

	err := command.ExperimentRun(context.Background(), msg)
	test.Require().NoError(err)

	experiments := repository.Experiment{Conn: test.conn}
	got, err := experiments.Get(context.Background(), experiment.Id)
	test.Require().NoError(err)
	test.Equal(pb.Experiment_COMPLETED, got.Status)
	test.Equal(executionID, got.GetExecutionId())
}

func (test *ExperimentCommandTest) TestExperimentRunLauncherFails() {
	experiment := test.seedExperiment("eu")

	launcher := new(ss.MockExperimentLauncher)
	launcher.On("Run", mock.Anything, mock.Anything).Return(nil, errors.New("zipline exploded"))

	msg := new(stream.MockMessage)
	msg.On("ParsePayload", mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		cmd := args.Get(0).(*ss.ExperimentRunCommand)
		cmd.ExperimentID = experiment.Id
	})
	msg.On("MustGet", stream.DBDep).Return(test.conn)
	msg.On("MustGet", dependency.ExperimentLauncherDep).Return(launcher)

	err := command.ExperimentRun(context.Background(), msg)
	test.Require().Error(err)
	test.Require().ErrorContains(err, "zipline exploded")

	experiments := repository.Experiment{Conn: test.conn}
	got, err := experiments.Get(context.Background(), experiment.Id)
	test.Require().NoError(err)
	test.Equal(pb.Experiment_FAILED, got.Status)
	test.Equal("zipline exploded", got.GetError())
}

func (test *ExperimentCommandTest) TestExperimentRunParseFails() {
	msg := new(stream.MockMessage)
	msg.On("ParsePayload", mock.Anything).Return(errors.New("not working"))

	err := command.ExperimentRun(context.Background(), msg)
	test.Require().Error(err)
	test.Require().ErrorContains(err, "not working")
}

// The run's context is gone by the time the failure is stored, as when the
// command is aborted. The FAILED write must still land, so the experiment
// isn't left RUNNING.
func (test *ExperimentCommandTest) TestExperimentRunCancelledStillMarksFailed() {
	experiment := test.seedExperiment("eu")
	ctx, cancel := context.WithCancel(context.Background())

	launcher := new(ss.MockExperimentLauncher)
	launcher.On("Run", mock.Anything, mock.Anything).Return(nil, context.Canceled).Run(func(mock.Arguments) {
		cancel()
	})

	msg := new(stream.MockMessage)
	msg.On("ParsePayload", mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		cmd := args.Get(0).(*ss.ExperimentRunCommand)
		cmd.ExperimentID = experiment.Id
	})
	msg.On("MustGet", stream.DBDep).Return(test.conn)
	msg.On("MustGet", dependency.ExperimentLauncherDep).Return(launcher)

	err := command.ExperimentRun(ctx, msg)
	test.Require().ErrorIs(err, context.Canceled)
	test.Require().EqualError(err, "context canceled")

	experiments := repository.Experiment{Conn: test.conn}
	got, err := experiments.Get(context.Background(), experiment.Id)
	test.Require().NoError(err)
	test.Equal(pb.Experiment_FAILED, got.Status)
	test.Equal("context canceled", got.GetError())
}
