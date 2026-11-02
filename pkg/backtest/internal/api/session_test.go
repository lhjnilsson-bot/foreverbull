package api_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lhjnilsson/foreverbull/internal/auth"
	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/internal/test_helper"
	"github.com/lhjnilsson/foreverbull/pkg/backtest/engine"
	"github.com/lhjnilsson/foreverbull/pkg/backtest/internal/api"
	"github.com/lhjnilsson/foreverbull/pkg/backtest/internal/repository"
	"github.com/lhjnilsson/foreverbull/pkg/pb"
	"github.com/lhjnilsson/foreverbull/pkg/pb/backtest"
	servicePb "github.com/lhjnilsson/foreverbull/pkg/pb/backtest/service"
	service_pb "github.com/lhjnilsson/foreverbull/pkg/pb/service"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

type SessionTest struct {
	suite.Suite

	conn *pgxpool.Pool

	listener   *bufconn.Listener
	baseServer *grpc.Server
	activity   <-chan bool

	mockEngine        *engine.MockEngine
	mockEngineSession *engine.MockEngineSession
	client            servicePb.SessionServicerClient

	backtest *backtest.Backtest
	session  *backtest.Session
}

func TestSessionManual(t *testing.T) {
	suite.Run(t, new(SessionTest))
}

func (s *SessionTest) SetupSuite() {
	test_helper.SetupEnvironment(s.T(), &test_helper.Containers{
		Postgres: true,
	})

	var err error
	s.conn, err = pgxpool.New(context.Background(), environment.GetPostgresURL())
	s.Require().NoError(err)
}

func (s *SessionTest) TearDownSuite() {
}

func (s *SessionTest) SetupTest() {
	err := repository.Recreate(context.Background(), s.conn)
	s.Require().NoError(err)

	backtests := repository.Backtest{Conn: s.conn}
	backtest := backtest.Backtest{
		Name: "backtest",
		Periods: []*backtest.Backtest_Period{
			{StartDate: &pb.Date{Year: 2024, Month: 1, Day: 1}, EndDate: &pb.Date{Year: 2024, Month: 12, Day: 31}},
		},
	}
	s.backtest, err = backtests.Create(context.Background(), &backtest)
	s.Require().NoError(err)
	sessions := repository.Session{Conn: s.conn}
	s.session, err = sessions.Create(context.TODO(), "backtest")
	s.Require().NoError(err)

	s.listener = bufconn.Listen(1024 * 1024)

	s.mockEngine = new(engine.MockEngine)
	s.mockEngineSession = new(engine.MockEngineSession)
	s.mockEngine.On("NewSession", mock.Anything, mock.Anything).Return(s.mockEngineSession, nil)

	s.baseServer, s.activity, err = api.NewGRPCSessionServer(s.session, s.conn, s.mockEngine)
	s.Require().NoError(err)

	go func() {
		if err := s.baseServer.Serve(s.listener); err != nil {
			log.Printf("error serving server: %v", err)
		}
	}()

	resolver.SetDefaultScheme("passthrough")

	conn, err := grpc.NewClient(s.listener.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(_ context.Context, _ string) (net.Conn, error) {
			return s.listener.Dial()
		}),
	)
	if err != nil {
		log.Printf("error connecting to server: %v", err)
	}

	s.client = servicePb.NewSessionServicerClient(conn)
}

func (s *SessionTest) TearDownTest() {
	err := s.listener.Close()
	if err != nil {
		s.Failf("error closing listener: %s", err.Error())
	}

	s.baseServer.Stop()
}

func (s *SessionTest) TestCreateExecution() {
	rsp, err := s.client.CreateExecution(context.Background(), &servicePb.CreateExecutionRequest{
		Id:        "execution_123",
		Session:   s.session.Id,
		StartDate: &pb.Date{Year: 2024, Month: 1, Day: 1},
		EndDate:   &pb.Date{Year: 2024, Month: 1, Day: 1},
		Algorithm: &service_pb.Algorithm{},
	})
	s.Require().NoError(err)
	s.Require().NotNil(rsp)
	select {
	case <-s.activity:
	case <-time.After(5 * time.Second):
		s.Require().Fail("timeout waiting for activity")
	}
}

