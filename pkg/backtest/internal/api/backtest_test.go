package api_test

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lhjnilsson/foreverbull/internal/environment"
	internalGrpc "github.com/lhjnilsson/foreverbull/internal/grpc"
	"github.com/lhjnilsson/foreverbull/internal/stream"
	streamPb "github.com/lhjnilsson/foreverbull/internal/stream/pb"
	"github.com/lhjnilsson/foreverbull/internal/test_helper"
	"github.com/lhjnilsson/foreverbull/pkg/backtest/internal/api"
	"github.com/lhjnilsson/foreverbull/pkg/backtest/internal/repository"
	"github.com/lhjnilsson/foreverbull/pkg/pb"
	"github.com/lhjnilsson/foreverbull/pkg/pb/backtest"
	servicePb "github.com/lhjnilsson/foreverbull/pkg/pb/backtest/service"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/suite"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

type BacktestServerTest struct {
	suite.Suite

	pgx    *pgxpool.Pool
	stream *stream.MockStream

	listener *bufconn.Listener
	server   *grpc.Server
	client   servicePb.BacktestServicerClient
}

func TestBacktestServerTest(t *testing.T) {
	suite.Run(t, new(BacktestServerTest))
}

func (test *BacktestServerTest) SetupSuite() {
	test_helper.SetupEnvironment(test.T(), &test_helper.Containers{
		Postgres: true,
	})
}

func (test *BacktestServerTest) SetupSubTest() {
	var err error
	test.pgx, err = pgxpool.New(context.Background(), environment.GetPostgresURL())
	test.Require().NoError(err)
	test.Require().NoError(repository.Recreate(context.TODO(), test.pgx))

	test.listener = bufconn.Listen(1024 * 1024)
	test.server, err = internalGrpc.NewServer()
	test.Require().NoError(err)

	test.stream = new(stream.MockStream)
	server := api.NewBacktestServer(test.pgx, test.stream)
	servicePb.RegisterBacktestServicerServer(test.server, server)

	go func() {
		test.server.Serve(test.listener) // nolint:errcheck
	}()

	resolver.SetDefaultScheme("passthrough")

	conn, err := grpc.NewClient(test.listener.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(_ context.Context, _ string) (net.Conn, error) {
			return test.listener.Dial()
		}),
	)
	if err != nil {
		log.Printf("error connecting to server: %v", err)
	}

	test.client = servicePb.NewBacktestServicerClient(conn)
}

func (test *BacktestServerTest) TearDownSubTest() {
	err := test.listener.Close()
	if err != nil {
		test.T().Errorf("Error closing listener: %v", err)
	}

	test.server.Stop()
}

func (test *BacktestServerTest) createBacktest(name string) *backtest.Backtest {
	test.T().Helper()

	backtests := repository.Backtest{Conn: test.pgx}
	b := backtest.Backtest{
		Name: name,
	}
	backtest, err := backtests.Create(context.TODO(), &b)
	test.Require().NoError(err)
	test.Require().NotNil(backtest)
	return backtest
}

func (test *BacktestServerTest) createSession(backtest string) *backtest.Session {
	test.T().Helper()

	sessions := repository.Session{Conn: test.pgx}
	session, err := sessions.Create(context.TODO(), backtest)
	test.Require().NoError(err)
	test.Require().NotNil(session)

	return session
}

func (test *BacktestServerTest) createExecution(session string) *backtest.Execution {
	test.T().Helper()

	executions := repository.Execution{Conn: test.pgx}
	e := backtest.Execution{
		Session:   session,
		StartDate: &pb.Date{Year: 2024, Month: 1, Day: 1},
		EndDate:   &pb.Date{Year: 2024, Month: 1, Day: 1},
	}

	execution, err := executions.Create(context.TODO(), &e, nil)
	test.Require().NoError(err)
	test.Require().NotNil(execution)

	return execution
}

func (test *BacktestServerTest) TestListBacktests() {
	test.Run("List", func() {
		req := &servicePb.ListBacktestsRequest{}

		resp, err := test.client.ListBacktests(context.Background(), req)
		test.Require().NoError(err)
		test.NotNil(resp)
	})
}

