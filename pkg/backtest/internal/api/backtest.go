package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/internal/stream"
	internalPb "github.com/lhjnilsson/foreverbull/internal/stream/pb"
	"github.com/lhjnilsson/foreverbull/internal/usercopy"
	"github.com/lhjnilsson/foreverbull/pkg/backtest/internal/repository"
	backtestStream "github.com/lhjnilsson/foreverbull/pkg/backtest/stream"
	"github.com/lhjnilsson/foreverbull/pkg/pb"
	"github.com/lhjnilsson/foreverbull/pkg/pb/backtest"
	servicePb "github.com/lhjnilsson/foreverbull/pkg/pb/backtest/service"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type BacktestServer struct {
	servicePb.UnimplementedBacktestServicerServer

	pgx    *pgxpool.Pool
	stream stream.Stream
}

func NewBacktestServer(pgx *pgxpool.Pool, stream stream.Stream) *BacktestServer {
	return &BacktestServer{
		pgx:    pgx,
		stream: stream,
	}
}

func (bs *BacktestServer) ListBacktests(ctx context.Context,
	_ *servicePb.ListBacktestsRequest,
) (*servicePb.ListBacktestsResponse, error) {
	if environment.GetAuthEnabled() {
		return bs.listBacktestsAuthed(ctx)
	}
	backtests := repository.Backtest{Conn: bs.pgx}

	list, err := backtests.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("error listing backtests: %w", err)
	}

	return &servicePb.ListBacktestsResponse{
		Backtests: list,
	}, nil
}

func (bs *BacktestServer) CreateBacktest(req *servicePb.CreateBacktestRequest,
	serverStream servicePb.BacktestServicer_CreateBacktestServer,
) error {
	ctx := serverStream.Context()
	if environment.GetAuthEnabled() {
		return bs.createBacktestAuthed(req, serverStream)
	}
	rsp := &servicePb.CreateBacktestResponse{
		CREATING: &pb.Step{
			State: pb.State_STATE_UNSPECIFIED,
		},
		DOWNLOADING: &pb.Step{
			State: pb.State_STATE_UNSPECIFIED,
		},
		INGESTING: &pb.Step{
			State: pb.State_STATE_UNSPECIFIED,
		},
		STORING: &pb.Step{
			State: pb.State_STATE_UNSPECIFIED,
		},
		Backtest: nil,
	}
	if err := serverStream.Send(rsp); err != nil {
		return fmt.Errorf("error sending response: %w", err)
	}

	var o *internalPb.Orchestration
	var err error
	switch {
	case req.GetEtf() != nil:
		fromETF := req.GetEtf()
		o, err = backtestStream.CreateBacktestFromETF(fromETF.GetName(), fromETF.GetBaseEtf(),
			pb.DateToDateString(fromETF.GetStartDate()), pb.DateToDateString(fromETF.GetEndDate()),
			fromETF.GetProvider(), fromETF.GetProfile())
		if err != nil {
			return fmt.Errorf("error creating backtest orchestration: %w", err)
		}
	case req.GetBacktest() != nil:
		source := req.GetBacktest()
		o, err = backtestStream.CreateBacktestFromBacktest(source.Backtest)
		if err != nil {
			return fmt.Errorf("error creating backtest orchestration: %w", err)
		}
	default:
		return fmt.Errorf("invalid create type")
	}

	steps := bs.stream.RunOrchestration(ctx, o)
	for step := range steps {
		err = stream.UpdateOrchestration(rsp, step)
		if err != nil {
			return fmt.Errorf("error updating orchestration: %w", err)
		}
		err = serverStream.Send(rsp)
		if err != nil {
			return fmt.Errorf("error sending response: %w", err)
		}
	}
	backtests := repository.Backtest{Conn: bs.pgx}
	return sendStoredBacktest(rsp, serverStream, createdName(req), func() (*backtest.Backtest, error) {
		return backtests.Get(ctx, createdName(req))
	})
}

// createdName is the backtest name a create request stores.
func createdName(req *servicePb.CreateBacktestRequest) string {
	if etf := req.GetEtf(); etf != nil {
		return etf.GetName()
	}
	return req.GetBacktest().GetBacktest().GetName()
}