func (s *SessionTest) TestCreateExecutionStoresUsedArguments() {
	rsp, err := s.client.CreateExecution(context.Background(), &servicePb.CreateExecutionRequest{
		Session:   s.session.Id,
		StartDate: &pb.Date{Year: 2024, Month: 1, Day: 2},
		EndDate:   &pb.Date{Year: 2024, Month: 6, Day: 30},
		Algorithm: &service_pb.Algorithm{
			Functions: []*service_pb.Algorithm_Function{
				{
					Name: "rebalance",
					Parameters: []*service_pb.Algorithm_FunctionParameter{
						{Key: "fastperiod", Value: strp("12"), DefaultValue: strp("10"), ValueType: "int"},
						{Key: "slowperiod", DefaultValue: strp("26"), ValueType: "int"},
						{Key: "signalperiod", ValueType: "int"},
						{Key: "oversold", Value: strp(""), DefaultValue: strp("30"), ValueType: "int"},
					},
				},
				{Name: "screen"},
			},
		},
	})
	s.Require().NoError(err)
	s.Require().NotNil(rsp.GetExecution())

	select {
	case <-s.activity:
	case <-time.After(5 * time.Second):
		s.Require().Fail("timeout waiting for activity")
	}

	want := []*backtest.ExecutionFunction{
		{
			Name: "rebalance",
			Parameters: []*backtest.ExecutionFunction_Parameter{
				{Key: "fastperiod", Value: "12", ValueType: "int"},
				{Key: "slowperiod", Value: "26", ValueType: "int"},
				{Key: "signalperiod", Value: "", ValueType: "int"},
				{Key: "oversold", Value: "", ValueType: "int"},
			},
		},
		{Name: "screen", Parameters: []*backtest.ExecutionFunction_Parameter{}},
	}

	executions := repository.Execution{Conn: s.conn}
	stored, err := executions.GetFunctions(context.Background(), rsp.Execution.Id)
	s.Require().NoError(err)
	s.Len(stored, len(want))
	for i := range want {
		s.True(proto.Equal(want[i], stored[i]))
	}

	reader := api.NewBacktestServer(s.conn, nil)
	got, err := reader.GetExecution(context.Background(), &servicePb.GetExecutionRequest{ExecutionId: rsp.Execution.Id})
	s.Require().NoError(err)
	s.Equal(rsp.Execution.Id, got.GetExecution().GetId())
	s.Equal(s.session.Id, got.GetExecution().GetSession())
	s.Equal("backtest", got.GetExecution().GetBacktest())
	s.Equal(int32(2024), got.GetExecution().GetStartDate().GetYear())
	s.Equal(int32(1), got.GetExecution().GetStartDate().GetMonth())
	s.Equal(int32(2), got.GetExecution().GetStartDate().GetDay())
	s.Equal(int32(2024), got.GetExecution().GetEndDate().GetYear())
	s.Equal(int32(6), got.GetExecution().GetEndDate().GetMonth())
	s.Equal(int32(30), got.GetExecution().GetEndDate().GetDay())
	s.Len(got.GetFunctions(), len(want))
	for i := range want {
		s.True(proto.Equal(want[i], got.GetFunctions()[i]))
	}
	s.Equal("yahoo", got.GetProvider())
}

func strp(v string) *string {
	return &v
}

func (s *SessionTest) TestRunExecution() {
	rsp, err := s.client.CreateExecution(context.Background(), &servicePb.CreateExecutionRequest{
		Id:        "execution_123",
		Session:   s.session.Id,
		StartDate: &pb.Date{Year: 2024, Month: 1, Day: 1},
		EndDate:   &pb.Date{Year: 2024, Month: 1, Day: 1},
		Algorithm: &service_pb.Algorithm{},
	})
	s.Require().NoError(err)
	s.Require().NotNil(rsp)
	select {
	case <-s.activity:
	case <-time.After(5 * time.Second):
		s.Require().Fail("timeout waiting for activity")
	}

	executions := repository.Execution{Conn: s.conn}
	e := backtest.Execution{
		Session:   s.session.Id,
		StartDate: &pb.Date{Year: 2024, Month: 1, Day: 1},
		EndDate:   &pb.Date{Year: 2024, Month: 1, Day: 1},
	}
	execution, err := executions.Create(context.Background(), &e, nil)
	s.Require().NoError(err)

	ch := make(chan *servicePb.RunExecutionResponse, 5)
	ch <- &servicePb.RunExecutionResponse{}
	ch <- &servicePb.RunExecutionResponse{}
	ch <- &servicePb.RunExecutionResponse{}
	ch <- &servicePb.RunExecutionResponse{}
	ch <- &servicePb.RunExecutionResponse{}
	close(ch)
	s.mockEngineSession.On("RunBacktest", mock.Anything, mock.Anything, mock.Anything).Return(ch, nil)

	stream, err := s.client.RunExecution(context.Background(), &servicePb.RunExecutionRequest{
		ExecutionId: execution.Id,
	})
	s.Require().NoError(err)
	s.Require().NotNil(stream)

	entries := 0

	for {
		_, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}

		s.Require().NoError(err)

		entries++
	}

	s.Require().Equal(5, entries)
}

