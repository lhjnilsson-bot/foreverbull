package api

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/lhjnilsson/foreverbull/internal/auth"
	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/internal/postgres"
	"github.com/lhjnilsson/foreverbull/internal/usercopy"
	"github.com/lhjnilsson/foreverbull/pkg/backtest/engine"
	"github.com/lhjnilsson/foreverbull/pkg/backtest/internal/repository"
	financebars "github.com/lhjnilsson/foreverbull/pkg/finance/bars"
	"github.com/lhjnilsson/foreverbull/pkg/pb/backtest"
	servicePb "github.com/lhjnilsson/foreverbull/pkg/pb/backtest/service"
	"github.com/lhjnilsson/foreverbull/pkg/service/worker"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type grpcSessionServer struct {
	servicePb.UnimplementedSessionServicerServer

	session *backtest.Session

	db              postgres.Query
	backtestSession engine.EngineSession
	wp              worker.Pool

	activity chan bool
}

const (
	ActivityBufferSize = 5
)

func NewGRPCSessionServer(session *backtest.Session, database postgres.Query,
	ziplineEngine engine.Engine,
) (*grpc.Server, <-chan bool, error) {
	grpcServer := grpc.NewServer()

	backtestSession, err := ziplineEngine.NewSession(context.Background(), session)
	if err != nil {
		log.Error().Err(err).Msg("fail to create zipline session")
		return nil, nil, fmt.Errorf("error creating session: %w", err)
	}

	activity := make(chan bool, ActivityBufferSize)
	server := &grpcSessionServer{
		session:         session,
		db:              database,
		backtestSession: backtestSession,
		activity:        activity,
	}
	servicePb.RegisterSessionServicerServer(grpcServer, server)

	return grpcServer, activity, nil
}

// NewSecuredGRPCSessionServer requires x-foreverbull-session on every call.
// The secret is the value minted for this session. It is not the user bearer.
func NewSecuredGRPCSessionServer(session *backtest.Session, database postgres.Query,
	ziplineEngine engine.Engine, secret string,
) (*grpc.Server, <-chan bool, error) {
	if secret == "" {
		return nil, nil, fmt.Errorf("%s is missing", auth.SessionMetadata())
	}
	grpcServer := grpc.NewServer(auth.SessionInterceptors(secret)...)

	backtestSession, err := ziplineEngine.NewSession(context.Background(), session)
	if err != nil {
		log.Error().Err(err).Msg("fail to create zipline session")
		return nil, nil, fmt.Errorf("error creating session: %w", err)
	}

	activity := make(chan bool, ActivityBufferSize)
	server := &grpcSessionServer{
		session:         session,
		db:              database,
		backtestSession: backtestSession,
		activity:        activity,
	}
	servicePb.RegisterSessionServicerServer(grpcServer, server)
	return grpcServer, activity, nil
}

// storedBacktest loads the row the session was opened on. Auth-on sessions
// store the backtest id. Auth-off sessions store the name.
func (s *grpcSessionServer) storedBacktest(ctx context.Context) (*backtest.Backtest, error) {
	backtests := repository.Backtest{Conn: s.db}
	if !environment.GetAuthEnabled() {
		return backtests.Get(ctx, s.session.GetBacktest())
	}
	sessions := repository.Session{Conn: s.db}
	id, err := sessions.BacktestID(ctx, s.session.GetId())
	if err != nil {
		return nil, err
	}
	return backtests.GetByID(ctx, id)
}

func backtestLoadStatus(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return status.Error(codes.NotFound, "backtest not found")
	}
	return status.Error(codes.Internal, "could not load the backtest")
}

func (s *grpcSessionServer) pingActivity() {
	select {
	case s.activity <- true:
	default:
	}
}

