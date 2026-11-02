package api

import (
	"context"
	"errors"
	"fmt"

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
			log.Error().Err(err).Msg("backtest run failed")
			// Fixed sentence: err.Error() can quote a DB URL or stack fragment.
			return status.Error(codes.FailedPrecondition, "The backtest run stopped early. Try again, or run `fbenv status`.")
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
		return nil, fmt.Errorf("error getting result: %w", err)
	}
	executions := repository.Execution{Conn: s.db}
	err = executions.StorePeriods(ctx, req.ExecutionId, rsp.Periods)
	if err != nil {
		return nil, fmt.Errorf("error storing result: %w", err)
	}
	return &servicePb.StoreExecutionResultResponse{}, nil
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
