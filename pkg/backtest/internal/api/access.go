package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/lhjnilsson/foreverbull/internal/auth"
	"github.com/lhjnilsson/foreverbull/internal/stream"
	internalPb "github.com/lhjnilsson/foreverbull/internal/stream/pb"
	"github.com/lhjnilsson/foreverbull/pkg/backtest/internal/repository"
	backtestStream "github.com/lhjnilsson/foreverbull/pkg/backtest/stream"
	"github.com/lhjnilsson/foreverbull/pkg/pb"
	"github.com/lhjnilsson/foreverbull/pkg/pb/backtest"
	servicePb "github.com/lhjnilsson/foreverbull/pkg/pb/backtest/service"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (bs *BacktestServer) listBacktestsAuthed(ctx context.Context) (*servicePb.ListBacktestsResponse, error) {
	account, err := accountFrom(ctx)
	if err != nil {
		return nil, err
	}
	backtests := repository.Backtest{Conn: bs.pgx}
	list, err := backtests.ListVisible(ctx, account)
	if err != nil {
		return nil, fmt.Errorf("error listing backtests: %w", err)
	}
	return &servicePb.ListBacktestsResponse{Backtests: list}, nil
}

func (bs *BacktestServer) getBacktestAuthed(ctx context.Context, req *servicePb.GetBacktestRequest) (*servicePb.GetBacktestResponse, error) {
	account, err := accountFrom(ctx)
	if err != nil {
		return nil, err
	}
	id, err := auth.Resolve(ctx, bs.pgx, account, auth.DomainBacktest, req.GetName(), req.GetOwner(), auth.ActionRead)
	if err != nil {
		return nil, auth.GRPCStatus(err)
	}
	backtests := repository.Backtest{Conn: bs.pgx}
	item, err := backtests.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "not found")
		}
		return nil, fmt.Errorf("error getting backtest: %w", err)
	}
	return &servicePb.GetBacktestResponse{Name: item.Name, Backtest: item}, nil
}

func (bs *BacktestServer) deleteBacktestAuthed(ctx context.Context, req *servicePb.DeleteBacktestRequest) (*servicePb.DeleteBacktestResponse, error) {
	account, err := accountFrom(ctx)
	if err != nil {
		return nil, err
	}
	id, err := auth.Resolve(ctx, bs.pgx, account, auth.DomainBacktest, req.GetName(), req.GetOwner(), auth.ActionDelete)
	if err != nil {
		return nil, auth.GRPCStatus(err)
	}
	backtests := repository.Backtest{Conn: bs.pgx}
	if err := backtests.DeleteByID(ctx, id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "not found")
		}
		return nil, fmt.Errorf("error deleting backtest: %w", err)
	}
	return &servicePb.DeleteBacktestResponse{}, nil
}

func (bs *BacktestServer) createBacktestAuthed(req *servicePb.CreateBacktestRequest, serverStream servicePb.BacktestServicer_CreateBacktestServer) error {
	ctx := serverStream.Context()
	account, err := accountFrom(ctx)
	if err != nil {
		return err
	}
	if err := auth.MayCreate(account); err != nil {
		return auth.GRPCStatus(err)
	}
	rsp := newCreateBacktestResponse()
	if err := serverStream.Send(rsp); err != nil {
		return fmt.Errorf("error sending response: %w", err)
	}
	orchestration, err := ownedCreateOrchestration(req, account.ID)
	if err != nil {
		return err
	}
	backtests := repository.Backtest{Conn: bs.pgx}
	queueFull, err := newQueueFullCreate(ctx, ownedBacktest(backtests, account.ID, createdName(req)))
	if err != nil {
		return err
	}
	steps := bs.stream.RunOrchestration(ctx, orchestration)
	for step := range steps {
		queueFull.rewrite(ctx, step)
		if err := stream.UpdateOrchestration(rsp, step); err != nil {
			return fmt.Errorf("error updating orchestration: %w", err)
		}
		if err := serverStream.Send(rsp); err != nil {
			return fmt.Errorf("error sending response: %w", err)
		}
	}
	return sendStoredBacktest(rsp, serverStream, createdName(req), func() (*backtest.Backtest, error) {
		id, err := backtests.IDByOwnerAndName(ctx, account.ID, createdName(req))
		if err != nil {
			return nil, err
		}
		return backtests.GetByID(ctx, id)
	})
}