func (test *BacktestServerTest) TestCreateBacktest() {
	test.Run("From ETF", func() {
		statuses := []*streamPb.Orchestration_Step{
			{Num: 2, State: pb.State_STATE_RUNNING},
			{Num: 2, State: pb.State_STATE_COMPLETED},
		}

		statusCh := make(chan *streamPb.Orchestration_Step, len(statuses))
		for _, status := range statuses {
			statusCh <- status
		}
		close(statusCh)
		test.stream.On("RunOrchestration", mock.Anything, mock.Anything).Return((<-chan *streamPb.Orchestration_Step)(statusCh))

		req := &servicePb.CreateBacktestRequest{
			Create: &servicePb.CreateBacktestRequest_Etf{
				Etf: &servicePb.CreateBacktestRequest_CreateFromETF{
					Name:      "Test Backtest",
					StartDate: &pb.Date{Year: 2022, Month: 1, Day: 1},
					EndDate:   &pb.Date{Year: 2022, Month: 12, Day: 31},
					BaseEtf:   "iShares S&P 100",
				},
			},
		}

		resp, err := test.client.CreateBacktest(context.TODO(), req)
		test.Require().NoError(err)
		test.NotNil(resp)

		responses := []*servicePb.CreateBacktestResponse{}

		for {
			msg, err := resp.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			test.Require().NoError(err)
			test.Require().NotNil(msg)
			responses = append(responses, msg)
		}
		test.Require().Len(responses, 3)
	})
	test.Run("From Backtest", func() {
		statuses := []*streamPb.Orchestration_Step{
			{Num: 2, State: pb.State_STATE_RUNNING},
			{Num: 2, State: pb.State_STATE_COMPLETED},
		}

		statusCh := make(chan *streamPb.Orchestration_Step, len(statuses))
		for _, status := range statuses {
			statusCh <- status
		}
		close(statusCh)
		test.stream.On("RunOrchestration", mock.Anything, mock.Anything).Return((<-chan *streamPb.Orchestration_Step)(statusCh))

		req := &servicePb.CreateBacktestRequest{
			Create: &servicePb.CreateBacktestRequest_Backtest{
				Backtest: &servicePb.CreateBacktestRequest_CreateFromBacktest{
					Backtest: &backtest.Backtest{},
				},
			},
		}

		resp, err := test.client.CreateBacktest(context.TODO(), req)
		test.Require().NoError(err)
		test.NotNil(resp)

		responses := []*servicePb.CreateBacktestResponse{}

		for {
			msg, err := resp.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			test.Require().NoError(err)
			test.Require().NotNil(msg)
			responses = append(responses, msg)
		}
		test.Require().Len(responses, 3)
	})
	test.Run("Ingest notices reach the client in INGESTING.notices", func() {
		// The ingest stores the backtest before INGESTING completes, and the
		// server then loads it and sends it, so the test stores it first.
		test.createBacktest("noticed")
		lines := []string{"Example notice for XYZ on 2022-03-02."}
		responses := test.createStream("noticed", []*streamPb.Orchestration_Step{
			{Num: 3, State: pb.State_STATE_RUNNING},
			{Num: 3, State: pb.State_STATE_COMPLETED, Notices: lines},
		})
		test.Require().NotEmpty(responses)
		last := responses[len(responses)-1]
		test.Equal(pb.State_STATE_COMPLETED, last.GetINGESTING().GetState())
		test.Equal(lines, last.GetINGESTING().GetNotices())
		// message is the failure text; an older client that reads it sees
		// a completed step with no message.
		test.Empty(last.GetINGESTING().GetMessage())
		// The notices stay on the response that carries the stored backtest.
		test.Equal(pb.State_STATE_COMPLETED, last.GetSTORING().GetState())
		test.Require().NotNil(last.GetBacktest())
		test.Equal("noticed", last.GetBacktest().GetName())
	})
}

func (test *BacktestServerTest) createStream(name string, statuses []*streamPb.Orchestration_Step) []*servicePb.CreateBacktestResponse {
	test.T().Helper()

	statusCh := make(chan *streamPb.Orchestration_Step, len(statuses))
	for _, status := range statuses {
		statusCh <- status
	}
	close(statusCh)
	test.stream.On("RunOrchestration", mock.Anything, mock.Anything).Return((<-chan *streamPb.Orchestration_Step)(statusCh))

	req := &servicePb.CreateBacktestRequest{
		Create: &servicePb.CreateBacktestRequest_Backtest{
			Backtest: &servicePb.CreateBacktestRequest_CreateFromBacktest{
				Backtest: &backtest.Backtest{Name: name},
			},
		},
	}
	resp, err := test.client.CreateBacktest(context.TODO(), req)
	test.Require().NoError(err)

	responses := []*servicePb.CreateBacktestResponse{}
	for {
		msg, err := resp.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		test.Require().NoError(err)
		responses = append(responses, msg)
	}
	return responses
}

