package grpc

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lhjnilsson/foreverbull/internal/auth"
	"github.com/lhjnilsson/foreverbull/internal/environment"
)

func TestPgxErrorToStatus(t *testing.T) {
	t.Parallel()

	cases := []struct {
		code     string
		wantCode codes.Code
		wantMsg  string
	}{
		{"23505", codes.AlreadyExists, "Resource already exists"},
		{"23503", codes.FailedPrecondition, "Referenced resource not found"},
		{"23502", codes.InvalidArgument, "Required field missing"},
		{"22P02", codes.InvalidArgument, "Invalid input format"},
		{"42P01", codes.NotFound, "Resource not found"},
		{"42703", codes.Internal, "Invalid field reference"},
		{"53300", codes.ResourceExhausted, "Database connection limit reached"},
		{"57014", codes.DeadlineExceeded, "Query timeout"},
		{"XXXXX", codes.Internal, clientInternalDatabase},
	}

	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			t.Parallel()

			err := pgxErrorToStatus(&pgconn.PgError{Code: tc.code})
			st, ok := status.FromError(err)
			require.True(t, ok)
			require.Equal(t, tc.wantCode, st.Code())
			require.Equal(t, tc.wantMsg, st.Message())
		})
	}

	t.Run("unary interceptor maps pg error", func(t *testing.T) {
		t.Parallel()

		handler := func(context.Context, interface{}) (interface{}, error) {
			return "ignored", &pgconn.PgError{Code: "23505"}
		}
		resp, err := pgxErrorInterceptor(context.Background(), nil, &grpc.UnaryServerInfo{}, handler)
		require.Nil(t, resp)
		st, ok := status.FromError(err)
		require.True(t, ok)
		require.Equal(t, codes.AlreadyExists, st.Code())
	})

	t.Run("unary interceptor passes through other errors", func(t *testing.T) {
		t.Parallel()

		want := errors.New("not postgres")
		handler := func(context.Context, interface{}) (interface{}, error) {
			return "ok", want
		}
		resp, err := pgxErrorInterceptor(context.Background(), nil, &grpc.UnaryServerInfo{}, handler)
		require.Equal(t, "ok", resp)
		require.ErrorIs(t, err, want)
	})
}

func TestReadScopeAllowsOnlyReadRPCs(t *testing.T) {
	t.Setenv(environment.AuthEnabled, "true")

	const (
		create = "/foreverbull.backtest.service.v1.BacktestServicer/CreateBacktest"
		get    = "/foreverbull.backtest.service.v1.BacktestServicer/GetBacktest"
	)
	read := auth.Account{ID: "a", Role: auth.RoleEditor, Scope: auth.ScopeRead}
	write := auth.Account{ID: "a", Role: auth.RoleEditor, Scope: auth.ScopeWrite}
	session := auth.Account{ID: "a", Role: auth.RoleEditor}
	viewer := auth.Account{ID: "a", Role: auth.RoleViewer, Scope: auth.ScopeWrite}

	err := roleAllowsMethod(read, create)
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	require.NoError(t, roleAllowsMethod(read, get))
	require.NoError(t, roleAllowsMethod(write, create))
	require.NoError(t, roleAllowsMethod(session, create))
	require.Equal(t, codes.PermissionDenied, status.Code(roleAllowsMethod(viewer, create)))
	require.NoError(t, roleAllowsMethod(viewer, get))
}

// GetIndex fans out one provider lookup per index member, so a viewer or a
// read token may not call it. GetAsset stays readable.
func TestGetIndexIsNotAReadRPC(t *testing.T) {
	t.Setenv(environment.AuthEnabled, "true")

	const (
		getIndex = "/foreverbull.finance.service.v1.Marketdata/GetIndex"
		getAsset = "/foreverbull.finance.service.v1.Marketdata/GetAsset"
	)
	viewer := auth.Account{ID: "a", Role: auth.RoleViewer}
	read := auth.Account{ID: "a", Role: auth.RoleEditor, Scope: auth.ScopeRead}
	editor := auth.Account{ID: "a", Role: auth.RoleEditor, Scope: auth.ScopeWrite}

	require.Equal(t, codes.PermissionDenied, status.Code(roleAllowsMethod(viewer, getIndex)))
	require.Equal(t, codes.PermissionDenied, status.Code(roleAllowsMethod(read, getIndex)))
	require.NoError(t, roleAllowsMethod(editor, getIndex))
	require.NoError(t, roleAllowsMethod(viewer, getAsset))
	require.NoError(t, roleAllowsMethod(read, getAsset))
}