func newCreateBacktestResponse() *servicePb.CreateBacktestResponse {
	return &servicePb.CreateBacktestResponse{
		CREATING:    &pb.Step{State: pb.State_STATE_UNSPECIFIED},
		DOWNLOADING: &pb.Step{State: pb.State_STATE_UNSPECIFIED},
		INGESTING:   &pb.Step{State: pb.State_STATE_UNSPECIFIED},
		STORING:     &pb.Step{State: pb.State_STATE_UNSPECIFIED},
	}
}

func ownedCreateOrchestration(req *servicePb.CreateBacktestRequest, ownerID string) (*internalPb.Orchestration, error) {
	switch {
	case req.GetEtf() != nil:
		fromETF := req.GetEtf()
		item, err := backtestStream.CreateBacktestFromETFOwned(fromETF.GetName(), fromETF.GetBaseEtf(),
			pb.DateToDateString(fromETF.GetStartDate()), pb.DateToDateString(fromETF.GetEndDate()),
			fromETF.GetProvider(), fromETF.GetProfile(), ownerID)
		if err != nil {
			return nil, fmt.Errorf("error creating backtest orchestration: %w", err)
		}
		return item, nil
	case req.GetBacktest() != nil:
		item, err := backtestStream.CreateBacktestFromBacktestOwned(req.GetBacktest().GetBacktest(), ownerID)
		if err != nil {
			return nil, fmt.Errorf("error creating backtest orchestration: %w", err)
		}
		return item, nil
	default:
		return nil, fmt.Errorf("invalid create type")
	}
}

func (bs *BacktestServer) createSessionAuthed(req *servicePb.CreateSessionRequest, serverStream servicePb.BacktestServicer_CreateSessionServer) error {
	ctx := serverStream.Context()
	account, err := accountFrom(ctx)
	if err != nil {
		return err
	}
	backtestID, err := auth.Resolve(ctx, bs.pgx, account, auth.DomainBacktest, req.GetBacktestName(), req.GetOwner(), auth.ActionTrigger)
	if err != nil {
		return auth.GRPCStatus(err)
	}
	rsp := &servicePb.CreateSessionResponse{
		CREATING: &pb.Step{State: pb.State_STATE_RUNNING},
		STARTING: &pb.Step{State: pb.State_STATE_UNSPECIFIED},
		STORING:  &pb.Step{State: pb.State_STATE_UNSPECIFIED},
	}
	if err := serverStream.Send(rsp); err != nil {
		return fmt.Errorf("error sending response: %w", err)
	}
	sessions := repository.Session{Conn: bs.pgx}
	session, err := sessions.CreateForBacktest(ctx, backtestID)
	if err != nil {
		return fmt.Errorf("error creating session: %w", err)
	}
	secret, err := auth.MintBacktestSecret(session.GetId())
	if err != nil {
		return err
	}
	rsp.Session = session
	rsp.CREATING.State = pb.State_STATE_COMPLETED
	if err := serverStream.Send(rsp); err != nil {
		auth.ForgetBacktestSecret(session.GetId())
		return fmt.Errorf("error sending response: %w", err)
	}
	msg, err := backtestStream.NewSessionRunCommand(session.GetBacktest(), session.GetId())
	if err != nil {
		auth.ForgetBacktestSecret(session.GetId())
		return fmt.Errorf("error creating session run command: %w", err)
	}
	rsp.STARTING.State = pb.State_STATE_RUNNING
	if err := serverStream.Send(rsp); err != nil {
		auth.ForgetBacktestSecret(session.GetId())
		return fmt.Errorf("error sending response: %w", err)
	}
	if err := bs.stream.Publish(ctx, msg, stream.WaitForCompletion()); err != nil {
		auth.ForgetBacktestSecret(session.GetId())
		return sessionRunError(session.GetBacktest(), err)
	}
	session, err = sessions.Get(ctx, session.GetId())
	if err != nil {
		return fmt.Errorf("error getting session: %w", err)
	}
	rsp.STARTING.State = pb.State_STATE_COMPLETED
	rsp.Session = session
	stampBacktestSecret(rsp, secret)
	if err := serverStream.Send(rsp); err != nil {
		return fmt.Errorf("error sending response: %w", err)
	}
	return nil
}

func stampBacktestSecret(rsp *servicePb.CreateSessionResponse, secret string) {
	if secret == "" || rsp == nil || rsp.Session == nil || rsp.Session.GetPort() == 0 {
		return
	}
	rsp.SessionSecret = secret
}

