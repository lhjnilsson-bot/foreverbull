package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lhjnilsson/foreverbull/internal/stream"
	internalPb "github.com/lhjnilsson/foreverbull/internal/stream/pb"
	"github.com/lhjnilsson/foreverbull/internal/usercopy"
	"github.com/lhjnilsson/foreverbull/pkg/backtest/internal/repository"
	"github.com/rs/zerolog/log"
)

// queueFullCleanupTimeout bounds the delete after a create failed on a full
// task queue. It runs on a context that outlives a client that left.
const queueFullCleanupTimeout = 10 * time.Second

// createdBacktest is the backtest a create stores, found by name, or by
// owner and name when auth is on. exists answers whether it is stored, and
// remove deletes it, returning pgx.ErrNoRows when it isn't.
type createdBacktest struct {
	name   string
	exists func(context.Context) (bool, error)
	remove func(context.Context) error
}

// queueFullCreate follows a create's orchestration for a step that failed on
// a full task queue (stream.StepNotRecorded). The worker never reached its
// own cleanup then: a refused COMPLETED leaves what the step stored. Only a
// backtest that didn't exist before this create is this create's own, so
// only then is it removed. The failed step's message becomes the whole
// sentence, which the client prints as the error.
//
// The backtest is removed by name: the create never learns the id the
// worker stored, and the steps don't carry it. That leaves one race. Two
// creates of the same name start together, both before either stored it;
// one stores it, and the other's CREATING is refused by a full queue before
// it ran. That one then removes the first one's backtest. It needs a full
// queue and two creates of one name at once, and the first create's later
// steps then fail on the missing backtest rather than leave a wrong one.
type queueFullCreate struct {
	backtest      createdBacktest
	existedBefore bool
}

// newQueueFullCreate reads whether the backtest exists before the create
// runs. A failed read fails the create before anything is sent.
func newQueueFullCreate(ctx context.Context, backtest createdBacktest) (*queueFullCreate, error) {
	existed, err := backtest.exists(ctx)
	if err != nil {
		return nil, fmt.Errorf("error checking backtest %s before create: %w", backtest.name, err)
	}
	return &queueFullCreate{backtest: backtest, existedBefore: existed}, nil
}

// rewrite replaces a full-queue step's bare cause with the create's
// sentence, after removing what this create stored. Other steps pass
// through unchanged.
func (q *queueFullCreate) rewrite(ctx context.Context, step *internalPb.Orchestration_Step) {
	if !stream.StepNotRecorded(step) {
		return
	}
	sentence := usercopy.BacktestCreateQueueFull(q.backtest.name, q.removeOwn(ctx))
	step.Error = &sentence
}

// removeOwn deletes the backtest when this create stored it, and reports
// whether nothing of this create is left. A delete that fails is logged
// and reported as not removed, so the sentence says to delete it first.
func (q *queueFullCreate) removeOwn(ctx context.Context) bool {
	if q.existedBefore {
		// The create was refused before it stored anything: a stored
		// backtest of the same name makes the create itself fail.
		return true
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), queueFullCleanupTimeout)
	defer cancel()
	err := q.backtest.remove(cleanupCtx)
	if err == nil || errors.Is(err, pgx.ErrNoRows) {
		return true
	}
	log.Error().Err(err).Str("backtest", q.backtest.name).Msg("error deleting backtest after its create failed on a full task queue")
	return false
}

// namedBacktest is the created backtest by name, with auth off.
func namedBacktest(backtests repository.Backtest, name string) createdBacktest {
	return createdBacktest{
		name: name,
		exists: func(ctx context.Context) (bool, error) {
			_, err := backtests.Get(ctx, name)
			if errors.Is(err, pgx.ErrNoRows) {
				return false, nil
			}
			return err == nil, err
		},
		remove: func(ctx context.Context) error { return backtests.Delete(ctx, name) },
	}
}

// ownedBacktest is the created backtest by owner and name, with auth on.
func ownedBacktest(backtests repository.Backtest, ownerID, name string) createdBacktest {
	return createdBacktest{
		name: name,
		exists: func(ctx context.Context) (bool, error) {
			_, err := backtests.IDByOwnerAndName(ctx, ownerID, name)
			if errors.Is(err, pgx.ErrNoRows) {
				return false, nil
			}
			return err == nil, err
		},
		remove: func(ctx context.Context) error {
			id, err := backtests.IDByOwnerAndName(ctx, ownerID, name)
			if err != nil {
				return err
			}
			return backtests.DeleteByID(ctx, id)
		},
	}
}
