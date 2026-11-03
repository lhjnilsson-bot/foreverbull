package api

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/lhjnilsson/foreverbull/internal/stream"
	internalPb "github.com/lhjnilsson/foreverbull/internal/stream/pb"
	"github.com/lhjnilsson/foreverbull/internal/usercopy"
	"github.com/lhjnilsson/foreverbull/pkg/pb"
	"github.com/stretchr/testify/require"
)

// fakeCreated is a created backtest whose reads and deletes are set by the
// test, and which records whether it was removed.
type fakeCreated struct {
	existsErr error
	existed   bool
	removeErr error
	removed   int
}

func (f *fakeCreated) backtest() createdBacktest {
	return createdBacktest{
		name:   "spy",
		exists: func(context.Context) (bool, error) { return f.existed, f.existsErr },
		remove: func(context.Context) error { f.removed++; return f.removeErr },
	}
}

func queueFullStep() *internalPb.Orchestration_Step {
	cause := stream.ErrStateNotRecorded.Error()
	return &internalPb.Orchestration_Step{Num: 2, State: pb.State_STATE_FAILED, Error: &cause}
}

const (
	createQueueFullRemoved = "Couldn't load data for backtest 'spy': foreverbull's task queue is full, so it couldn't record the task's progress. " +
		"The backtest wasn't saved. Old tasks leave the queue after 24 hours, so run `fbull backtest create` again once they have. " +
		"Restarting fbenv won't clear the queue."
	createQueueFullPartly = "Couldn't load data for backtest 'spy': foreverbull's task queue is full, so it couldn't record the task's progress. " +
		"The backtest was partly saved and couldn't be removed. Run `fbull backtest delete spy` first. " +
		"Old tasks leave the queue after 24 hours, so run `fbull backtest create` again once they have. " +
		"Restarting fbenv won't clear the queue."
)

func TestQueueFullCreateRemovesTheBacktestItStored(t *testing.T) {
	for name, removeErr := range map[string]error{"deleted": nil, "already gone": pgx.ErrNoRows} {
		f := &fakeCreated{removeErr: removeErr}
		q, err := newQueueFullCreate(context.Background(), f.backtest())
		require.NoError(t, err, name)
		step := queueFullStep()
		q.rewrite(context.Background(), step)
		require.Equal(t, createQueueFullRemoved, step.GetError(), name)
		require.Equal(t, 1, f.removed, name)
	}
}

func TestQueueFullCreateSaysToDeleteABacktestItCouldntRemove(t *testing.T) {
	f := &fakeCreated{removeErr: errors.New("connection reset")}
	q, err := newQueueFullCreate(context.Background(), f.backtest())
	require.NoError(t, err)
	step := queueFullStep()
	q.rewrite(context.Background(), step)
	require.Equal(t, createQueueFullPartly, step.GetError())
	require.NotContains(t, step.GetError(), "connection reset")
}

func TestQueueFullCreateKeepsABacktestThatExistedBefore(t *testing.T) {
	f := &fakeCreated{existed: true}
	q, err := newQueueFullCreate(context.Background(), f.backtest())
	require.NoError(t, err)
	step := queueFullStep()
	q.rewrite(context.Background(), step)
	require.Equal(t, createQueueFullRemoved, step.GetError())
	require.Zero(t, f.removed, "a backtest that existed before the create isn't this create's")
}

func TestQueueFullCreateLeavesOtherStepsAlone(t *testing.T) {
	f := &fakeCreated{}
	q, err := newQueueFullCreate(context.Background(), f.backtest())
	require.NoError(t, err)
	other := "ingest failed"
	for _, step := range []*internalPb.Orchestration_Step{
		{Num: 3, State: pb.State_STATE_FAILED, Error: &other},
		{Num: 2, State: pb.State_STATE_RUNNING, Error: stringPtr(usercopy.TaskStateNotRecorded)},
		{Num: 1, State: pb.State_STATE_COMPLETED},
	} {
		before := step.GetError()
		q.rewrite(context.Background(), step)
		require.Equal(t, before, step.GetError())
	}
	require.Zero(t, f.removed)
}

func TestQueueFullCreateFailsWhenItCantCheckTheBacktest(t *testing.T) {
	f := &fakeCreated{existsErr: errors.New("connection reset")}
	_, err := newQueueFullCreate(context.Background(), f.backtest())
	require.ErrorContains(t, err, "error checking backtest spy before create")
}

func stringPtr(s string) *string { return &s }
