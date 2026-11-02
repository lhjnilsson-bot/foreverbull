package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"

	"github.com/lhjnilsson/foreverbull/pkg/backtest/engine"
	"github.com/lhjnilsson/foreverbull/pkg/backtest/internal/repository"
	"github.com/lhjnilsson/foreverbull/pkg/finance"
)

// listingLookback is how many sessions before the period start are fetched
// before a late first bar can count as a listing.
const listingLookback = 10

// SymbolInspector asks a provider about one symbol. from and to are the
// lookback window; both nil skips the listing-date and lookback fetch.
// An error is a failed lookup. It never means the symbol is gone.
type SymbolInspector func(ctx context.Context, db *pgxpool.Pool, provider, profile, symbol string, from, to *time.Time) (finance.SymbolFacts, error)

// symbolInspector is the provider lookup create uses. Tests replace it.
var symbolInspector SymbolInspector = finance.InspectSymbol

// SetSymbolInspector replaces the provider lookup and returns the previous one.
func SetSymbolInspector(fn SymbolInspector) func() {
	prev := symbolInspector
	symbolInspector = fn
	return func() { symbolInspector = prev }
}

// excuseEdges drops an edge gap that the provider confirms, and returns the
// events to store when every remaining gap is gone. A failed lookup is not a
// confirmation: that edge is dropped so it is reported as the lookup's cause,
// not as an unconfirmed edge. misses collects every failed lookup. A calendar
// failure, or any inspector error that is not a lookup, is returned.
func excuseEdges(ctx context.Context, db *pgxpool.Pool, eng engine.Engine, provider, profile, benchmark string,
	sessions []time.Time, bars map[string]*repository.SymbolBars, gaps []symbolGaps,
) ([]symbolGaps, []repository.SymbolEvent, []lookupMiss, error) {
	kept := make([]symbolGaps, 0, len(gaps))
	events := make([]repository.SymbolEvent, 0)
	var misses []lookupMiss
	for _, gap := range gaps {
		if !gap.noneAfter.IsZero() {
			event, ok, miss, err := confirmDelisting(ctx, db, provider, profile, benchmark, sessions, bars, gap)
			if err != nil {
				return nil, nil, nil, err
			}
			if miss != nil {
				misses = append(misses, *miss)
				gap.noneAfter = time.Time{}
			} else if ok {
				gap.noneAfter = time.Time{}
				events = append(events, event)
			}
		}
		if !gap.noneBefore.IsZero() {
			event, ok, miss, err := confirmListing(ctx, db, eng, provider, profile, sessions, bars, gap)
			if err != nil {
				return nil, nil, nil, err
			}
			if miss != nil {
				misses = append(misses, *miss)
				gap.noneBefore = time.Time{}
			} else if ok {
				gap.noneBefore = time.Time{}
				events = append(events, event)
			}
		}
		if len(gap.sessions) > 0 || gap.atEdge() || gap.noBars {
			kept = append(kept, gap)
		}
	}
	return kept, events, misses, nil
}

// confirmDelisting is the three-part delisting rule. A miss is not an error.
// A failed lookup is a lookupMiss: the symbol is not delisted.
func confirmDelisting(ctx context.Context, db *pgxpool.Pool, provider, profile, benchmark string,
	sessions []time.Time, bars map[string]*repository.SymbolBars, gap symbolGaps,
) (repository.SymbolEvent, bool, *lookupMiss, error) {
	if provider == "stooq" || gap.symbol == benchmark {
		return repository.SymbolEvent{}, false, nil, nil
	}
	if !benchmarkCoversAfter(bars[benchmark], sessions, gap.noneAfter) {
		return repository.SymbolEvent{}, false, nil, nil
	}
	facts, err := symbolInspector(ctx, db, provider, profile, gap.symbol, nil, nil)
	if err != nil {
		miss, unexpected := failedLookup(provider, gap.symbol, false, err)
		return repository.SymbolEvent{}, false, miss, unexpected
	}
	if !facts.Gone || facts.LaterTicker || facts.Evidence == "" {
		return repository.SymbolEvent{}, false, nil, nil
	}
	return repository.SymbolEvent{
		Symbol: gap.symbol, Kind: repository.EventDelisted, On: gap.noneAfter, Evidence: facts.Evidence,
	}, true, nil, nil
}