// Zipline's sentence for a refused run reaches the client unchanged, through
// the server's client-facing interceptor, so fbull prints it as written.
func (s *SessionTest) TestRunExecutionShowsZiplinesRefusalAsWritten() {
	sentences := []string{
		"Couldn't run backtest 'backtest': the benchmark SPY has no prices on 3 trading days: " +
			"2024-02-14, 2024-02-20 to 2024-02-21. Nothing was run. " +
			"Run `fbull backtest create` again to reload its prices, or choose another benchmark.",
		"The price data for backtest 'backtest' can't be read: its version marker is damaged. " +
			"Run `fbull backtest create` again to rebuild it.",
	}
	for index, sentence := range sentences {
		executionID := fmt.Sprintf("refused_%d", index)
		_, err := s.client.CreateExecution(context.Background(), &servicePb.CreateExecutionRequest{
			Id:        executionID,
			Session:   s.session.Id,
			StartDate: &pb.Date{Year: 2024, Month: 1, Day: 1},
			EndDate:   &pb.Date{Year: 2024, Month: 1, Day: 1},
			Algorithm: &service_pb.Algorithm{},
		})
		s.Require().NoError(err)
		executions := repository.Execution{Conn: s.conn}
		execution, err := executions.Create(context.Background(), &backtest.Execution{
			Session:   s.session.Id,
			StartDate: &pb.Date{Year: 2024, Month: 1, Day: 1},
			EndDate:   &pb.Date{Year: 2024, Month: 1, Day: 1},
		}, nil)
		s.Require().NoError(err)
		s.mockEngineSession.On("RunBacktest", mock.Anything, mock.Anything, mock.Anything).
			Return(nil, status.Error(codes.FailedPrecondition, sentence)).Once()

		stream, err := s.client.RunExecution(context.Background(), &servicePb.RunExecutionRequest{ExecutionId: execution.Id})
		s.Require().NoError(err)
		_, err = stream.Recv()
		refused, ok := status.FromError(err)
		s.Require().True(ok)
		s.Equal(codes.FailedPrecondition, refused.Code())
		s.Equal(sentence, refused.Message())
	}
}

// noticingSession is an engine session that reports run notices, as
// the Zipline session does when it left a member out after its last price.
type noticingSession struct {
	*engine.MockEngineSession
	notices []string
}

func (n noticingSession) RunNotices() []string { return n.notices }

// The run's notices are stored on the execution before the stream ends, so
// the summary the CLI reads right after names the members left out.
func (s *SessionTest) TestRunExecutionStoresRunNotices() {
	line := "1 symbol was delisted after its last price on 2024-03-28 and was left out: ABC."
	session := noticingSession{MockEngineSession: new(engine.MockEngineSession), notices: []string{line}}
	ch := make(chan *servicePb.RunExecutionResponse, 1)
	ch <- &servicePb.RunExecutionResponse{}
	close(ch)
	session.On("RunBacktest", mock.Anything, mock.Anything, mock.Anything).Return(ch, nil)
	mockEngine := new(engine.MockEngine)
	mockEngine.On("NewSession", mock.Anything, mock.Anything).Return(session, nil)

	server, _, err := api.NewGRPCSessionServer(s.session, s.conn, mockEngine)
	s.Require().NoError(err)
	listener := bufconn.Listen(1024 * 1024)
	go func() {
		if err := server.Serve(listener); err != nil {
			log.Printf("error serving server: %v", err)
		}
	}()
	defer server.Stop()
	conn, err := grpc.NewClient(listener.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(_ context.Context, _ string) (net.Conn, error) {
			return listener.Dial()
		}),
	)
	s.Require().NoError(err)
	defer conn.Close() //nolint: errcheck
	client := servicePb.NewSessionServicerClient(conn)

	executions := repository.Execution{Conn: s.conn}
	execution, err := executions.Create(context.Background(), &backtest.Execution{
		Session:   s.session.Id,
		StartDate: &pb.Date{Year: 2024, Month: 1, Day: 1},
		EndDate:   &pb.Date{Year: 2024, Month: 12, Day: 31},
	}, nil)
	s.Require().NoError(err)

	stream, err := client.RunExecution(context.Background(), &servicePb.RunExecutionRequest{ExecutionId: execution.Id})
	s.Require().NoError(err)
	for {
		_, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		s.Require().NoError(err)
	}

	notices, err := executions.GetNotices(context.Background(), execution.Id)
	s.Require().NoError(err)
	s.Equal([]string{line}, notices)
}

