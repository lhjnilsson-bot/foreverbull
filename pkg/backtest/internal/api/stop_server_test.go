package api

import (
	"context"
	"errors"
	"testing"

	"github.com/lhjnilsson/foreverbull/pkg/pb/backtest"
	servicePb "github.com/lhjnilsson/foreverbull/pkg/pb/backtest/service"
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
	activity := make(chan bool, ActivityBufferSize)
	server := &grpcSessionServer{session: &backtest.Session{Id: "s-1", Backtest: "spy"}, wp: pool, activity: activity}

	_, err := server.StopServer(context.Background(), &servicePb.StopServerRequest{})
	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.Internal, st.Code())
	require.Equal(t, "Backtest 'spy' finished, but foreverbull couldn't stop the processes running your strategy. "+
		"Run `fbenv status` to check the environment before the next run.", st.Message())
	_, open := <-activity
	require.False(t, open)
}
