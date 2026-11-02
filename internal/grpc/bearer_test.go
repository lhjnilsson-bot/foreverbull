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
