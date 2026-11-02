package api_test

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lhjnilsson/foreverbull/internal/environment"
	internalGrpc "github.com/lhjnilsson/foreverbull/internal/grpc"
	"github.com/lhjnilsson/foreverbull/internal/test_helper"
	"github.com/lhjnilsson/foreverbull/pkg/finance/supplier"
	common "github.com/lhjnilsson/foreverbull/pkg/pb"
	financepb "github.com/lhjnilsson/foreverbull/pkg/pb/finance"
	service "github.com/lhjnilsson/foreverbull/pkg/pb/service"
	pb "github.com/lhjnilsson/foreverbull/pkg/pb/strategy"
	servicePb "github.com/lhjnilsson/foreverbull/pkg/pb/strategy/service"
	"github.com/lhjnilsson/foreverbull/pkg/strategy/internal/api"
	"github.com/lhjnilsson/foreverbull/pkg/strategy/internal/repository"
	ss "github.com/lhjnilsson/foreverbull/pkg/strategy/internal/stream"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/suite"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type recordingRunner struct {
	added     []*pb.Deployment
	removed   []string
	addErr    error
	removeErr error
	onRemove  func()
}

func (r *recordingRunner) Add(_ context.Context, d *pb.Deployment) error {
	r.added = append(r.added, d)
	return r.addErr
}

func (r *recordingRunner) Remove(ctx context.Context, deploymentID string) error {
	r.removed = append(r.removed, deploymentID)
	if r.onRemove != nil {
		r.onRemove()
	}
	if r.removeErr != nil {
		return r.removeErr
	}
	return ctx.Err()
}

type stubTrading struct{}

func (stubTrading) GetPortfolio(context.Context) (*financepb.Portfolio, error) {
	return &financepb.Portfolio{}, nil
}

func (stubTrading) PlaceOrder(_ context.Context, order *financepb.Order) (*financepb.Order, error) {
	return order, nil
}

func (stubTrading) GetOrders(context.Context) ([]*financepb.Order, error) {
	return nil, nil
}

var _ supplier.Trading = stubTrading{}

type profileRegistry map[string]supplier.Trading

func (p profileRegistry) Get(name string) (supplier.Trading, bool) {
	if p == nil {
		return nil, false
	}
	client, ok := p[name]
	return client, ok
}

func paperProfiles() supplier.Registry {
	return profileRegistry{"alpaca-paper": stubTrading{}}
}

type StrategyServerTest struct {
	suite.Suite

	pgx      *pgxpool.Pool
	launcher *ss.MockExperimentLauncher
	runner   *recordingRunner

	listener *bufconn.Listener
	server   *grpc.Server
	client   servicePb.StrategyServicerClient
}

func TestStrategyServer(t *testing.T) {
	suite.Run(t, new(StrategyServerTest))
}

func (test *StrategyServerTest) SetupSuite() {
	test_helper.SetupEnvironment(test.T(), &test_helper.Containers{
		Postgres: true,
	})
}

func (test *StrategyServerTest) SetupSubTest() {
	var err error
	test.pgx, err = pgxpool.New(context.Background(), environment.GetPostgresURL())
	test.Require().NoError(err)
	test.Require().NoError(repository.Recreate(context.Background(), test.pgx))

	test.launcher = new(ss.MockExperimentLauncher)
	test.runner = &recordingRunner{}
	test.startServer(nil)
}

func (test *StrategyServerTest) startServer(profiles supplier.Registry) {
	if test.server != nil {
		test.server.Stop()
		test.server = nil
	}
	if test.listener != nil {
		_ = test.listener.Close()
		test.listener = nil
	}

	var err error
	test.listener = bufconn.Listen(1024 * 1024)
	test.server, err = internalGrpc.NewServer()
	test.Require().NoError(err)

	srv := api.NewStrategyServer(test.pgx, test.launcher, test.runner)
	if profiles != nil {
		srv = srv.WithProfiles(profiles)
	}
	servicePb.RegisterStrategyServicerServer(test.server, srv)

	server := test.server
	listener := test.listener
	go func() {
		_ = server.Serve(listener)
	}()

	resolver.SetDefaultScheme("passthrough")

	conn, err := grpc.NewClient("bufnet",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(_ context.Context, _ string) (net.Conn, error) {
			return test.listener.Dial()
		}),
	)
	test.Require().NoError(err)
	test.client = servicePb.NewStrategyServicerClient(conn)
}

func (test *StrategyServerTest) TearDownSubTest() {
	if test.server != nil {
		test.server.Stop()
	}
	if test.listener != nil {
		test.NoError(test.listener.Close())
	}
}

func (test *StrategyServerTest) seedVersion(name, image string, status service.Service_Status_Status) *pb.Version {
	test.T().Helper()
	ctx := context.Background()
	_, err := test.pgx.Exec(ctx, `INSERT INTO service (image, status) VALUES ($1, $2)`, image, int(status))
	test.Require().NoError(err)

	strategies := repository.Strategy{Conn: test.pgx}
	_, err = strategies.Create(ctx, name, nil)
	test.Require().NoError(err)

	versions := repository.Version{Conn: test.pgx}
	version, err := versions.Create(ctx, name, image, nil)
	test.Require().NoError(err)
	return version
}