func (bs *BacktestServer) getSessionAuthed(ctx context.Context, req *servicePb.GetSessionRequest) (*servicePb.GetSessionResponse, error) {
	account, err := accountFrom(ctx)
	if err != nil {
		return nil, err
	}
	sessions := repository.Session{Conn: bs.pgx}
	backtestID, err := sessions.BacktestID(ctx, req.GetSessionId())
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "not found")
		}
		return nil, fmt.Errorf("error getting session: %w", err)
	}
	if err := auth.Can(ctx, bs.pgx, account, auth.DomainBacktest, backtestID, auth.ActionRead); err != nil {
		return nil, auth.GRPCStatus(err)
	}
	session, err := sessions.Get(ctx, req.GetSessionId())
	if err != nil {
		return nil, fmt.Errorf("error getting session: %w", err)
	}
	return &servicePb.GetSessionResponse{Session: session}, nil
}

func (bs *BacktestServer) listExecutionsAuthed(ctx context.Context, req *servicePb.ListExecutionsRequest) (*servicePb.ListExecutionsResponse, error) {
	account, err := accountFrom(ctx)
	if err != nil {
		return nil, err
	}
	storage := repository.Execution{Conn: bs.pgx}
	if req.GetSessionId() != "" && req.GetBacktest() != "" {
		id, err := auth.Resolve(ctx, bs.pgx, account, auth.DomainBacktest, req.GetBacktest(), req.GetOwner(), auth.ActionRead)
		if err != nil {
			return nil, auth.GRPCStatus(err)
		}
		sessions := repository.Session{Conn: bs.pgx}
		sessionBacktest, err := sessions.BacktestID(ctx, req.GetSessionId())
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, status.Error(codes.NotFound, "not found")
			}
			return nil, fmt.Errorf("error listing executions: %w", err)
		}
		if err := auth.Can(ctx, bs.pgx, account, auth.DomainBacktest, sessionBacktest, auth.ActionRead); err != nil {
			return nil, auth.GRPCStatus(err)
		}
		if sessionBacktest != id {
			return nil, status.Error(codes.InvalidArgument, "backtest and session do not match")
		}
		executions, err := storage.ListBySession(ctx, req.GetSessionId())
		if err != nil {
			return nil, fmt.Errorf("error listing executions: %w", err)
		}
		return &servicePb.ListExecutionsResponse{Executions: executions}, nil
	}
	if req.GetSessionId() != "" {
		sessions := repository.Session{Conn: bs.pgx}
		backtestID, err := sessions.BacktestID(ctx, req.GetSessionId())
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, status.Error(codes.NotFound, "not found")
			}
			return nil, fmt.Errorf("error listing executions: %w", err)
		}
		if err := auth.Can(ctx, bs.pgx, account, auth.DomainBacktest, backtestID, auth.ActionRead); err != nil {
			return nil, auth.GRPCStatus(err)
		}
		executions, err := storage.ListBySession(ctx, req.GetSessionId())
		if err != nil {
			return nil, fmt.Errorf("error listing executions: %w", err)
		}
		return &servicePb.ListExecutionsResponse{Executions: executions}, nil
	}
	if req.GetBacktest() != "" {
		id, err := auth.Resolve(ctx, bs.pgx, account, auth.DomainBacktest, req.GetBacktest(), req.GetOwner(), auth.ActionRead)
		if err != nil {
			return nil, auth.GRPCStatus(err)
		}
		executions, err := storage.ListByBacktestID(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("error listing executions: %w", err)
		}
		return &servicePb.ListExecutionsResponse{Executions: executions}, nil
	}
	executions, err := storage.ListVisible(ctx, account)
	if err != nil {
		return nil, fmt.Errorf("error listing executions: %w", err)
	}
	return &servicePb.ListExecutionsResponse{Executions: executions}, nil
}

func (bs *BacktestServer) getExecutionAuthed(ctx context.Context, req *servicePb.GetExecutionRequest) (*servicePb.GetExecutionResponse, error) {
	account, err := accountFrom(ctx)
	if err != nil {
		return nil, err
	}
	storage := repository.Execution{Conn: bs.pgx}
	backtestID, err := storage.BacktestID(ctx, req.GetExecutionId())
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "not found")
		}
		return nil, fmt.Errorf("error getting execution: %w", err)
	}
	if err := auth.Can(ctx, bs.pgx, account, auth.DomainBacktest, backtestID, auth.ActionRead); err != nil {
		return nil, auth.GRPCStatus(err)
	}
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

func accountFrom(ctx context.Context) (auth.Account, error) {
	account, ok := auth.FromContext(ctx)
	if !ok || account.ID == "" {
		return auth.Account{}, auth.GRPCStatus(auth.ErrUnauthorized)
	}
	return account, nil
}
