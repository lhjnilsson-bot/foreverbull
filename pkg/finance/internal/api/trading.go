package api

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lhjnilsson/foreverbull/internal/auth"
	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/internal/stream"
	streamPb "github.com/lhjnilsson/foreverbull/internal/stream/pb"
	"github.com/lhjnilsson/foreverbull/internal/usercopy"
	"github.com/lhjnilsson/foreverbull/pkg/finance/internal/repository"
	financeStream "github.com/lhjnilsson/foreverbull/pkg/finance/stream"
	"github.com/lhjnilsson/foreverbull/pkg/finance/supplier"
	commonPb "github.com/lhjnilsson/foreverbull/pkg/pb"
	servicePb "github.com/lhjnilsson/foreverbull/pkg/pb/finance/service"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type TradingServer struct {
	servicePb.UnimplementedTradingServicerServer

	pgx    *pgxpool.Pool
	stream stream.Stream

	trading supplier.Trading
}

func NewTradingServer(pgx *pgxpool.Pool, stream stream.Stream, trading supplier.Trading) *TradingServer {
	return &TradingServer{
		pgx:     pgx,
		stream:  stream,
		trading: trading,
	}
}

func (ts *TradingServer) GetOrders(ctx context.Context, req *servicePb.GetOrdersRequest) (*servicePb.GetOrdersResponse, error) {
	if err := requireTradingAccount(ctx); err != nil {
		return nil, err
	}
	if ts.trading == nil {
		return nil, status.Error(codes.Unimplemented, "trading is not enabled")
	}

	orders, err := ts.trading.GetOrders(ctx)
	if err != nil {
		return nil, fmt.Errorf("error getting orders: %w", err)
	}
	rsp := &servicePb.GetOrdersResponse{
		Orders: orders,
	}
	return rsp, nil
}

func (ts *TradingServer) GetPortfolio(ctx context.Context, req *servicePb.GetPortfolioRequest) (*servicePb.GetPortfolioResponse, error) {
	if err := requireTradingAccount(ctx); err != nil {
		return nil, err
	}
	if ts.trading == nil {
		return nil, status.Error(codes.Unimplemented, "trading is not enabled")
	}

	portfolio, err := ts.trading.GetPortfolio(ctx)
	if err != nil {
		return nil, fmt.Errorf("error getting portfolio: %w", err)
	}
	rsp := &servicePb.GetPortfolioResponse{
		Portfolio: portfolio,
	}
	return rsp, nil
}

func (ts *TradingServer) CreateSession(req *servicePb.CreateSessionRequest, serverStream servicePb.TradingServicer_CreateSessionServer) error {
	if ts.trading == nil {
		return status.Error(codes.Unimplemented, "trading is not enabled")
	}
	ctx := serverStream.Context()
	if err := requireTradingAccount(ctx); err != nil {
		return err
	}
	rsp := &servicePb.CreateSessionResponse{
		CREATING: &commonPb.Step{
			State: commonPb.State_STATE_RUNNING,
		},
		UPDATING_MARKET_DATA: &commonPb.Step{
			State: commonPb.State_STATE_UNSPECIFIED,
		},
		STARTING: &commonPb.Step{
			State: commonPb.State_STATE_UNSPECIFIED,
		},
		FINISHING: &commonPb.Step{
			State: commonPb.State_STATE_UNSPECIFIED,
		},
		Session: nil,
	}
	if err := serverStream.Send(rsp); err != nil {
		return fmt.Errorf("error sending response: %w", err)
	}

	sessions := repository.Trading{Conn: ts.pgx}
	s, err := sessions.Create(ctx)
	if err != nil {
		return fmt.Errorf("error creating session: %w", err)
	}
	secret := ""
	if environment.GetAuthEnabled() {
		secret, err = auth.MintTradingSecret(s.GetId())
		if err != nil {
			return err
		}
	}
	published := false
	defer func() {
		if secret != "" && !published {
			auth.ForgetTradingSecret(s.GetId())
		}
	}()
	rsp.Session = s
	rsp.CREATING.State = commonPb.State_STATE_COMPLETED
	if err := serverStream.Send(rsp); err != nil {
		return fmt.Errorf("error sending response: %w", err)
	}
	o, err := financeStream.NewRunTradeOrchestration(s.Id, req.Symbols)
	if err != nil {
		return err
	}
	ch := ts.stream.RunOrchestration(ctx, o)
	for msg := range ch {
		if err := stream.UpdateOrchestration(rsp, tradingStep(msg, s.GetId())); err != nil {
			return fmt.Errorf("error updating orchestration: %w", err)
		}
		rsp.Session, err = sessions.Get(ctx, s.GetId())
		if err != nil {
			return err
		}
		stampTradingSecret(rsp, secret)
		if err := serverStream.Send(rsp); err != nil {
			return fmt.Errorf("error sending response: %w", err)
		}
	}
	published = true
	return nil
}

// tradingStep names the session in a step that failed because the services
// couldn't reach each other; the step's message is what the client prints.
func tradingStep(step *streamPb.Orchestration_Step, sessionID string) *streamPb.Orchestration_Step {
	if step.GetError() != usercopy.ServicesUnreachable {
		return step
	}
	named := proto.Clone(step).(*streamPb.Orchestration_Step) //nolint:forcetypeassert // Clone keeps the type
	text := usercopy.TradingSessionUnreachable(sessionID)
	named.Error = &text
	return named
}

func stampTradingSecret(rsp *servicePb.CreateSessionResponse, secret string) {
	if secret == "" || rsp == nil || rsp.GetSession() == nil || rsp.GetSession().GetPort() == 0 {
		return
	}
	rsp.SessionSecret = secret
}

func requireTradingEditor(ctx context.Context) error {
	if !environment.GetAuthEnabled() {
		return nil
	}
	account, ok := auth.FromContext(ctx)
	if !ok || account.ID == "" {
		return auth.GRPCStatus(auth.ErrUnauthorized)
	}
	if account.Role != auth.RoleEditor && account.Role != auth.RoleAdmin {
		return auth.GRPCStatus(auth.ErrForbidden)
	}
	return nil
}

// requireTradingAccount guards the broker trading account. CreateSession,
// GetOrders, and GetPortfolio name no strategy, so there is no domain row to
// grant. An editor uses strategy deploy, which checks a trigger grant. These
// RPCs stay limited to an admin while authentication is on.
func requireTradingAccount(ctx context.Context) error {
	if !environment.GetAuthEnabled() {
		return nil
	}
	account, ok := auth.FromContext(ctx)
	if !ok || account.ID == "" {
		return auth.GRPCStatus(auth.ErrUnauthorized)
	}
	if account.Role != auth.RoleAdmin {
		return auth.GRPCStatus(auth.ErrForbidden)
	}
	return nil
}