func (test *StrategyServerTest) insertBacktest(name string) {
	test.T().Helper()
	_, err := test.pgx.Exec(context.Background(), `INSERT INTO backtest (name, benchmark) VALUES ($1, $2)`, name, "SPY")
	test.Require().NoError(err)
}

func (test *StrategyServerTest) insertPeriod(
	executionID, date string,
	portfolioValue, maxDrawdown, algorithmPeriodReturn float64,
	sharpe, benchmark *float64,
) {
	test.T().Helper()
	_, err := test.pgx.Exec(context.Background(), `
		INSERT INTO backtest_execution_period (
			backtest_execution, date, pnl, returns, portfolio_value,
			long_value, short_value, starting_exposure, ending_exposure, long_exposure, short_exposure,
			capital_used, gross_leverage, net_leverage,
			starting_value, ending_value, starting_cash, ending_cash,
			max_drawdown, max_leverage, excess_returns, treasury_period_return, algorithm_period_return,
			sharpe, benchmark_period_return
		) VALUES (
			$1, $2, 0, 0, $3,
			0, 0, 0, 0, 0, 0,
			0, 0, 0,
			0, 0, 0, 0,
			$4, 0, 0, 0, $5,
			$6, $7
		)`,
		executionID, date, portfolioValue, maxDrawdown, algorithmPeriodReturn, sharpe, benchmark,
	)
	test.Require().NoError(err)
}

func floatPtr(v float64) *float64 {
	return &v
}

func (test *StrategyServerTest) insertExecution(backtestName string) string {
	test.T().Helper()
	ctx := context.Background()
	var n int
	err := test.pgx.QueryRow(ctx, `SELECT COUNT(*) FROM backtest WHERE name=$1`, backtestName).Scan(&n)
	test.Require().NoError(err)
	if n == 0 {
		test.insertBacktest(backtestName)
	}

	var sessionID string
	err = test.pgx.QueryRow(ctx, `INSERT INTO session (backtest) VALUES ($1) RETURNING id`, backtestName).Scan(&sessionID)
	test.Require().NoError(err)

	var executionID string
	err = test.pgx.QueryRow(ctx,
		`INSERT INTO backtest_execution (session, start_date, end_date) VALUES ($1, $2, $3) RETURNING id`,
		sessionID, "2020-01-01", "2020-12-31",
	).Scan(&executionID)
	test.Require().NoError(err)
	return executionID
}

func recvAll(stream grpc.ServerStreamingClient[servicePb.CreateExperimentResponse]) ([]*servicePb.CreateExperimentResponse, error) {
	var out []*servicePb.CreateExperimentResponse
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, msg)
	}
}

func (test *StrategyServerTest) TestCreateStrategyUnimplemented() {
	test.Run("unimplemented", func() {
		_, err := test.client.CreateStrategy(context.Background(), &servicePb.CreateStrategyRequest{
			Name: "mean-rev",
		})
		test.Require().Error(err)
		st, ok := status.FromError(err)
		test.True(ok)
		test.Equal(codes.Unimplemented, st.Code())
	})
}

