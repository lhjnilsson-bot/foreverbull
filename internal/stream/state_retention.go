package stream

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"
)

const (
	// StateRetention is how long a message_states row is kept. A row copies
	// a state message from the foreverbull stream and carries the state's
	// time, so it goes when its message has left the stream. Rows go one
	// by one by their own age, not per task: a task running longer than
	// this loses its early rows (PUBLISHED, RECEIVED) while it still runs.
	// That is harmless, as nothing reads a task's earlier rows once a later
	// state is in, and the stream drops those messages the same way.
	StateRetention = MaxAge

	// statePruneInterval is how often old message_states rows are deleted.
	// Rows outlive StateRetention by at most this long.
	statePruneInterval = time.Hour

	// statePruneBatch rows are deleted per statement, and at most
	// statePruneBatches statements run per prune, so one prune deletes at
	// most a million rows. A larger backlog is left for the next prune.
	statePruneBatch   = 10_000
	statePruneBatches = 100
)

// statePruner deletes message_states rows older than StateRetention.
type statePruner struct {
	repository *Repository
	now        func() time.Time
	interval   time.Duration
	batch      int
	batches    int
}

func newStatePruner(repository *Repository) *statePruner {
	return &statePruner{
		repository: repository,
		now:        time.Now,
		interval:   statePruneInterval,
		batch:      statePruneBatch,
		batches:    statePruneBatches,
	}
}

// prune deletes rows that occurred before now minus StateRetention, in
// batches, until a batch comes back short or the batch count is reached. It
// returns how many rows it deleted.
func (p *statePruner) prune(ctx context.Context) (int64, error) {
	cutoff := p.now().Add(-StateRetention)
	var deleted int64
	for range p.batches {
		n, err := p.repository.DeleteStatesBefore(ctx, cutoff, p.batch)
		deleted += n
		if err != nil {
			return deleted, fmt.Errorf("error pruning message states: %w", err)
		}
		if n < int64(p.batch) {
			return deleted, nil
		}
	}
	log.Warn().Int64("rows", deleted).Int("batches", p.batches).
		Msg("message states prune hit its cap; the rest is left for the next prune")
	return deleted, nil
}

// run prunes at once and then every interval until ctx is done. It returns
// nil when ctx ends it, and the error of the first prune that fails.
func (p *statePruner) run(ctx context.Context) error {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		deleted, err := p.prune(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if deleted > 0 {
			log.Info().Int64("rows", deleted).Dur("retention", StateRetention).Msg("pruned message states")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
