package api

import (
	"context"
	"fmt"
	"time"

	"github.com/lhjnilsson/foreverbull/internal/auth"
	"github.com/lhjnilsson/foreverbull/internal/usercopy"
	"github.com/lhjnilsson/foreverbull/pkg/finance/supplier"
	pb "github.com/lhjnilsson/foreverbull/pkg/pb/finance"
	servicePb "github.com/lhjnilsson/foreverbull/pkg/pb/finance/service"
	"github.com/lhjnilsson/foreverbull/pkg/service/worker"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type grpcSessionServer struct {
	servicePb.UnimplementedSessionServicerServer

	trading supplier.Trading

	wp worker.Pool

	orders []*pb.Order

	activity chan bool

	bars     worker.BarSource
	provider string

	// sessionID names the trading session in the user's sentences.
	sessionID string
}

type sessionOption func(*grpcSessionServer)

// WithBars fills WorkerRequest.bars from source during paper runs.
// provider is the price source. An empty provider is yahoo.
func WithBars(source worker.BarSource, provider string) sessionOption {
	return func(gs *grpcSessionServer) {
		gs.bars = source
		gs.provider = provider
	}
}

// WithSessionID names the trading session the server runs.
func WithSessionID(id string) sessionOption {
	return func(gs *grpcSessionServer) {
		gs.sessionID = id
	}
}

func NewGRPCSessionServer(trading supplier.Trading, opts ...sessionOption) (*grpc.Server, <-chan bool, error) {
	grpcServer := grpc.NewServer()
	activity := make(chan bool)
	server := &grpcSessionServer{
		trading:  trading,
		activity: activity,
	}
	for _, opt := range opts {
		opt(server)
	}
	servicePb.RegisterSessionServicerServer(grpcServer, server)

	return grpcServer, activity, nil
}

// NewSecuredGRPCSessionServer requires x-foreverbull-session on every call.
func NewSecuredGRPCSessionServer(trading supplier.Trading, secret string, opts ...sessionOption) (*grpc.Server, <-chan bool, error) {
	if secret == "" {
		return nil, nil, fmt.Errorf("%s is missing", auth.SessionMetadata())
	}
	grpcServer := grpc.NewServer(auth.SessionInterceptors(secret)...)
	activity := make(chan bool)
	server := &grpcSessionServer{
		trading:  trading,
		activity: activity,
	}
	for _, opt := range opts {
		opt(server)
	}
	servicePb.RegisterSessionServicerServer(grpcServer, server)
	return grpcServer, activity, nil
}

func (gs *grpcSessionServer) Configure(ctx context.Context, req *servicePb.ConfigureRequest) (*servicePb.ConfigureResponse, error) {
	var poolOpts []worker.Option
	if gs.bars != nil {
		poolOpts = append(poolOpts, worker.WithBars(gs.bars, gs.provider))
	}
	var err error
	gs.wp, err = worker.NewPool(ctx, req.GetAlgorithm(), poolOpts...)
	if err != nil {
		return nil, err
	}
	configuration := gs.wp.Configure()
	return &servicePb.ConfigureResponse{
		Configuration: configuration,
	}, nil
}

func (gs *grpcSessionServer) Run(ctx context.Context, req *servicePb.RunRequest) (*servicePb.RunResponse, error) {
	portfolio, err := gs.trading.GetPortfolio(ctx)
	if err != nil {
		return nil, err
	}

	orders, err := gs.wp.Process(ctx, time.Now(), req.Symbols, portfolio)
	if err != nil {
		return nil, err
	}
	gs.orders = orders
	return &servicePb.RunResponse{
		Orders: orders,
	}, nil
}

func (gs *grpcSessionServer) PlaceOrders(ctx context.Context, req *servicePb.PlaceOrderRequest) (*servicePb.PlaceOrderResponse, error) {
	rsp := &servicePb.PlaceOrderResponse{
		Orders: make([]*pb.Order, len(req.Orders)),
	}
	for i, order := range req.Orders {
		placed, err := gs.trading.PlaceOrder(ctx, order)
		if err != nil {
			return nil, err
		}
		rsp.Orders[i] = placed
	}
	return rsp, nil
}

func (gs *grpcSessionServer) StopServer(ctx context.Context, req *servicePb.StopServerRequest) (*servicePb.StopServerResponse, error) {
	var closeErr error
	if gs.wp != nil {
		closeErr = gs.wp.Close()
	}
	// The session server stops either way; a pool that didn't close is
	// still the caller's failure.
	close(gs.activity)
	if closeErr != nil {
		// The socket error names local addresses, so it goes to the log.
		log.Error().Err(closeErr).Str("session", gs.sessionID).Msg("error closing worker pool")
		return nil, status.Error(codes.Internal, usercopy.TradingSessionStopFailed(gs.sessionID))
	}
	return &servicePb.StopServerResponse{}, nil
}