// sendStoredBacktest sends the stored backtest once every step completed, so the
// client can print what was created. The orchestration has no STORING step, so
// it is marked here.
func sendStoredBacktest(rsp *servicePb.CreateBacktestResponse,
	serverStream servicePb.BacktestServicer_CreateBacktestServer, name string,
	load func() (*backtest.Backtest, error),
) error {
	if rsp.GetINGESTING().GetState() != pb.State_STATE_COMPLETED {
		return nil
	}
	stored, err := load()
	if err != nil {
		// The database error stays in the log; the client gets fixed text.
		log.Error().Err(err).Str("backtest", name).Msg("error loading created backtest")
		return status.Errorf(codes.Internal,
			"Backtest %s was created, but its details couldn't be loaded. Run `fbull backtest get %s` to see it.",
			name, name)
	}
	rsp.Backtest = stored
	rsp.STORING.State = pb.State_STATE_COMPLETED
	if err := serverStream.Send(rsp); err != nil {
		return fmt.Errorf("error sending response: %w", err)
	}
	return nil
}

func (bs *BacktestServer) GetBacktest(ctx context.Context,
	req *servicePb.GetBacktestRequest,
) (*servicePb.GetBacktestResponse, error) {
	if environment.GetAuthEnabled() {
		return bs.getBacktestAuthed(ctx, req)
	}
	backtests := repository.Backtest{Conn: bs.pgx}

	backtest, err := backtests.Get(ctx, req.GetName())
	if errors.Is(err, pgx.ErrNoRows) {
		// NotFound lets the client say the name is wrong instead of leaking the query error.
		return nil, status.Errorf(codes.NotFound, "backtest %s not found", req.GetName())
	}
	if err != nil {
		return nil, fmt.Errorf("error getting backtest: %w", err)
	}

	return &servicePb.GetBacktestResponse{
		Name:     backtest.Name,
		Backtest: backtest,
	}, nil
}

func (bs *BacktestServer) DeleteBacktest(ctx context.Context, req *servicePb.DeleteBacktestRequest) (*servicePb.DeleteBacktestResponse, error) {
	if environment.GetAuthEnabled() {
		return bs.deleteBacktestAuthed(ctx, req)
	}
	backtests := repository.Backtest{Conn: bs.pgx}
	err := backtests.Delete(ctx, req.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		// NotFound lets a client tell "already gone" apart from a failed delete.
		return nil, status.Errorf(codes.NotFound, "backtest %s not found", req.Name)
	}
	if err != nil {
		return nil, fmt.Errorf("error deleting backtest: %w", err)
	}
	return &servicePb.DeleteBacktestResponse{}, nil
}

func (bs *BacktestServer) CreateSession(
	req *servicePb.CreateSessionRequest, serverStream servicePb.BacktestServicer_CreateSessionServer,
) error {
	ctx := serverStream.Context()
	if environment.GetAuthEnabled() {
		return bs.createSessionAuthed(req, serverStream)
	}
	sessions := repository.Session{Conn: bs.pgx}
	rsp := &servicePb.CreateSessionResponse{
		CREATING: &pb.Step{
			State: pb.State_STATE_RUNNING,
		},
		STARTING: &pb.Step{
			State: pb.State_STATE_UNSPECIFIED,
		},
		STORING: &pb.Step{
			State: pb.State_STATE_UNSPECIFIED,
		},
		Session: nil,
	}
	if err := serverStream.Send(rsp); err != nil {
		return fmt.Errorf("error sending response: %w", err)
	}

	session, err := sessions.Create(ctx, req.GetBacktestName())
	if err != nil {
		return fmt.Errorf("error creating session: %w", err)
	}
	rsp.Session = session
	rsp.CREATING.State = pb.State_STATE_COMPLETED
	if err := serverStream.Send(rsp); err != nil {
		return fmt.Errorf("error sending response: %w", err)
	}

	msg, err := backtestStream.NewSessionRunCommand(session.Backtest, session.Id)
	if err != nil {
		return fmt.Errorf("error creating session run command: %w", err)
	}
	rsp.STARTING.State = pb.State_STATE_RUNNING
	if err := serverStream.Send(rsp); err != nil {
		return fmt.Errorf("error sending response: %w", err)
	}

	err = bs.stream.Publish(ctx, msg, stream.WaitForCompletion())
	if err != nil {
		return sessionRunError(session.Backtest, err)
	}

	session, err = sessions.Get(ctx, session.Id)
	if err != nil {
		return fmt.Errorf("error getting session: %w", err)
	}

	rsp.STARTING.State = pb.State_STATE_COMPLETED
	rsp.Session = session
	if err := serverStream.Send(rsp); err != nil {
		return fmt.Errorf("error sending response: %w", err)
	}
	return nil
}