func (test *StrategyServerTest) TestCreateExperiment() {
	test.Run("success", func() {
		version := test.seedVersion("mean-rev", "algo:local", service.Service_Status_READY)
		executionID := test.insertExecution("spy")

		test.launcher.On("Run", mock.Anything, mock.MatchedBy(func(in ss.RunInput) bool {
			return in.BacktestName == "spy" && in.ServiceImage == "algo:local"
		})).Return(&ss.RunOutput{ExecutionID: executionID}, nil)

		stream, err := test.client.CreateExperiment(context.Background(), &servicePb.CreateExperimentRequest{
			StrategyName: "mean-rev",
			VersionId:    version.Id,
			BacktestName: "spy",
		})
		test.Require().NoError(err)

		msgs, err := recvAll(stream)
		test.Require().NoError(err)
		test.NotEmpty(msgs)
		last := msgs[len(msgs)-1]
		test.Equal(common.State_STATE_COMPLETED, last.GetQUEUED().GetState())
		test.Equal(common.State_STATE_COMPLETED, last.GetRUNNING().GetState())
		test.Equal(common.State_STATE_COMPLETED, last.GetCOMPLETED().GetState())
		test.Equal(pb.Experiment_COMPLETED, last.GetExperiment().GetStatus())
		test.Equal(executionID, last.GetExperiment().GetExecutionId())
		test.Equal("spy", last.GetExperiment().GetBacktestName())
	})

	test.Run("same version many backtests", func() {
		version := test.seedVersion("mean-rev", "algo:local", service.Service_Status_READY)
		spyExec := test.insertExecution("spy")
		euExec := test.insertExecution("eu")

		test.launcher.On("Run", mock.Anything, mock.MatchedBy(func(in ss.RunInput) bool {
			return in.BacktestName == "spy"
		})).Return(&ss.RunOutput{ExecutionID: spyExec}, nil).Once()
		test.launcher.On("Run", mock.Anything, mock.MatchedBy(func(in ss.RunInput) bool {
			return in.BacktestName == "eu"
		})).Return(&ss.RunOutput{ExecutionID: euExec}, nil).Once()

		for _, name := range []string{"spy", "eu"} {
			stream, err := test.client.CreateExperiment(context.Background(), &servicePb.CreateExperimentRequest{
				StrategyName: "mean-rev",
				VersionId:    version.Id,
				BacktestName: name,
			})
			test.Require().NoError(err)
			msgs, err := recvAll(stream)
			test.Require().NoError(err)
			test.Equal(name, msgs[len(msgs)-1].GetExperiment().GetBacktestName())
			test.Equal(pb.Experiment_COMPLETED, msgs[len(msgs)-1].GetExperiment().GetStatus())
		}

		listed, err := test.client.ListExperiments(context.Background(), &servicePb.ListExperimentsRequest{
			StrategyName: "mean-rev",
			VersionId:    version.Id,
		})
		test.Require().NoError(err)
		test.Len(listed.Experiments, 2)
		names := []string{listed.Experiments[0].BacktestName, listed.Experiments[1].BacktestName}
		test.ElementsMatch([]string{"spy", "eu"}, names)
	})

	test.Run("service not ready", func() {
		version := test.seedVersion("mean-rev", "algo:local", service.Service_Status_CREATED)
		test.insertBacktest("spy")

		stream, err := test.client.CreateExperiment(context.Background(), &servicePb.CreateExperimentRequest{
			StrategyName: "mean-rev",
			VersionId:    version.Id,
			BacktestName: "spy",
		})
		test.Require().NoError(err)
		_, err = stream.Recv()
		test.Require().Error(err)
		st, ok := status.FromError(err)
		test.True(ok)
		test.Equal(codes.FailedPrecondition, st.Code())
		test.Contains(st.Message(), "not READY")
		test.launcher.AssertNotCalled(test.T(), "Run", mock.Anything, mock.Anything)
	})

	test.Run("missing backtest", func() {
		version := test.seedVersion("mean-rev", "algo:local", service.Service_Status_READY)

		stream, err := test.client.CreateExperiment(context.Background(), &servicePb.CreateExperimentRequest{
			StrategyName: "mean-rev",
			VersionId:    version.Id,
			BacktestName: "commodities",
		})
		test.Require().NoError(err)
		_, err = stream.Recv()
		test.Require().Error(err)
		st, ok := status.FromError(err)
		test.True(ok)
		test.Equal(codes.NotFound, st.Code())
		test.Contains(st.Message(), "commodities")
		test.Contains(st.Message(), "fbull backtest create")
		test.Contains(st.Message(), "do not auto-ingest")
		test.launcher.AssertNotCalled(test.T(), "Run", mock.Anything, mock.Anything)
	})

	test.Run("launcher fails", func() {
		version := test.seedVersion("mean-rev", "algo:local", service.Service_Status_READY)
		test.insertBacktest("spy")
		test.launcher.On("Run", mock.Anything, mock.Anything).Return(nil, errors.New("engine down"))

		stream, err := test.client.CreateExperiment(context.Background(), &servicePb.CreateExperimentRequest{
			StrategyName: "mean-rev",
			VersionId:    version.Id,
			BacktestName: "spy",
		})
		test.Require().NoError(err)
		msgs, err := recvAll(stream)
		test.Require().Error(err)
		test.NotEmpty(msgs)
		last := msgs[len(msgs)-1]
		test.Equal(pb.Experiment_FAILED, last.GetExperiment().GetStatus())
		test.Contains(last.GetExperiment().GetError(), "engine down")
		st, ok := status.FromError(err)
		test.Require().True(ok)
		test.Equal(codes.Internal, st.Code())
		test.Equal("Couldn't run experiment 'mean-rev': the backtest run failed. Run `fbenv status` to check that "+
			"the environment is running, then run `fbull strategy experiment mean-rev --backtest spy` again.", st.Message())
	})

	test.Run("version by rev", func() {
		version := test.seedVersion("mean-rev", "algo:local", service.Service_Status_READY)
		executionID := test.insertExecution("spy")
		test.launcher.On("Run", mock.Anything, mock.Anything).Return(&ss.RunOutput{ExecutionID: executionID}, nil)

		stream, err := test.client.CreateExperiment(context.Background(), &servicePb.CreateExperimentRequest{
			StrategyName: "mean-rev",
			VersionRev:   version.Rev,
			BacktestName: "spy",
		})
		test.Require().NoError(err)
		msgs, err := recvAll(stream)
		test.Require().NoError(err)
		test.Equal(version.Id, msgs[len(msgs)-1].GetExperiment().GetVersionId())
	})
}

