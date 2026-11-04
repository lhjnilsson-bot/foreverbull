package grpc

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lhjnilsson/foreverbull/internal/auth"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// tokenDB answers the token lookup with one valid write token and fails
// the last-use write.
type tokenDB struct{}

type tokenRow []any

func (r tokenRow) Scan(dest ...any) error {
	for i, d := range dest {
		target := reflect.ValueOf(d).Elem()
		if r[i] == nil {
			target.Set(reflect.Zero(target.Type()))
			continue
		}
		target.Set(reflect.ValueOf(r[i]))
	}
	return nil
}

func (tokenDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("not used")
}

func (tokenDB) QueryRow(context.Context, string, ...any) pgx.Row {
	// id, role, scope, token id, client, created_at, last_used_at, revoked_at, expires_at
	return tokenRow{"acc-1", auth.RoleEditor, auth.ScopeWrite, "tok-1", nil, nil, nil, nil, nil}
}

func (tokenDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("connection lost")
}

func tokenLookup(ctx context.Context, token string) (auth.Account, error) {
	return auth.AccountFromBearer(ctx, tokenDB{}, token, time.Now())
}

const useNotRecordedMessage = "Couldn't record this request, so foreverbull stopped it. Run `fbenv status` to check that " +
	"the environment is running, then run the same command again."

func bearerContext() context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer fb_secret"))
}

type contextStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s contextStream) Context() context.Context { return s.ctx }

// Through both interceptors, a token whose use couldn't be recorded stops
// the call with the sentence before the handler runs.
func TestBearerInterceptorsUseWriteFails(t *testing.T) {
	const method = "/foreverbull.backtest.service.v1.BacktestServicer/ListBacktests"

	t.Run("unary", func(t *testing.T) {
		_, err := bearerUnary(tokenLookup)(bearerContext(), nil, &grpc.UnaryServerInfo{FullMethod: method},
			func(context.Context, any) (any, error) {
				t.Fatal("the handler ran after the token use wasn't recorded")
				return nil, nil
			})
		st, ok := status.FromError(err)
		require.True(t, ok)
		require.Equal(t, codes.Unavailable, st.Code())
		require.Equal(t, useNotRecordedMessage, st.Message())
	})

	t.Run("stream", func(t *testing.T) {
		err := bearerStream(tokenLookup)(nil, contextStream{ctx: bearerContext()}, &grpc.StreamServerInfo{FullMethod: method},
			func(any, grpc.ServerStream) error {
				t.Fatal("the handler ran after the token use wasn't recorded")
				return nil
			})
		st, ok := status.FromError(err)
		require.True(t, ok)
		require.Equal(t, codes.Unavailable, st.Code())
		require.Equal(t, useNotRecordedMessage, st.Message())
	})
}