func (bs *BacktestServer) GetSession(ctx context.Context,
	req *servicePb.GetSessionRequest,
) (*servicePb.GetSessionResponse, error) {
	if environment.GetAuthEnabled() {
		return bs.getSessionAuthed(ctx, req)
	}
	sessions := repository.Session{Conn: bs.pgx}

	session, err := sessions.Get(ctx, req.GetSessionId())
	if err != nil {
		return nil, fmt.Errorf("error getting session: %w", err)
	}

	return &servicePb.GetSessionResponse{
		Session: session,
	}, nil
}

func (bs *BacktestServer) ListExecutions(ctx context.Context,
	req *servicePb.ListExecutionsRequest,
) (*servicePb.ListExecutionsResponse, error) {
	if environment.GetAuthEnabled() {
		return bs.listExecutionsAuthed(ctx, req)
	}
	storage := repository.Execution{Conn: bs.pgx}

	if req.GetSessionId() != "" {
		executions, err := storage.ListBySession(ctx, req.GetSessionId())
		if err != nil {
			return nil, fmt.Errorf("error listing executions: %w", err)
		}

		return &servicePb.ListExecutionsResponse{
			Executions: executions,
		}, nil
	} else if req.GetBacktest() != "" {
		executions, err := storage.ListByBacktest(ctx, req.GetBacktest())
		if err != nil {
			return nil, fmt.Errorf("error listing executions: %w", err)
		}

		return &servicePb.ListExecutionsResponse{
			Executions: executions,
		}, nil
	}

	executions, err := storage.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("error listing executions: %w", err)
	}

	return &servicePb.ListExecutionsResponse{
		Executions: executions,
	}, nil
}

func (bs *BacktestServer) GetExecution(ctx context.Context,
	req *servicePb.GetExecutionRequest,
) (*servicePb.GetExecutionResponse, error) {
	if environment.GetAuthEnabled() {
		return bs.getExecutionAuthed(ctx, req)
	}
	storage := repository.Execution{Conn: bs.pgx}

	execution, err := storage.Get(ctx, req.GetExecutionId())
	if err != nil {
		return nil, fmt.Errorf("error getting execution: %w", err)
	}
	periods, err := storage.GetPeriods(ctx, req.GetExecutionId())
	if err != nil {
		return nil, fmt.Errorf("error getting execution periods: %w", err)
	}
	functions, err := storage.GetFunctions(ctx, req.GetExecutionId())
	if err != nil {
		return nil, fmt.Errorf("error getting execution functions: %w", err)
	}
	provider, err := storage.GetProvider(ctx, req.GetExecutionId())
	if err != nil {
		return nil, fmt.Errorf("error getting execution provider: %w", err)
	}
	notices, err := storage.GetNotices(ctx, req.GetExecutionId())
	if err != nil {
		return nil, fmt.Errorf("error getting execution notices: %w", err)
	}
	execution.Notices = notices

	return &servicePb.GetExecutionResponse{
		Execution: execution,
		Periods:   periods,
		Functions: functions,
		Provider:  provider,
	}, nil
}

// sessionRunError is the error CreateSession returns when the session run
// command failed. Services that couldn't reach each other are named for
// the user; anything else stays an internal error.
func sessionRunError(backtest string, err error) error {
	if errors.Is(err, stream.ErrServicesUnreachable) {
		return status.Error(codes.Unavailable, usercopy.BacktestRunUnreachable(backtest, "fbull backtest run "+backtest))
	}
	return fmt.Errorf("error publishing session run command: %w", err)
}