// confirmListing is the two-part listing rule. Only Yahoo can name a listing
// date. A failed lookup is a lookupMiss, not a listing. A calendar failure
// is returned so create stops instead of guessing. A first-trade date after
// the symbol's first stored bar contradicts the bars, so it is not a listing.
func confirmListing(ctx context.Context, db *pgxpool.Pool, eng engine.Engine, provider, profile string,
	sessions []time.Time, bars map[string]*repository.SymbolBars, gap symbolGaps,
) (repository.SymbolEvent, bool, *lookupMiss, error) {
	if provider != "yahoo" {
		return repository.SymbolEvent{}, false, nil, nil
	}
	stored := bars[gap.symbol]
	if stored == nil || !stored.First.Equal(gap.noneBefore) {
		return repository.SymbolEvent{}, false, nil, nil
	}
	window, err := lookbackSessions(ctx, eng, sessions)
	if err != nil {
		return repository.SymbolEvent{}, false, nil, err
	}
	if len(window) < listingLookback {
		return repository.SymbolEvent{}, false, nil, nil
	}
	from, to := window[0], window[len(window)-1]
	facts, err := symbolInspector(ctx, db, provider, profile, gap.symbol, &from, &to)
	if err != nil {
		miss, unexpected := failedLookup(provider, gap.symbol, true, err)
		return repository.SymbolEvent{}, false, miss, unexpected
	}
	lastMissing, ok := lastSessionBefore(sessions, gap.noneBefore)
	if !ok || !facts.LookbackEmpty || facts.FirstTrade.IsZero() || facts.FirstTrade.Before(lastMissing) ||
		facts.FirstTrade.After(gap.noneBefore) {
		return repository.SymbolEvent{}, false, nil, nil
	}
	return repository.SymbolEvent{
		Symbol:   gap.symbol,
		Kind:     repository.EventListed,
		On:       gap.noneBefore,
		Evidence: finance.EvidenceYahooListed(facts.FirstTrade),
	}, true, nil, nil
}

// failedLookup logs a failed lookup and splits it into the classified miss
// the user sentence is built from, or an unexpected error. The raw error is
// the log line. It is not the user sentence.
func failedLookup(provider, symbol string, listing bool, err error) (*lookupMiss, error) {
	kind := "delisting"
	if listing {
		kind = "listing"
	}
	log.Warn().Err(err).Str("symbol", symbol).Str("provider", provider).Str("lookup", kind).
		Msg("listing or delisting lookup failed")
	var lookup *finance.LookupError
	if !errors.As(err, &lookup) {
		return nil, err
	}
	return &lookupMiss{symbol: symbol, listing: listing, cause: lookup}, nil
}

// benchmarkCoversAfter is true when the benchmark has a bar on every session
// after last. The symbol's own bars stopping is not enough: the benchmark
// has to keep going, or the gap is the provider's.
func benchmarkCoversAfter(bench *repository.SymbolBars, sessions []time.Time, last time.Time) bool {
	if bench == nil {
		return false
	}
	have := make(map[time.Time]bool, len(bench.Days))
	for _, day := range bench.Days {
		have[day] = true
	}
	saw := false
	for _, session := range sessions {
		if !session.After(last) {
			continue
		}
		saw = true
		if !have[session] {
			return false
		}
	}
	return saw
}

// lookbackSessions is the listingLookback sessions immediately before the
// period. The period's first session is sessions[0].
func lookbackSessions(ctx context.Context, eng engine.Engine, sessions []time.Time) ([]time.Time, error) {
	if len(sessions) == 0 {
		return nil, fmt.Errorf("no sessions to look back from")
	}
	start := sessions[0]
	from := start.AddDate(0, 0, -40)
	to := start.AddDate(0, 0, -1)
	found, err := eng.Sessions(ctx, from, to)
	if err != nil {
		return nil, err
	}
	if len(found) > listingLookback {
		found = found[len(found)-listingLookback:]
	}
	return found, nil
}

// lastSessionBefore is the latest session strictly before day.
func lastSessionBefore(sessions []time.Time, day time.Time) (time.Time, bool) {
	var last time.Time
	for _, session := range sessions {
		if session.Before(day) {
			last = session
		}
	}
	return last, !last.IsZero()
}
