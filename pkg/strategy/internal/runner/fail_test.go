package runner

import (
	"context"
	"errors"
	"testing"
	"time"

	commonpb "github.com/lhjnilsson/foreverbull/pkg/pb"
	pb "github.com/lhjnilsson/foreverbull/pkg/pb/strategy"
	"github.com/stretchr/testify/require"
)

type brokenClock struct {
	*FakeClock
	err error
}

func (c brokenClock) NextClose(context.Context, string, *commonpb.Date) (*SessionClose, error) {
	return nil, c.err
}

type brokenGetStore struct {
	*MemoryStore
	err error
}

func (s brokenGetStore) GetDeployment(context.Context, string) (*pb.Deployment, error) {
	return nil, s.err
}

func runningDeployment() *pb.Deployment {
	return &pb.Deployment{Id: "dep-1", Backend: "alpaca-paper", CalendarCode: "XNYS", DryRun: true, Status: pb.Deployment_RUNNING}
}

// A loop that stops on an error leaves the deployment ERROR with the step
// and cause, never RUNNING with no loop behind it.
func TestLoopMarksDeploymentError(t *testing.T) {
	cases := []struct {
		name  string
		build func(store *MemoryStore) *InProcess
		want  string
	}{
		{
			name: "next close fails",
			build: func(store *MemoryStore) *InProcess {
				return New(store, brokenClock{FakeClock: NewFakeClock(timeZero()), err: errors.New("calendar read failed")}, nil, nil)
			},
			want: "next close: calendar read failed",
		},
		{
			name: "deployment read fails",
			build: func(store *MemoryStore) *InProcess {
				return New(brokenGetStore{MemoryStore: store, err: errors.New("connection lost")}, NewFakeClock(timeZero()), nil, nil)
			},
			want: "get deployment: connection lost",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := NewMemoryStore()
			store.Put(runningDeployment())
			c.build(store).loop(context.Background(), "dep-1")

			d, err := store.GetDeployment(context.Background(), "dep-1")
			require.NoError(t, err)
			require.Equal(t, pb.Deployment_ERROR, d.GetStatus())
			require.Equal(t, c.want, d.GetError())
		})
	}
}

// A loop cancelled by Remove or Stop is not a failure, so a STOPPED or
// still-running row is left as it is.
func TestLoopCancelledKeepsStatus(t *testing.T) {
	store := NewMemoryStore()
	store.Put(runningDeployment())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runner := New(store, brokenClock{FakeClock: NewFakeClock(timeZero()), err: context.Canceled}, nil, nil)
	runner.fail(ctx, "dep-1", errors.New("next close: context canceled"))

	d, err := store.GetDeployment(context.Background(), "dep-1")
	require.NoError(t, err)
	require.Equal(t, pb.Deployment_RUNNING, d.GetStatus())
	require.Nil(t, d.Error)
}

func timeZero() time.Time { return time.Date(2024, 3, 4, 12, 0, 0, 0, time.UTC) }