func (test *StrategyServerTest) TestGetAndListExperiments() {
	test.Run("get and list", func() {
		version := test.seedVersion("mean-rev", "algo:local", service.Service_Status_READY)
		executionID := test.insertExecution("spy")
		test.insertBacktest("eu")

		experiments := repository.Experiment{Conn: test.pgx}
		spy, err := experiments.Create(context.Background(), version.Id, "spy")
		test.Require().NoError(err)
		test.Require().NoError(experiments.SetExecutionID(context.Background(), spy.Id, executionID))
		test.Require().NoError(experiments.UpdateStatus(context.Background(), spy.Id, pb.Experiment_COMPLETED, nil))
		eu, err := experiments.Create(context.Background(), version.Id, "eu")
		test.Require().NoError(err)

		got, err := test.client.GetExperiment(context.Background(), &servicePb.GetExperimentRequest{
			ExperimentId: spy.Id,
		})
		test.Require().NoError(err)
		test.Equal(spy.Id, got.GetExperiment().GetId())
		test.Equal(executionID, got.GetExperiment().GetExecutionId())
		test.Require().NotNil(got.GrafanaPath)
		wantPath, err := api.GrafanaExecutionURL(environment.GetGrafanaURL(), executionID)
		test.Require().NoError(err)
		test.Equal(wantPath, *got.GrafanaPath)
		test.Contains(*got.GrafanaPath, "/d/ee9ep4jxtwkcgf?")
		test.Contains(*got.GrafanaPath, "var-execution="+executionID)
		test.Nil(got.LastPeriod)
		test.Nil(got.GetExperiment().GetStats())

		listed, err := test.client.ListExperiments(context.Background(), &servicePb.ListExperimentsRequest{
			StrategyName: "mean-rev",
		})
		test.Require().NoError(err)
		test.Len(listed.Experiments, 2)

		byBacktest, err := test.client.ListExperiments(context.Background(), &servicePb.ListExperimentsRequest{
			StrategyName: "mean-rev",
			BacktestName: "eu",
		})
		test.Require().NoError(err)
		test.Len(byBacktest.Experiments, 1)
		test.Equal(eu.Id, byBacktest.Experiments[0].Id)
	})

	test.Run("not found", func() {
		_, err := test.client.GetExperiment(context.Background(), &servicePb.GetExperimentRequest{
			ExperimentId: "missing",
		})
		test.Require().Error(err)
		st, ok := status.FromError(err)
		test.True(ok)
		test.Equal(codes.NotFound, st.Code())
	})

	test.Run("joins last period stats", func() {
		version := test.seedVersion("mean-rev", "algo:local", service.Service_Status_READY)
		spyExec := test.insertExecution("spy")
		euExec := test.insertExecution("eu")
		test.insertPeriod(spyExec, "2020-01-02", 1000, -0.1, 0.01, floatPtr(0.5), nil)
		test.insertPeriod(spyExec, "2020-06-01", 1100, -0.2, 0.05, floatPtr(1.25), floatPtr(0.03))
		test.insertPeriod(euExec, "2020-06-01", 900, -0.15, 0.02, nil, floatPtr(0.01))
		test.insertBacktest("commodities")

		experiments := repository.Experiment{Conn: test.pgx}
		spy, err := experiments.Create(context.Background(), version.Id, "spy")
		test.Require().NoError(err)
		test.Require().NoError(experiments.SetExecutionID(context.Background(), spy.Id, spyExec))
		test.Require().NoError(experiments.UpdateStatus(context.Background(), spy.Id, pb.Experiment_COMPLETED, nil))
		eu, err := experiments.Create(context.Background(), version.Id, "eu")
		test.Require().NoError(err)
		test.Require().NoError(experiments.SetExecutionID(context.Background(), eu.Id, euExec))
		test.Require().NoError(experiments.UpdateStatus(context.Background(), eu.Id, pb.Experiment_COMPLETED, nil))
		queued, err := experiments.Create(context.Background(), version.Id, "commodities")
		test.Require().NoError(err)

		got, err := test.client.GetExperiment(context.Background(), &servicePb.GetExperimentRequest{
			ExperimentId: spy.Id,
		})
		test.Require().NoError(err)
		test.Require().NotNil(got.GetExperiment().GetStats())
		test.InDelta(1100, got.GetExperiment().GetStats().GetPortfolioValue(), 1e-9)
		test.InDelta(-0.2, got.GetExperiment().GetStats().GetMaxDrawdown(), 1e-9)
		test.InDelta(1.25, got.GetExperiment().GetStats().GetSharpe(), 1e-9)
		test.InDelta(0.05, got.GetExperiment().GetStats().GetAlgorithmPeriodReturn(), 1e-9)
		test.InDelta(0.03, got.GetExperiment().GetStats().GetBenchmarkPeriodReturn(), 1e-9)
		test.Require().NotNil(got.LastPeriod)
		test.Equal(int32(2020), got.LastPeriod.Date.Year)
		test.Equal(int32(6), got.LastPeriod.Date.Month)
		test.Equal(int32(1), got.LastPeriod.Date.Day)
		test.InDelta(1100, got.LastPeriod.PortfolioValue, 1e-9)
		test.InDelta(-0.2, got.LastPeriod.MaxDrawdown, 1e-9)
		test.Require().NotNil(got.LastPeriod.Sharpe)
		test.InDelta(1.25, *got.LastPeriod.Sharpe, 1e-9)
		test.InDelta(0.05, got.LastPeriod.AlgorithmPeriodReturn, 1e-9)
		test.Require().NotNil(got.LastPeriod.BenchmarkPeriodReturn)
		test.InDelta(0.03, *got.LastPeriod.BenchmarkPeriodReturn, 1e-9)

		listed, err := test.client.ListExperiments(context.Background(), &servicePb.ListExperimentsRequest{
			StrategyName: "mean-rev",
			VersionId:    version.Id,
		})
		test.Require().NoError(err)
		test.Len(listed.Experiments, 3)
		byID := map[string]*pb.Experiment{}
		for _, e := range listed.Experiments {
			byID[e.Id] = e
		}

		test.Require().NotNil(byID[spy.Id].Stats)
		test.InDelta(1100, byID[spy.Id].Stats.PortfolioValue, 1e-9)
		test.InDelta(1.25, byID[spy.Id].Stats.GetSharpe(), 1e-9)
		test.Require().NotNil(byID[eu.Id].Stats)
		test.InDelta(900, byID[eu.Id].Stats.PortfolioValue, 1e-9)
		test.Nil(byID[eu.Id].Stats.Sharpe)
		test.InDelta(-0.15, byID[eu.Id].Stats.MaxDrawdown, 1e-9)
		test.Nil(byID[queued.Id].Stats)

		empty, err := test.client.GetExperiment(context.Background(), &servicePb.GetExperimentRequest{
			ExperimentId: queued.Id,
		})
		test.Require().NoError(err)
		test.Nil(empty.GetExperiment().GetStats())
		test.Nil(empty.LastPeriod)
		test.Nil(empty.GrafanaPath)
	})
}