func (s *SessionTest) TestStopServer() {
	rsp, err := s.client.StopServer(context.Background(), &servicePb.StopServerRequest{})
	s.Require().NoError(err)
	s.Require().NotNil(rsp)
	select {
	case _, done := <-s.activity:
		s.Require().False(done)
	default:
		s.Require().Fail("activity channel should be closed")
	}
}

// A run outside the backtest's dates has no universe and no bars, so the
// broker refuses it instead of returning a flat result.
func (s *SessionTest) TestCreateExecutionRejectsDatesOutsideBacktest() {
	cases := []struct {
		name  string
		start *pb.Date
		end   *pb.Date
		want  string
	}{
		{
			name:  "start before first period",
			start: &pb.Date{Year: 2023, Month: 12, Day: 29},
			end:   &pb.Date{Year: 2024, Month: 6, Day: 28},
			want: "Start 2023-12-29 is before backtest 'backtest' begins. " +
				"Pick dates between 2024-01-01 and 2024-12-31, or leave out the dates to run the whole backtest.",
		},
		{
			name:  "end after last period",
			start: &pb.Date{Year: 2024, Month: 1, Day: 2},
			end:   &pb.Date{Year: 2025, Month: 1, Day: 3},
			want: "End 2025-01-03 is after backtest 'backtest' ends. " +
				"Pick dates between 2024-01-01 and 2024-12-31, or leave out the dates to run the whole backtest.",
		},
		{
			name:  "start after end",
			start: &pb.Date{Year: 2024, Month: 6, Day: 28},
			end:   &pb.Date{Year: 2024, Month: 1, Day: 2},
			// Both dates are inside the backtest, so its range adds nothing.
			want: "Start 2024-06-28 is after end 2024-01-02. Pick a start on or before the end.",
		},
	}
	for _, tc := range cases {
		s.Run(tc.name, func() {
			_, err := s.client.CreateExecution(context.Background(), &servicePb.CreateExecutionRequest{
				Session:   s.session.Id,
				StartDate: tc.start,
				EndDate:   tc.end,
				Algorithm: &service_pb.Algorithm{},
			})
			s.Require().Error(err)
			st, ok := status.FromError(err)
			s.Require().True(ok)
			s.Equal(codes.InvalidArgument, st.Code(), st.Message())
			s.Equal(tc.want, st.Message())
		})
	}

	var stored int
	err := s.conn.QueryRow(context.Background(), `SELECT count(*) FROM backtest_execution`).Scan(&stored)
	s.Require().NoError(err)
	s.Zero(stored, "a rejected request stores no execution")
}

func (s *SessionTest) TestCreateExecutionAcceptsBacktestBounds() {
	rsp, err := s.client.CreateExecution(context.Background(), &servicePb.CreateExecutionRequest{
		Session:   s.session.Id,
		StartDate: &pb.Date{Year: 2024, Month: 1, Day: 1},
		EndDate:   &pb.Date{Year: 2024, Month: 12, Day: 31},
		Algorithm: &service_pb.Algorithm{},
	})
	s.Require().NoError(err)
	s.Equal("2024-01-01", pb.DateToDateString(rsp.GetExecution().GetStartDate()))
	s.Equal("2024-12-31", pb.DateToDateString(rsp.GetExecution().GetEndDate()))
}

// A request without dates runs over the whole backtest.
func (s *SessionTest) TestCreateExecutionWithoutDatesUsesBacktestRange() {
	rsp, err := s.client.CreateExecution(context.Background(), &servicePb.CreateExecutionRequest{
		Session:   s.session.Id,
		Algorithm: &service_pb.Algorithm{},
	})
	s.Require().NoError(err)
	s.Equal("2024-01-01", pb.DateToDateString(rsp.GetExecution().GetStartDate()))
	s.Equal("2024-12-31", pb.DateToDateString(rsp.GetExecution().GetEndDate()))
}

