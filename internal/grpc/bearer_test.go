package grpc

import (
	"context"
	"fmt"
	"testing"

	"github.com/lhjnilsson/foreverbull/internal/auth"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// A token whose use couldn't be recorded stops the request with the
// sentence, not the database error.
func TestBearerUseNotRecorded(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer fb_x"))
	lookup := func(context.Context, string) (auth.Account, error) {
		return auth.Account{}, fmt.Errorf("record token use: %w", auth.ErrUseNotRecorded)
	}
	_, err := accountFromBearer(ctx, lookup)
	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.Unavailable, st.Code())
	require.Equal(t, "Couldn't record this request, so foreverbull stopped it. Run `fbenv status` to check that "+
		"the environment is running, then run the same command again.", st.Message())
}

// An expired sign-in is UNAUTHENTICATED with the sentence that names
// `fbull login`. Any other refusal keeps the bare "unauthorized".
func TestBearerExpiredSignInNamesFbullLogin(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer fb_x"))
	cases := []struct {
		name    string
		err     error
		message string
	}{
		{"expired", auth.ErrSignInExpired, "Your sign-in has expired. Run `fbull login` to sign in again."},
		{"unknown", auth.ErrUnauthorized, "unauthorized"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lookup := func(context.Context, string) (auth.Account, error) { return auth.Account{}, tc.err }
			_, err := accountFromBearer(ctx, lookup)
			st, ok := status.FromError(err)
			require.True(t, ok)
			require.Equal(t, codes.Unauthenticated, st.Code())
			require.Equal(t, tc.message, st.Message())
		})
	}
}