func (test *StrategyServerTest) seedDeployableVersion(name, image string) *pb.Version {
	test.T().Helper()
	version := test.seedVersion(name, image, service.Service_Status_READY)
	test.insertBacktest("spy")
	experiments := repository.Experiment{Conn: test.pgx}
	created, err := experiments.Create(context.Background(), version.Id, "spy")
	test.Require().NoError(err)
	test.Require().NoError(experiments.UpdateStatus(context.Background(), created.Id, pb.Experiment_COMPLETED, nil))
	return version
}

func (test *StrategyServerTest) TestDeploy() {
	test.Run("defaults paper dry-run", func() {
		version := test.seedDeployableVersion("mean-rev", "algo:local")

		rsp, err := test.client.Deploy(context.Background(), &servicePb.DeployRequest{
			StrategyName: "mean-rev",
			VersionId:    version.Id,
			Symbols:      []string{"AAPL"},
		})
		test.Require().NoError(err)
		d := rsp.GetDeployment()
		test.Equal(pb.Deployment_PAPER, d.GetMode())
		test.True(d.GetDryRun())
		test.Empty(d.GetBackend())
		test.Equal("XNYS", d.GetCalendarCode())
		test.Equal(pb.Deployment_SESSION_CLOSE, d.GetScheduleKind())
		test.Equal(pb.Deployment_RUNNING, d.GetStatus())
		test.Equal([]string{"AAPL"}, d.GetSymbols())
		test.Require().Len(test.runner.added, 1)
		test.Equal(d.Id, test.runner.added[0].Id)
	})

	test.Run("rejects live", func() {
		version := test.seedDeployableVersion("mean-rev", "algo:local")
		_, err := test.client.Deploy(context.Background(), &servicePb.DeployRequest{
			StrategyName: "mean-rev",
			VersionId:    version.Id,
			Mode:         pb.Deployment_LIVE,
			Confirm:      true,
		})
		test.Require().Error(err)
		st, ok := status.FromError(err)
		test.True(ok)
		test.Equal(codes.FailedPrecondition, st.Code())
		test.Contains(st.Message(), "live")
		test.Empty(test.runner.added)
	})

	test.Run("rejects live even with trading", func() {
		test.startServer(paperProfiles())
		version := test.seedDeployableVersion("mean-rev", "algo:local")
		dry := false
		_, err := test.client.Deploy(context.Background(), &servicePb.DeployRequest{
			StrategyName: "mean-rev",
			VersionId:    version.Id,
			Mode:         pb.Deployment_LIVE,
			DryRun:       &dry,
			Confirm:      true,
		})
		test.Require().Error(err)
		st, ok := status.FromError(err)
		test.True(ok)
		test.Equal(codes.FailedPrecondition, st.Code())
		test.Contains(st.Message(), "live")
		test.Empty(test.runner.added)
	})

	test.Run("rejects dry_run false without confirm", func() {
		test.T().Setenv(environment.TradingEnabled, "true")
		test.startServer(paperProfiles())
		version := test.seedDeployableVersion("mean-rev", "algo:local")
		dry := false
		_, err := test.client.Deploy(context.Background(), &servicePb.DeployRequest{
			StrategyName: "mean-rev",
			VersionId:    version.Id,
			DryRun:       &dry,
		})
		test.Require().Error(err)
		st, ok := status.FromError(err)
		test.True(ok)
		test.Equal(codes.FailedPrecondition, st.Code())
		test.Contains(st.Message(), "confirm")
		test.Empty(test.runner.added)
	})

	test.Run("rejects dry_run false when trading disabled", func() {
		version := test.seedDeployableVersion("mean-rev", "algo:local")
		dry := false
		_, err := test.client.Deploy(context.Background(), &servicePb.DeployRequest{
			StrategyName: "mean-rev",
			VersionId:    version.Id,
			DryRun:       &dry,
			Confirm:      true,
		})
		test.Require().Error(err)
		st, ok := status.FromError(err)
		test.True(ok)
		test.Equal(codes.FailedPrecondition, st.Code())
		test.Contains(st.Message(), "TRADING_ENABLED")
		test.Empty(test.runner.added)
	})

	test.Run("allows paper dry_run false with confirm and trading", func() {
		test.T().Setenv(environment.TradingEnabled, "true")
		test.startServer(paperProfiles())
		version := test.seedDeployableVersion("mean-rev", "algo:local")
		dry := false
		rsp, err := test.client.Deploy(context.Background(), &servicePb.DeployRequest{
			StrategyName: "mean-rev",
			VersionId:    version.Id,
			Backend:      "alpaca-paper",
			DryRun:       &dry,
			Confirm:      true,
			Symbols:      []string{"AAPL"},
		})
		test.Require().NoError(err)
		d := rsp.GetDeployment()
		test.Equal(pb.Deployment_PAPER, d.GetMode())
		test.False(d.GetDryRun())
		test.Equal("alpaca-paper", d.GetBackend())
		test.Equal(pb.Deployment_RUNNING, d.GetStatus())
		test.Require().Len(test.runner.added, 1)
		test.Equal(d.Id, test.runner.added[0].Id)
		test.False(test.runner.added[0].GetDryRun())
	})

	test.Run("rejects dry_run false with empty backend", func() {
		test.T().Setenv(environment.TradingEnabled, "true")
		test.startServer(paperProfiles())
		version := test.seedDeployableVersion("mean-rev", "algo:local")
		dry := false
		_, err := test.client.Deploy(context.Background(), &servicePb.DeployRequest{
			StrategyName: "mean-rev",
			VersionId:    version.Id,
			DryRun:       &dry,
			Confirm:      true,
		})
		test.Require().Error(err)
		st, ok := status.FromError(err)
		test.True(ok)
		test.Equal(codes.FailedPrecondition, st.Code())
		test.Contains(st.Message(), "profile")
		test.Empty(test.runner.added)
	})

	test.Run("rejects dry_run false with unknown backend", func() {
		test.T().Setenv(environment.TradingEnabled, "true")
		test.startServer(paperProfiles())
		version := test.seedDeployableVersion("mean-rev", "algo:local")
		dry := false
		_, err := test.client.Deploy(context.Background(), &servicePb.DeployRequest{
			StrategyName: "mean-rev",
			VersionId:    version.Id,
			Backend:      "no-such",
			DryRun:       &dry,
			Confirm:      true,
		})
		test.Require().Error(err)
		st, ok := status.FromError(err)
		test.True(ok)
		test.Equal(codes.FailedPrecondition, st.Code())
		test.Contains(st.Message(), "no-such")
		test.Empty(test.runner.added)
	})

	test.Run("requires completed experiment", func() {
		version := test.seedVersion("mean-rev", "algo:local", service.Service_Status_READY)
		_, err := test.client.Deploy(context.Background(), &servicePb.DeployRequest{
			StrategyName: "mean-rev",
			VersionId:    version.Id,
		})
		test.Require().Error(err)
		st, ok := status.FromError(err)
		test.True(ok)
		test.Equal(codes.FailedPrecondition, st.Code())
		test.Contains(st.Message(), "completed experiment")
	})

	test.Run("one running deployment per strategy", func() {
		version := test.seedDeployableVersion("mean-rev", "algo:local")
		_, err := test.client.Deploy(context.Background(), &servicePb.DeployRequest{
			StrategyName: "mean-rev",
			VersionId:    version.Id,
		})
		test.Require().NoError(err)

		_, err = test.client.Deploy(context.Background(), &servicePb.DeployRequest{
			StrategyName: "mean-rev",
			VersionId:    version.Id,
		})
		test.Require().Error(err)
		st, ok := status.FromError(err)
		test.True(ok)
		test.Equal(codes.FailedPrecondition, st.Code())
		test.Contains(st.Message(), "already has a running deployment")
	})
}

