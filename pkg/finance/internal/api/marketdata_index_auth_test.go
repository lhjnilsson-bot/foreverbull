package api

import (
	"context"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lhjnilsson/foreverbull/internal/auth"
	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/pkg/finance/supplier"
	pb "github.com/lhjnilsson/foreverbull/pkg/pb/finance"
	servicePb "github.com/lhjnilsson/foreverbull/pkg/pb/finance/service"
)

// GetIndex is refused before any provider lookup unless the caller is an
// editor or an admin while authentication is on.
func TestGetIndexRequiresTradingEditor(t *testing.T) {
	t.Setenv(environment.AuthEnabled, "true")
	req := &servicePb.GetIndexRequest{Symbol: "^GDAXI"}

	refused := []struct {
		name string
		ctx  context.Context
		code codes.Code
	}{
		{"no account", context.Background(), codes.Unauthenticated},
		{"viewer", auth.WithAccount(context.Background(), auth.Account{ID: "viewer", Role: auth.RoleViewer}), codes.PermissionDenied},
		{"pending", auth.WithAccount(context.Background(), auth.Account{ID: "pending", Role: auth.RolePending}), codes.PermissionDenied},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			md := supplier.NewMockMarketdata(t)
			rsp, err := NewMarketdataServer(nil, md).GetIndex(tc.ctx, req)
			require.Nil(t, rsp)
			require.Equal(t, tc.code, status.Code(err))
			md.AssertNotCalled(t, "GetIndex", mock.Anything, mock.Anything)
		})
	}

	for _, role := range []string{auth.RoleEditor, auth.RoleAdmin} {
		t.Run(role, func(t *testing.T) {
			md := supplier.NewMockMarketdata(t)
			md.On("GetIndex", mock.Anything, "^GDAXI").Return([]*pb.Asset{{Symbol: "ADS.DE"}}, nil).Once()
			ctx := auth.WithAccount(context.Background(), auth.Account{ID: "a", Role: role})
			rsp, err := NewMarketdataServer(nil, md).GetIndex(ctx, req)
			require.NoError(t, err)
			require.Len(t, rsp.GetAssets(), 1)
		})
	}

	t.Run("auth off", func(t *testing.T) {
		t.Setenv(environment.AuthEnabled, "false")
		md := supplier.NewMockMarketdata(t)
		md.On("GetIndex", mock.Anything, "^GDAXI").Return([]*pb.Asset{{Symbol: "ADS.DE"}}, nil).Once()
		rsp, err := NewMarketdataServer(nil, md).GetIndex(context.Background(), req)
		require.NoError(t, err)
		require.Len(t, rsp.GetAssets(), 1)
	})
}