func (s *grpcSessionServer) CreateExecution(ctx context.Context, req *servicePb.CreateExecutionRequest) (*servicePb.CreateExecutionResponse, error) {
	log.Debug().Msg("create execution")
	s.pingActivity()

	if environment.GetAuthEnabled() && req.GetSession() != s.session.GetId() {
		return nil, status.Error(codes.NotFound, "not found")
	}
	stored, err := s.storedBacktest(ctx)
	if err != nil {
		log.Error().Err(err).Str("session", s.session.GetId()).Msg("error getting backtest")
		return nil, backtestLoadStatus(err)
	}
	startDate, endDate, err := resolveRunDates(stored, req.GetStartDate(), req.GetEndDate())
	if err != nil {
		log.Info().Err(err).Str("backtest", s.session.GetBacktest()).Msg("execution dates rejected")
		return nil, err
	}

	executions := repository.Execution{Conn: s.db}
	e := backtest.Execution{
		Session:   s.session.Id,
		StartDate: startDate,
		EndDate:   endDate,
	}

	execution, err := executions.Create(ctx, &e, executionFunctionsFromAlgorithm(req.GetAlgorithm()))
	if err != nil {
		log.Error().Err(err).Msg("error creating execution")
		return nil, fmt.Errorf("error creating execution: %w", err)
	}

	var poolOpts []worker.Option
	if s.db != nil {
		poolOpts = append(poolOpts, worker.WithBars(financebars.New(s.db), stored.GetProvider()))
	}
	s.wp, err = worker.NewPool(ctx, req.GetAlgorithm(), poolOpts...)
	if err != nil {
		log.Error().Err(err).Msg("error creating worker pool")
		return nil, fmt.Errorf("error creating worker pool: %w", err)
	}

	configuration := s.wp.Configure()
	log.Debug().Str("execution_id", execution.GetId()).Msg("execution created")

	return &servicePb.CreateExecutionResponse{
		Execution:     execution,
		Configuration: configuration,
	}, nil
}

func (s *grpcSessionServer) RunExecution(req *servicePb.RunExecutionRequest, stream servicePb.SessionServicer_RunExecutionServer) error {
	log.Debug().Any("request", req).Msg("run execution")
	s.pingActivity()
	ctx := stream.Context()

	execution, err := s.executionForListener(ctx, req.ExecutionId)
	if err != nil {
		log.Error().Err(err).Str("execution_id", req.ExecutionId).Msg("error getting execution")
		return err
	}

	periods, err := s.backtestSession.RunBacktest(ctx, execution, s.wp)
	if err != nil {
		log.Error().Err(err).Msg("error running backtest")
		if refused, ok := err.(interface{ GRPCStatus() *status.Status }); ok &&
			refused.GRPCStatus().Code() == codes.FailedPrecondition {
			// Zipline's own sentence for a refused run, unwrapped; fbull
			// prints it as is.
			return err
		}
		return fmt.Errorf("error running backtest: %w", err)
	}

	for period := range periods {
		err := stream.Send(period)
		if err != nil {
			return fmt.Errorf("error sending portfolio: %w", err)
		}
	}
	if failure, ok := s.backtestSession.(interface{ RunFailure() error }); ok {
		if err := failure.RunFailure(); err != nil {
			return runFailureError(err)
		}
	}
	if noticed, ok := s.backtestSession.(interface{ RunNotices() []string }); ok {
		// Stored before the stream ends, so the summary the client reads
		// next names the members the run left out.
		executions := repository.Execution{Conn: s.db}
		if err := executions.SetNotices(ctx, execution.GetId(), noticed.RunNotices()); err != nil {
			log.Error().Err(err).Str("execution_id", execution.GetId()).Msg("error storing run notices")
			return status.Error(codes.Internal, "The backtest finished, but its notices couldn't be saved. Run it again.")
		}
	}
	return nil
}

func (s *grpcSessionServer) StoreResult(ctx context.Context, req *servicePb.StoreExecutionResultRequest) (*servicePb.StoreExecutionResultResponse, error) {
	log.Debug().Any("request", req).Msg("store result")
	if _, err := s.executionForListener(ctx, req.ExecutionId); err != nil {
		return nil, err
	}
	rsp, err := s.backtestSession.GetResult(ctx, req.ExecutionId)
	if err != nil {
		return nil, getResultError(req.ExecutionId, err)
	}
	executions := repository.Execution{Conn: s.db}
	provider, err := executions.GetProvider(ctx, req.ExecutionId)
	if err != nil {
		return nil, fmt.Errorf("error getting execution provider: %w", err)
	}
	basis, err := returnBasis(provider)
	if err != nil {
		return nil, fmt.Errorf("error storing result: %w", err)
	}
	err = executions.StoreResult(ctx, req.ExecutionId, basis, rsp.GetForeignDividendsGross(), rsp.Periods)
	if err != nil {
		return nil, fmt.Errorf("error storing result: %w", err)
	}
	return &servicePb.StoreExecutionResultResponse{}, nil
}

// runStoppedEarly is the run's failure when the engine or a worker crashed or
// the connection to it was lost. Trying again can help.
const runStoppedEarly = "The backtest run stopped early. Try again, or run `fbenv status`."