func (test *StrategyServerTest) TestStopAndListDeploymentRuns() {
	test.Run("stop is idempotent and lists runs", func() {
		version := test.seedDeployableVersion("mean-rev", "algo:local")
		deployed, err := test.client.Deploy(context.Background(), &servicePb.DeployRequest{
			StrategyName: "mean-rev",
			VersionId:    version.Id,
			Symbols:      []string{"SPY"},
		})
		test.Require().NoError(err)

		runs := repository.DeploymentRun{Conn: test.pgx}
		_, err = runs.Create(context.Background(), deployed.Deployment.Id,
			&common.Date{Year: 2024, Month: 3, Day: 4}, nil, false, nil)
		test.Require().NoError(err)

		stopped, err := test.client.StopDeployment(context.Background(), &servicePb.StopDeploymentRequest{
			StrategyName: "mean-rev",
		})
		test.Require().NoError(err)
		test.Equal(pb.Deployment_STOPPED, stopped.GetDeployment().GetStatus())
		test.Equal([]string{deployed.Deployment.Id}, test.runner.removed)

		again, err := test.client.StopDeployment(context.Background(), &servicePb.StopDeploymentRequest{
			StrategyName: "mean-rev",
		})
		test.Require().NoError(err)
		test.Equal(pb.Deployment_STOPPED, again.GetDeployment().GetStatus())

		listed, err := test.client.ListDeploymentRuns(context.Background(), &servicePb.ListDeploymentRunsRequest{
			StrategyName: "mean-rev",
		})
		test.Require().NoError(err)
		test.Len(listed.Runs, 1)
		test.Equal(deployed.Deployment.Id, listed.Runs[0].DeploymentId)
		test.False(listed.Runs[0].Skipped)
	})
}

