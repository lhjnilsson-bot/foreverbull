package api

import (
	"context"
	"errors"
	"testing"

	servicePb "github.com/lhjnilsson/foreverbull/pkg/pb/finance/service"
	"github.com/lhjnilsson/foreverbull/pkg/service/worker"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A worker pool that can't be closed fails StopServer. The session server
// still stops.
func TestStopServerReportsPoolCloseFailure(t *testing.T) {
	pool := new(worker.MockPool)
	pool.On("Close").Return(errors.New("socket busy"))
	activity := make(chan bool)
	server := &grpcSessionServer{wp: pool, activity: activity, sessionID: "t-1"}

	_, err := server.StopServer(context.Background(), &servicePb.StopServerRequest{})
	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.Internal, st.Code())
	require.Equal(t, "Trading session 't-1' ended, but foreverbull couldn't stop the processes running your strategy. "+
		"Run `fbenv status` to check the environment before the next session.", st.Message())
	_, open := <-activity
	require.False(t, open)
}