// runFailureError is what fbull is told when the run stopped before its end.
// Zipline names a stop it knows the reason for, such as a dividend it didn't
// pay, with FailedPrecondition and the user's whole sentence; that fails the
// same way on every run, so it is passed on as written instead of "try again".
// Anything else gets the fixed sentence: its text can quote a DB URL or stack
// fragment.
func runFailureError(err error) error {
	log.Error().Err(err).Msg("backtest run failed")
	if engine, ok := engineStatus(err); ok && engine.Code() == codes.FailedPrecondition {
		return status.Error(codes.FailedPrecondition, engine.Message())
	}
	return status.Error(codes.FailedPrecondition, runStoppedEarly)
}

// engineStatus is the gRPC status the engine answered with, also when err
// wraps it.
func engineStatus(err error) (*status.Status, bool) {
	var engine interface{ GRPCStatus() *status.Status }
	if !errors.As(err, &engine) {
		return nil, false
	}
	return engine.GRPCStatus(), true
}

// engineResultErrorTag starts the message of the engine's own failure to read
// a result. It matches ENGINE_RESULT_ERROR_TAG in client/zipline
// session_service.py.
const engineResultErrorTag = "engine result error: "

// getResultError is StoreResult's error when the engine's result couldn't be
// read. Only the engine's own failure is the fixed report sentence; a lost
// connection, a cancel or a deadline is returned as it was.
func getResultError(executionID string, err error) error {
	if engine, ok := engineStatus(err); ok && engine.Code() == codes.Internal &&
		strings.HasPrefix(engine.Message(), engineResultErrorTag) {
		return resultUnreadableError(executionID, err)
	}
	return fmt.Errorf("error getting result: %w", err)
}

// resultUnreadable is the run's failure when the engine couldn't hand over
// its result, such as an internal check of the dividend book or a metric
// with no value. Those are foreverbull's bugs, not the algo's.
const resultUnreadable = "The backtest finished, but foreverbull couldn't read its result. " +
	"This is a problem in foreverbull, not in your algo. " +
	"Please report it at https://github.com/lhjnilsson/foreverbull/issues with this message."

// resultUnreadableError logs why the result couldn't be read and returns the
// fixed sentence: the engine's text can name internal checks and values.
func resultUnreadableError(executionID string, err error) error {
	log.Error().Err(err).Str("execution_id", executionID).Msg("error getting result")
	return status.Error(codes.FailedPrecondition, resultUnreadable)
}

// returnBasis is what a run's return measures, by its price provider.
// yahoo and alpaca bars leave dividends out and the run books them as
// cash: a total return. stooq bars have dividends inside the prices and
// the run books none: an adjusted-close return.
func returnBasis(provider string) (backtest.ReturnBasis, error) {
	switch provider {
	case "yahoo", "alpaca":
		return backtest.ReturnBasis_RETURN_BASIS_TOTAL, nil
	case "stooq":
		return backtest.ReturnBasis_RETURN_BASIS_ADJUSTED_CLOSE, nil
	default:
		return backtest.ReturnBasis_RETURN_BASIS_UNSPECIFIED, fmt.Errorf("execution has no known price provider (%q)", provider)
	}
}

func (s *grpcSessionServer) executionForListener(ctx context.Context, executionID string) (*backtest.Execution, error) {
	executions := repository.Execution{Conn: s.db}
	execution, err := executions.Get(ctx, executionID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			if !environment.GetAuthEnabled() {
				return nil, fmt.Errorf("error getting execution: %w", err)
			}
			return nil, status.Error(codes.NotFound, "not found")
		}
		// A failed read is not "not found". The client gets a sentence, so
		// the cause is logged here.
		log.Error().Err(err).Str("execution_id", executionID).Msg("error reading execution")
		return nil, status.Error(codes.Internal, usercopy.ExecutionReadFailed(executionID, "fbull backtest run "+s.session.GetBacktest()))
	}
	if environment.GetAuthEnabled() && execution.GetSession() != s.session.GetId() {
		return nil, status.Error(codes.NotFound, "not found")
	}
	return execution, nil
}

func (s *grpcSessionServer) StopServer(_ context.Context, _ *servicePb.StopServerRequest) (*servicePb.StopServerResponse, error) {
	log.Debug().Msg("stop server")
	var closeErr error
	if s.wp != nil {
		closeErr = s.wp.Close()
	}
	// The session server stops either way; a pool that didn't close is
	// still the caller's failure.
	close(s.activity)
	if closeErr != nil {
		// The socket error names local addresses, so it goes to the log.
		log.Error().Err(closeErr).Str("session", s.session.GetId()).Msg("error closing worker pool")
		return nil, status.Error(codes.Internal, usercopy.BacktestStopFailed(s.session.GetBacktest()))
	}
	return &servicePb.StopServerResponse{}, nil
}