// A backtest without periods has no data, so the broker refuses the run
// instead of skipping the date check.
func (s *SessionTest) TestCreateExecutionRejectsBacktestWithoutPeriods() {
	_, err := s.conn.Exec(context.Background(), `DELETE FROM backtest_period WHERE backtest_name = $1`, s.backtest.GetName())
	s.Require().NoError(err)

	_, err = s.client.CreateExecution(context.Background(), &servicePb.CreateExecutionRequest{
		Session:   s.session.Id,
		Algorithm: &service_pb.Algorithm{},
	})
	s.Require().Error(err)
	st, ok := status.FromError(err)
	s.Require().True(ok)
	s.Equal(codes.FailedPrecondition, st.Code(), st.Message())
	s.Equal("Backtest 'backtest' has no data to run. Delete it with `fbull backtest delete backtest`, "+
		"then create it again with `fbull backtest create`.", st.Message())

	var stored int
	err = s.conn.QueryRow(context.Background(), `SELECT count(*) FROM backtest_execution`).Scan(&stored)
	s.Require().NoError(err)
	s.Zero(stored, "a rejected request stores no execution")
}

func TestCreateExecutionAuthOnUsesBacktestID(t *testing.T) {
	t.Setenv(environment.AuthEnabled, "true")
	test_helper.SetupEnvironment(t, &test_helper.Containers{Postgres: true})
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, environment.GetPostgresURL())
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, repository.Recreate(ctx, pool))
	require.NoError(t, auth.Migrate(ctx, pool))
	admin := insertAccount(t, pool, auth.RoleAdmin, "ada")
	require.NoError(t, repository.MigrateBacktestOwner(ctx, pool, admin.ID, quietCopier{}))

	backtests := repository.Backtest{Conn: pool}
	_, err = backtests.CreateOwned(ctx, admin.ID, &backtest.Backtest{
		Name: "spy",
		Periods: []*backtest.Backtest_Period{{
			StartDate: &pb.Date{Year: 2024, Month: 1, Day: 1},
			EndDate:   &pb.Date{Year: 2024, Month: 12, Day: 31},
		}},
	})
	require.NoError(t, err)
	var backtestID string
	err = pool.QueryRow(ctx, `SELECT id FROM backtest WHERE owner_id = $1 AND name = 'spy'`, admin.ID).Scan(&backtestID)
	require.NoError(t, err)

	sessions := repository.Session{Conn: pool}
	session, err := sessions.CreateForBacktest(ctx, backtestID)
	require.NoError(t, err)
	require.Equal(t, "spy", session.GetBacktest())
	require.NotEqual(t, backtestID, session.GetBacktest())

	client, activity := dialSession(t, session, pool)
	rsp, err := client.CreateExecution(ctx, &servicePb.CreateExecutionRequest{
		Session:   session.GetId(),
		StartDate: &pb.Date{Year: 2024, Month: 1, Day: 2},
		EndDate:   &pb.Date{Year: 2024, Month: 1, Day: 3},
		Algorithm: &service_pb.Algorithm{},
	})
	require.NoError(t, err)
	require.NotEmpty(t, rsp.GetExecution().GetId())
	select {
	case <-activity:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for activity")
	}

	missing := &backtest.Session{Id: "missing-session", Backtest: backtestID}
	missingClient, _ := dialSession(t, missing, pool)
	_, err = missingClient.CreateExecution(ctx, &servicePb.CreateExecutionRequest{
		Session:   missing.GetId(),
		Algorithm: &service_pb.Algorithm{},
	})
	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.NotFound, st.Code())
	require.Equal(t, "backtest not found", st.Message())
	require.NotContains(t, st.Message(), "SELECT")
	require.NotContains(t, st.Message(), backtestID)
}

func dialSession(t *testing.T, session *backtest.Session, pool *pgxpool.Pool) (servicePb.SessionServicerClient, <-chan bool) {
	t.Helper()
	eng := new(engine.MockEngine)
	eng.On("NewSession", mock.Anything, mock.Anything).Return(new(engine.MockEngineSession), nil)
	srv, activity, err := api.NewGRPCSessionServer(session, pool, eng)
	require.NoError(t, err)
	listener := bufconn.Listen(1024 * 1024)
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() {
		srv.Stop()
		_ = listener.Close()
	})
	conn, err := grpc.NewClient("passthrough:///"+listener.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return listener.Dial()
		}),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return servicePb.NewSessionServicerClient(conn), activity
}
