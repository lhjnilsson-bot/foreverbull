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

// heldClock holds NextClose until release closes, whatever its ctx says, so
// a loop can be caught mid-step.
type heldClock struct {
	*FakeClock
	entered chan struct{}
	release chan struct{}
}

func (c heldClock) NextClose(context.Context, string, *commonpb.Date) (*SessionClose, error) {
	c.entered <- struct{}{}
	<-c.release
	return nil, ErrNoSession
}

func heldRunner(t *testing.T) (*InProcess, heldClock) {
	t.Helper()
	store := NewMemoryStore()
	store.Put(runningDeployment())
	clock := heldClock{FakeClock: NewFakeClock(timeZero()), entered: make(chan struct{}, 1), release: make(chan struct{})}
	runner := New(store, clock, nil, nil)
	require.NoError(t, runner.Add(context.Background(), runningDeployment()))
	<-clock.entered
	return runner, clock
}

// Remove returns only once the loop has ended, so StopDeployment's STOPPED
// is the last write the row gets.
func TestRemoveWaitsForTheLoopToEnd(t *testing.T) {
	runner, clock := heldRunner(t)
	removed := make(chan error, 1)
	go func() { removed <- runner.Remove(context.Background(), "dep-1") }()

	select {
	case err := <-removed:
		t.Fatalf("Remove returned while the loop was still running: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(clock.release)
	select {
	case err := <-removed:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Remove didn't return after the loop ended")
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	require.Empty(t, runner.loops)
}

// A loop that doesn't end within ctx fails Remove with ctx's cause, and
// stays listed so a later Remove waits for it again.
func TestRemoveFailsWhenTheLoopOutlastsContext(t *testing.T) {
	runner, clock := heldRunner(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	err := runner.Remove(ctx, "dep-1")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, "wait for deployment dep-1 loop to stop: context deadline exceeded", err.Error())
	runner.mu.Lock()
	require.Contains(t, runner.loops, "dep-1")
	runner.mu.Unlock()

	close(clock.release)
	require.NoError(t, runner.Remove(context.Background(), "dep-1"))
	runner.mu.Lock()
	defer runner.mu.Unlock()
	require.Empty(t, runner.loops)
}

func timeZero() time.Time { return time.Date(2024, 3, 4, 12, 0, 0, 0, time.UTC) }
