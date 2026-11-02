package grpc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"buf.build/go/protovalidate"
	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors"
	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/logging"
	protovalidate_middleware "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/protovalidate"
	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/selector"
	"github.com/lhjnilsson/foreverbull/internal/auth"
	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/internal/usercopy"
	"github.com/rs/zerolog/log"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const (
	FieldLength = 2
)

func pgxErrorToStatus(err *pgconn.PgError) error {
	switch err.Code {
	case "23505":
		return status.Error(codes.AlreadyExists, "Resource already exists")
	case "23503":
		return status.Error(codes.FailedPrecondition, "Referenced resource not found")
	case "23502":
		return status.Error(codes.InvalidArgument, "Required field missing")
	case "22P02":
		return status.Error(codes.InvalidArgument, "Invalid input format")
	case "42P01":
		return status.Error(codes.NotFound, "Resource not found")
	case "42703":
		return status.Error(codes.Internal, "Invalid field reference")
	case "53300":
		return status.Error(codes.ResourceExhausted, "Database connection limit reached")
	case "57014":
		return status.Error(codes.DeadlineExceeded, "Query timeout")
	default:
		return status.Error(codes.Internal, clientInternalDatabase)
	}
}

func pgxErrorInterceptor(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	resp, err := handler(ctx, req)
	if err != nil {
		// Check if it's a PostgreSQL error
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			return nil, pgxErrorToStatus(pgErr)
		}
		return resp, err
	}
	return resp, nil
}

func pgxStreamErrorInterceptor(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	err := handler(srv, ss)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			return pgxErrorToStatus(pgErr)
		}
		return err
	}
	return nil
}

func InterceptorLogger(logger *zap.Logger) logging.Logger {
	parseMessage := func(msg string, fields ...any) []zap.Field {
		zFields := make([]zap.Field, 0, len(fields)/FieldLength)

		for i := 0; i < len(fields); i += FieldLength {
			key := fields[i]
			value := fields[i+1]

			switch value := value.(type) {
			case string:
				zFields = append(zFields, zap.String(key.(string), value)) //nolint: forcetypeassert
			case int:
				zFields = append(zFields, zap.Int(key.(string), value)) //nolint: forcetypeassert
			case bool:
				zFields = append(zFields, zap.Bool(key.(string), value)) //nolint: forcetypeassert
			default:
				zFields = append(zFields, zap.Any(key.(string), value)) //nolint: forcetypeassert
			}
		}

		return zFields
	}

	return logging.LoggerFunc(func(ctx context.Context, lvl logging.Level, msg string, fields ...any) {
		f := parseMessage(msg, fields...)
		logger := logger.WithOptions(zap.AddCallerSkip(1)).With(f...)

		switch lvl {
		case logging.LevelDebug:
			logger.Debug(msg)
		case logging.LevelInfo:
			logger.Info(msg)
		case logging.LevelWarn:
			logger.Warn(msg)
		case logging.LevelError:
			logger.Error(msg)
		default:
			panic(fmt.Sprintf("unknown level %v", lvl))
		}
	})
}

// BearerLookup loads the account for one CLI token. The token is not logged.
type BearerLookup func(ctx context.Context, token string) (auth.Account, error)

type serverConfig struct {
	lookup BearerLookup
}

// ServerOption configures the broker gRPC server.
type ServerOption func(*serverConfig)

// WithBearerLookup supplies the token check used when AUTH_ENABLED is true.
func WithBearerLookup(lookup BearerLookup) ServerOption {
	return func(cfg *serverConfig) {
		cfg.lookup = lookup
	}
}

func NewServer(opts ...ServerOption) (*grpc.Server, error) {
	var cfg serverConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	if environment.GetAuthEnabled() && cfg.lookup == nil {
		return nil, errors.New("AUTH_ENABLED=true requires a bearer lookup")
	}

	logger := zap.NewExample()

	allButHealthZ := func(_ context.Context, callMeta interceptors.CallMeta) bool {
		return healthgrpc.Health_ServiceDesc.ServiceName != callMeta.Service
	}

	validator, err := protovalidate.New()
	if err != nil {
		return nil, fmt.Errorf("failed to create validator: %w", err)
	}

	logOpts := []logging.Option{
		logging.WithLogOnEvents(logging.StartCall, logging.FinishCall),
	}

	unary := []grpc.UnaryServerInterceptor{
		selector.UnaryServerInterceptor(
			logging.UnaryServerInterceptor(InterceptorLogger(logger), logOpts...),
			selector.MatchFunc(allButHealthZ),
		),
	}
	stream := []grpc.StreamServerInterceptor{
		selector.StreamServerInterceptor(
			logging.StreamServerInterceptor(InterceptorLogger(logger), logOpts...),
			selector.MatchFunc(allButHealthZ),
		),
	}
	if environment.GetAuthEnabled() {
		unary = append(unary, bearerUnary(cfg.lookup))
		stream = append(stream, bearerStream(cfg.lookup))
	}
	unary = append(unary,
		protovalidate_middleware.UnaryServerInterceptor(validator),
		clientFacingUnary,
		pgxErrorInterceptor,
	)
	stream = append(stream,
		protovalidate_middleware.StreamServerInterceptor(validator),
		clientFacingStream,
		pgxStreamErrorInterceptor,
	)

	return grpc.NewServer(
		grpc.ChainUnaryInterceptor(unary...),
		grpc.ChainStreamInterceptor(stream...),
	), nil
}