// A runner that can't start the loop leaves the deployment ERROR with the
// cause, and the caller gets the sentence naming the deployment.
func (test *StrategyServerTest) TestDeployRunnerAddFails() {
	test.Run("runner add fails", func() {
		version := test.seedDeployableVersion("mean-rev", "algo:local")
		test.runner.addErr = errors.New("loop table full")
		_, err := test.client.Deploy(context.Background(), &servicePb.DeployRequest{
			StrategyName: "mean-rev",
			VersionId:    version.Id,
		})
		st, ok := status.FromError(err)
		test.Require().True(ok)
		test.Equal(codes.Internal, st.Code())
		test.Equal("Couldn't start deployment 'mean-rev'. Run `fbenv status` to check that the environment is running, "+
			"then run `fbull strategy deploy mean-rev` again.", st.Message())

		deployments := repository.Deployment{Conn: test.pgx}
		list, err := deployments.List(context.Background(), "mean-rev")
		test.Require().NoError(err)
		test.Require().Len(list, 1)
		test.Equal(pb.Deployment_ERROR, list[0].GetStatus())
		test.Equal("loop table full", list[0].GetError())
	})
}

// A runner that can't start and an ERROR that can't be saved either: the
// row still reads RUNNING, so the sentence says so and stops it first.
func (test *StrategyServerTest) TestDeployRunnerAddFailsAndErrorNotStored() {
	test.Run("error not stored", func() {
		version := test.seedDeployableVersion("mean-rev", "algo:local")
		_, err := test.pgx.Exec(context.Background(), `
			CREATE OR REPLACE FUNCTION refuse_deployment_error() RETURNS trigger AS $$
			BEGIN RAISE EXCEPTION 'disk full'; END $$ LANGUAGE plpgsql;
			CREATE TRIGGER refuse_deployment_error BEFORE UPDATE ON strategy_deployment
			FOR EACH ROW WHEN (NEW.error IS NOT NULL) EXECUTE FUNCTION refuse_deployment_error();`)
		test.Require().NoError(err)
		test.runner.addErr = errors.New("loop table full")

		_, err = test.client.Deploy(context.Background(), &servicePb.DeployRequest{
			StrategyName: "mean-rev",
			VersionId:    version.Id,
		})
		st, ok := status.FromError(err)
		test.Require().True(ok)
		test.Equal(codes.Internal, st.Code())
		test.Equal("Couldn't start deployment 'mean-rev', and the failure couldn't be saved, so the deployment still shows as running. "+
			"Run `fbenv status` to check that the environment is running, "+
			"then run `fbull strategy stop mean-rev`, then `fbull strategy deploy mean-rev`.", st.Message())

		deployments := repository.Deployment{Conn: test.pgx}
		list, err := deployments.List(context.Background(), "mean-rev")
		test.Require().NoError(err)
		test.Require().Len(list, 1)
		test.Equal(pb.Deployment_RUNNING, list[0].GetStatus())
	})
}

// experimentStream is a CreateExperiment server stream whose Send fails
// on call failAt (1-based; 0 never fails).
type experimentStream struct {
	grpc.ServerStream
	ctx    context.Context
	sends  int
	failAt int
}

func (s *experimentStream) Context() context.Context { return s.ctx }

func (s *experimentStream) Send(*servicePb.CreateExperimentResponse) error {
	s.sends++
	if s.sends == s.failAt {
		return errors.New("client went away")
	}
	return nil
}