func (test *BacktestServerTest) TestCreateBacktestSendsStoredBacktest() {
	test.Run("completed ingest sends the backtest", func() {
		test.createBacktest("created")
		responses := test.createStream("created", []*streamPb.Orchestration_Step{
			{Num: 1, State: pb.State_STATE_COMPLETED},
			{Num: 2, State: pb.State_STATE_COMPLETED},
			{Num: 3, State: pb.State_STATE_COMPLETED},
		})
		test.Require().Len(responses, 5)
		last := responses[len(responses)-1]
		test.Equal(pb.State_STATE_COMPLETED, last.GetSTORING().GetState())
		test.Require().NotNil(last.GetBacktest())
		test.Equal("created", last.GetBacktest().GetName())
		for _, rsp := range responses[:len(responses)-1] {
			test.Nil(rsp.GetBacktest())
		}
	})
	test.Run("failed ingest sends no backtest", func() {
		test.createBacktest("failed")
		failure := "ingest failed"
		responses := test.createStream("failed", []*streamPb.Orchestration_Step{
			{Num: 1, State: pb.State_STATE_COMPLETED},
			{Num: 2, State: pb.State_STATE_COMPLETED},
			{Num: 3, State: pb.State_STATE_FAILED, Error: &failure},
		})
		test.Require().Len(responses, 4)
		for _, rsp := range responses {
			test.Nil(rsp.GetBacktest())
			test.NotEqual(pb.State_STATE_COMPLETED, rsp.GetSTORING().GetState())
		}
	})
}

func (test *BacktestServerTest) TestGetBacktest() {
	test.Run("not found", func() {
		req := &servicePb.GetBacktestRequest{
			Name: "not_found",
		}
		resp, err := test.client.GetBacktest(context.Background(), req)
		test.Require().Error(err)
		test.Equal(codes.NotFound, status.Code(err))
		test.NotContains(err.Error(), "no rows in result set")
		test.Nil(resp)
	})
	test.Run("found", func() {
		backtest := test.createBacktest("test_1")

		req := &servicePb.GetBacktestRequest{
			Name: backtest.Name,
		}

		resp, err := test.client.GetBacktest(context.Background(), req)
		test.Require().NoError(err)
		test.NotNil(resp)
		test.Equal(backtest.Name, resp.GetBacktest().Name)
	})
}

func (test *BacktestServerTest) TestDeleteBacktest() {
	test.Run("not found", func() {
		req := &servicePb.DeleteBacktestRequest{
			Name: "not_found",
		}
		resp, err := test.client.DeleteBacktest(context.Background(), req)
		test.Require().Error(err)
		test.Equal(codes.NotFound, status.Code(err))
		test.Nil(resp)
	})
	test.Run("found", func() {
		backtests := test.createBacktest("test_1")
		req := &servicePb.DeleteBacktestRequest{
			Name: backtests.Name,
		}
		resp, err := test.client.DeleteBacktest(context.Background(), req)
		test.Require().NoError(err)
		test.Require().NotNil(resp)
	})
}

func (test *BacktestServerTest) TestCreateSession() {
	test.Run("backtest not found", func() {
		req := &servicePb.CreateSessionRequest{
			BacktestName: "backtest",
		}

		resp, err := test.client.CreateSession(context.TODO(), req)
		test.Require().NoError(err)
		test.Require().NotNil(resp)

		msg, err := resp.Recv()
		test.Require().NoError(err)
		test.Require().NotNil(msg)
	})

	test.Run("normal", func() {
		backtest := test.createBacktest("test_1")

		test.stream.On("Publish", mock.Anything, mock.Anything, mock.Anything).Return(nil)

		req := &servicePb.CreateSessionRequest{
			BacktestName: backtest.Name,
		}

		resp, err := test.client.CreateSession(context.Background(), req)
		test.Require().NoError(err)
		test.NotNil(resp)

		responses := []*servicePb.CreateSessionResponse{}
		for {
			msg, err := resp.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			test.Require().NoError(err)
			test.Require().NotNil(msg)
			responses = append(responses, msg)
		}
		test.Require().Len(responses, 4)
	})
}

func (test *BacktestServerTest) TestGetSession() {
	test.Run("not found", func() {
		req := &servicePb.GetSessionRequest{
			SessionId: "not_found",
		}
		resp, err := test.client.GetSession(context.Background(), req)
		test.Require().Error(err)
		test.Nil(resp)
	})
	test.Run("found", func() {
		backtest := test.createBacktest("test_1")
		session := test.createSession(backtest.Name)

		req := &servicePb.GetSessionRequest{
			SessionId: session.Id,
		}

		resp, err := test.client.GetSession(context.Background(), req)
		test.Require().NoError(err)
		test.NotNil(resp)
	})
}