func bearerUnary(lookup BearerLookup) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if healthMethod(info.FullMethod) {
			return handler(ctx, req)
		}
		account, err := accountFromBearer(ctx, lookup)
		if err != nil {
			return nil, err
		}
		if err := roleAllowsMethod(account, info.FullMethod); err != nil {
			return nil, err
		}
		return handler(auth.WithAccount(ctx, account), req)
	}
}

func bearerStream(lookup BearerLookup) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if healthMethod(info.FullMethod) {
			return handler(srv, ss)
		}
		account, err := accountFromBearer(ss.Context(), lookup)
		if err != nil {
			return err
		}
		if err := roleAllowsMethod(account, info.FullMethod); err != nil {
			return err
		}
		return handler(srv, &accountStream{ServerStream: ss, ctx: auth.WithAccount(ss.Context(), account)})
	}
}

type accountStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *accountStream) Context() context.Context {
	return s.ctx
}

func healthMethod(full string) bool {
	return strings.HasPrefix(full, "/"+healthgrpc.Health_ServiceDesc.ServiceName+"/")
}

func accountFromBearer(ctx context.Context, lookup BearerLookup) (auth.Account, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return auth.Account{}, status.Error(codes.Unauthenticated, "unauthorized")
	}
	values := md.Get("authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
		return auth.Account{}, status.Error(codes.Unauthenticated, "unauthorized")
	}
	account, err := lookup(ctx, strings.TrimSpace(strings.TrimPrefix(values[0], "Bearer ")))
	if err != nil {
		if errors.Is(err, auth.ErrUnauthorized) {
			return auth.Account{}, status.Error(codes.Unauthenticated, "unauthorized")
		}
		if errors.Is(err, auth.ErrUseNotRecorded) {
			log.Error().Err(err).Msg("token use not recorded")
			return auth.Account{}, status.Error(codes.Unavailable, usercopy.RequestNotRecorded)
		}
		return auth.Account{}, status.Error(codes.Internal, clientInternalDatabase)
	}
	return account, nil
}

func roleAllowsMethod(account auth.Account, fullMethod string) error {
	if account.Role == auth.RoleNone || account.Role == auth.RolePending {
		return status.Error(codes.PermissionDenied, "forbidden")
	}
	if account.Role != auth.RoleViewer && account.Role != auth.RoleEditor && account.Role != auth.RoleAdmin {
		return status.Error(codes.PermissionDenied, "forbidden")
	}
	// A read token may call only the viewer methods. An empty scope is a
	// browser session and follows the role. A write token follows the role.
	switch account.Scope {
	case "", auth.ScopeWrite:
		if account.Role == auth.RoleViewer && !viewerMayCall(fullMethod) {
			return status.Error(codes.PermissionDenied, "forbidden")
		}
	case auth.ScopeRead:
		if !viewerMayCall(fullMethod) {
			return status.Error(codes.PermissionDenied, "forbidden")
		}
	default:
		return status.Error(codes.PermissionDenied, "forbidden")
	}
	return nil
}

func viewerMayCall(fullMethod string) bool {
	switch methodName(fullMethod) {
	case "ListBacktests", "GetBacktest", "GetSession", "ListExecutions", "GetExecution",
		"GetStrategy", "ListStrategies", "GetExperiment", "ListExperiments", "ListDeploymentRuns",
		"GetAsset":
		return true
	default:
		return false
	}
}

func methodName(fullMethod string) string {
	if i := strings.LastIndex(fullMethod, "/"); i >= 0 {
		return fullMethod[i+1:]
	}
	return fullMethod
}

var Module = fx.Options( //nolint: gochecknoglobals
	fx.Provide(
		func(pool *pgxpool.Pool) (*grpc.Server, error) {
			return NewServer(WithBearerLookup(func(ctx context.Context, token string) (auth.Account, error) {
				return auth.AccountFromBearer(ctx, pool, token, time.Now())
			}))
		},
	),
	fx.Invoke(
		func(lc fx.Lifecycle, grpcServer *grpc.Server, pool *pgxpool.Pool) error {
			lc.Append(
				fx.Hook{
					OnStart: func(context.Context) error {
						listener, err := net.Listen("tcp", fmt.Sprintf(":%s", environment.GetGRPCPort()))
						if err != nil {
							return fmt.Errorf("failed to listen: %w", err)
						}
						healthgrpc.RegisterHealthServer(grpcServer, newHealthCheck(pool))
						go func() {
							if err := grpcServer.Serve(listener); err != nil {
								panic(err)
							}
						}()
						return nil
					},
					OnStop: func(context.Context) error {
						grpcServer.GracefulStop()
						return nil
					},
				},
			)
			return nil
		},
	),
)