// A RUNNING step that can't be sent stops the experiment before the run,
// and the row is FAILED instead of left RUNNING.
func (test *StrategyServerTest) TestCreateExperimentRunningNotSent() {
	test.Run("running not sent", func() {
		version := test.seedVersion("mean-rev", "algo:local", service.Service_Status_READY)
		test.insertBacktest("spy")
		srv := api.NewStrategyServer(test.pgx, test.launcher, test.runner)

		err := srv.CreateExperiment(&servicePb.CreateExperimentRequest{
			StrategyName: "mean-rev",
			VersionId:    version.Id,
			BacktestName: "spy",
		}, &experimentStream{ctx: context.Background(), failAt: 3})
		st, ok := status.FromError(err)
		test.Require().True(ok)
		test.Equal(codes.Internal, st.Code())
		test.Equal("Couldn't run experiment 'mean-rev': fbull couldn't be told the run had started. "+
			"Run `fbenv status` to check that the environment is running, "+
			"then run `fbull strategy experiment mean-rev --backtest spy` again.", st.Message())
		test.launcher.AssertNotCalled(test.T(), "Run", mock.Anything, mock.Anything)

		experiments := repository.Experiment{Conn: test.pgx}
		list, err := experiments.List(context.Background(), "mean-rev", "", "")
		test.Require().NoError(err)
		test.Require().Len(list, 1)
		test.Equal(pb.Experiment_FAILED, list[0].GetStatus())
		test.Equal("send running: client went away", list[0].GetError())
	})
}

// A client that goes away during the run still gets the row COMPLETED with
// its execution: neither write runs on the request's context.
func (test *StrategyServerTest) TestCreateExperimentCompletesAfterClientLeft() {
	test.Run("client left during run", func() {
		version := test.seedVersion("mean-rev", "algo:local", service.Service_Status_READY)
		executionID := test.insertExecution("spy")
		srv := api.NewStrategyServer(test.pgx, test.launcher, test.runner)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		test.launcher.On("Run", mock.Anything, mock.Anything).
			Run(func(mock.Arguments) { cancel() }).
			Return(&ss.RunOutput{ExecutionID: executionID}, nil)

		err := srv.CreateExperiment(&servicePb.CreateExperimentRequest{
			StrategyName: "mean-rev",
			VersionId:    version.Id,
			BacktestName: "spy",
		}, &experimentStream{ctx: ctx})
		test.Require().Error(err)

		experiments := repository.Experiment{Conn: test.pgx}
		list, err := experiments.List(context.Background(), "mean-rev", "", "")
		test.Require().NoError(err)
		test.Require().Len(list, 1)
		test.Equal(pb.Experiment_COMPLETED, list[0].GetStatus())
		test.Equal(executionID, list[0].GetExecutionId())
	})
}

// A runner that can't stop the loop fails StopDeployment with a sentence
// instead of reporting a clean stop.
func (test *StrategyServerTest) TestStopDeploymentRunnerRemoveFails() {
	test.Run("runner remove fails", func() {
		version := test.seedDeployableVersion("mean-rev", "algo:local")
		_, err := test.client.Deploy(context.Background(), &servicePb.DeployRequest{
			StrategyName: "mean-rev",
			VersionId:    version.Id,
		})
		test.Require().NoError(err)
		test.runner.removeErr = errors.New("loop is busy")

		_, err = test.client.StopDeployment(context.Background(), &servicePb.StopDeploymentRequest{
			StrategyName: "mean-rev",
		})
		st, ok := status.FromError(err)
		test.Require().True(ok)
		test.Equal(codes.Internal, st.Code())
		test.Equal("Couldn't stop deployment 'mean-rev'. Run `fbenv status` to check that the environment is running, "+
			"then run `fbull strategy stop mean-rev` again.", st.Message())

		// The loop didn't stop, so the row isn't STOPPED either.
		deployments := repository.Deployment{Conn: test.pgx}
		list, err := deployments.List(context.Background(), "mean-rev")
		test.Require().NoError(err)
		test.Require().Len(list, 1)
		test.Equal(pb.Deployment_RUNNING, list[0].GetStatus())
	})
}

// A client that goes away while the loop stops still gets the row STOPPED:
// neither the wait for the loop nor the STOPPED write runs on the request's
// context.
func (test *StrategyServerTest) TestStopDeploymentStopsAfterClientLeft() {
	test.Run("client left during stop", func() {
		version := test.seedDeployableVersion("mean-rev", "algo:local")
		_, err := test.client.Deploy(context.Background(), &servicePb.DeployRequest{
			StrategyName: "mean-rev",
			VersionId:    version.Id,
		})
		test.Require().NoError(err)
		srv := api.NewStrategyServer(test.pgx, test.launcher, test.runner)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		test.runner.onRemove = cancel

		rsp, err := srv.StopDeployment(ctx, &servicePb.StopDeploymentRequest{StrategyName: "mean-rev"})
		test.Require().NoError(err)
		test.Equal(pb.Deployment_STOPPED, rsp.GetDeployment().GetStatus())

		deployments := repository.Deployment{Conn: test.pgx}
		list, err := deployments.List(context.Background(), "mean-rev")
		test.Require().NoError(err)
		test.Require().Len(list, 1)
		test.Equal(pb.Deployment_STOPPED, list[0].GetStatus())
	})
}