func (test *BacktestServerTest) TestListExecutions() {
	test.Run("List by session", func() {
		backtest := test.createBacktest("test_1")
		session := test.createSession(backtest.Name)
		execution := test.createExecution(session.Id)

		req := &servicePb.ListExecutionsRequest{
			SessionId: session.Id,
		}
		resp, err := test.client.ListExecutions(context.Background(), req)
		test.Require().NoError(err)
		test.NotNil(resp)
		test.Len(resp.Executions, 1)
		test.Equal(execution.Id, resp.Executions[0].Id)
	})
	test.Run("List by backtest", func() {
		backtest := test.createBacktest("test_1")
		session := test.createSession(backtest.Name)
		execution := test.createExecution(session.Id)

		req := &servicePb.ListExecutionsRequest{
			Backtest: backtest.Name,
		}
		resp, err := test.client.ListExecutions(context.Background(), req)
		test.Require().NoError(err)
		test.NotNil(resp)
		test.Len(resp.Executions, 1)
		test.Equal(execution.Id, resp.Executions[0].Id)
	})
	test.Run("List without filter", func() {
		backtest := test.createBacktest("test_1")
		session := test.createSession(backtest.Name)
		execution := test.createExecution(session.Id)

		req := &servicePb.ListExecutionsRequest{}
		resp, err := test.client.ListExecutions(context.Background(), req)
		test.Require().NoError(err)
		test.NotNil(resp)
		test.Len(resp.Executions, 1)
		test.Equal(execution.Id, resp.Executions[0].Id)
	})
}

func (test *BacktestServerTest) TestGetExecution() {
	test.Run("not found", func() {
		req := &servicePb.GetExecutionRequest{
			ExecutionId: "not_found",
		}
		resp, err := test.client.GetExecution(context.Background(), req)
		test.Require().Error(err)
		test.Nil(resp)
	})
	test.Run("found", func() {
		backtest := test.createBacktest("test_1")
		session := test.createSession(backtest.Name)
		execution := test.createExecution(session.Id)

		req := &servicePb.GetExecutionRequest{
			ExecutionId: execution.Id,
		}
		resp, err := test.client.GetExecution(context.Background(), req)
		test.Require().NoError(err)
		test.NotNil(resp)
		test.Equal(execution, resp.Execution)
		test.Empty(resp.GetFunctions())
		test.Equal("yahoo", resp.GetProvider())
	})
	test.Run("stored notices", func() {
		backtest := test.createBacktest("test_1")
		session := test.createSession(backtest.Name)
		execution := test.createExecution(session.Id)
		line := "3 symbols were delisted after their last price on 2026-03-31 and were left out: ABC, DEF, GHI."
		executions := repository.Execution{Conn: test.pgx}
		test.Require().NoError(executions.SetNotices(context.TODO(), execution.Id, []string{line}))

		resp, err := test.client.GetExecution(context.Background(), &servicePb.GetExecutionRequest{ExecutionId: execution.Id})
		test.Require().NoError(err)
		test.Equal([]string{line}, resp.GetExecution().GetNotices())
	})
	test.Run("stored arguments", func() {
		backtestRow := test.createBacktest("test_1")
		session := test.createSession(backtestRow.Name)
		want := []*backtest.ExecutionFunction{
			{
				Name: "rebalance",
				Parameters: []*backtest.ExecutionFunction_Parameter{
					{Key: "fastperiod", Value: "12", ValueType: "int"},
					{Key: "cash_reserve", Value: "0.05", ValueType: "float"},
				},
			},
		}
		executions := repository.Execution{Conn: test.pgx}
		execution, err := executions.Create(context.TODO(), &backtest.Execution{
			Session:   session.Id,
			StartDate: &pb.Date{Year: 2024, Month: 1, Day: 1},
			EndDate:   &pb.Date{Year: 2024, Month: 1, Day: 1},
		}, want)
		test.Require().NoError(err)

		resp, err := test.client.GetExecution(context.Background(), &servicePb.GetExecutionRequest{
			ExecutionId: execution.Id,
		})
		test.Require().NoError(err)
		test.Equal(execution.Id, resp.GetExecution().GetId())
		test.Equal(session.Id, resp.GetExecution().GetSession())
		test.Equal(backtestRow.Name, resp.GetExecution().GetBacktest())
		test.Len(resp.GetFunctions(), len(want))
		for i := range want {
			test.True(proto.Equal(want[i], resp.GetFunctions()[i]))
		}
		test.Equal("yahoo", resp.GetProvider())
	})
}
