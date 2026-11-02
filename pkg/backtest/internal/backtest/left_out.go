package backtest

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/lhjnilsson/foreverbull/pkg/backtest/internal/repository"
	"github.com/lhjnilsson/foreverbull/pkg/finance/supplier"
	commonPB "github.com/lhjnilsson/foreverbull/pkg/pb"
	pb "github.com/lhjnilsson/foreverbull/pkg/pb/backtest"
	"github.com/lhjnilsson/foreverbull/pkg/pb/finance"
)

// lastBarsByName is the membership lookup that also reads each member's last
// stored bar. repository.UniverseMember implements it.
type lastBarsByName interface {
	LastBars(ctx context.Context, backtestName, provider string, end time.Time) ([]repository.AliasLastBar, error)
}

type lastBarsByID interface {
	LastBarsByID(ctx context.Context, backtestID, provider string, end time.Time) ([]repository.AliasLastBar, error)
}

// leftOut records the members a run passed over because their prices had
// ended: an ETF member that was delisted keeps its membership until the next
// month-end holdings list drops it, but it has no bar to trade on. Each
// symbol keeps the date of its last bar. The runner goroutine writes it and
// RunNotices reads it after the period channel has closed.
//
// held names the left-out symbols the portfolio held on the session they
// were left out. Zipline auto-closes such a position at the end of that
// session at the last close (universe.md §8), so the notice explains a
// trade the strategy never placed.
type leftOut struct {
	lastBar map[string]time.Time
	held    map[string]bool
	holding map[string]bool
}

func newLeftOut() *leftOut {
	return &leftOut{
		lastBar: make(map[string]time.Time),
		held:    make(map[string]bool),
		holding: make(map[string]bool),
	}
}

// sawPortfolio records the open positions of the session about to be
// filtered. Zipline auto-closes at the end of a session, so the session a
// member is left out still holds its position. The universe bounds the map.
func (l *leftOut) sawPortfolio(portfolio *finance.Portfolio) {
	if l == nil {
		return
	}
	clear(l.holding)
	for _, position := range portfolio.GetPositions() {
		if position.GetAmount() != 0 {
			l.holding[position.GetSymbol()] = true
		}
	}
}

// barsEnd is the exclusive upper bound on the bars the bundle holds: the day
// after the backtest's last period. A later download for another backtest
// can store bars past it, and those are not in this run's bundle.
func barsEnd(stored *pb.Backtest) time.Time {
	periods := stored.GetPeriods()
	if len(periods) > 0 && periods[len(periods)-1].GetEndDate() != nil {
		return commonPB.DateToTime(periods[len(periods)-1].GetEndDate()).AddDate(0, 0, 1)
	}
	return time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
}

func barsProvider(stored *pb.Backtest) string {
	provider := strings.TrimSpace(stored.GetProvider())
	if provider == "" {
		return supplier.DefaultProvider
	}
	return provider
}

// withoutEndedPrices wraps a member source so a confirmed delisting is not
// passed to Process once its last bar is before t. confirmed is the symbol
// set create stored. A symbol whose bars merely ended is kept: that is a
// gap, not a delisting. A member whose last bar is the newest bar of any
// member is not left out either: its prices did not end, the stored data
// does.
func withoutEndedPrices(source symbolSource, aliases []repository.AliasLastBar, left *leftOut, confirmed map[string]bool) symbolSource {
	newest := newestBar(aliases)
	return func(ctx context.Context, t time.Time) ([]string, error) {
		symbols, err := source(ctx, t)
		if err != nil {
			return nil, err
		}
		day := dayOf(t)
		kept := make([]string, 0, len(symbols))
		for _, symbol := range symbols {
			last, ok := lastBarAt(aliases, symbol, day)
			if ok && confirmed[symbol] && last.Before(day) && last.Before(newest) {
				if previous, seen := left.lastBar[symbol]; !seen || last.After(previous) {
					left.lastBar[symbol] = last
				}
				if left.holding[symbol] {
					left.held[symbol] = true
				}
				continue
			}
			kept = append(kept, symbol)
		}
		return kept, nil
	}
}

// newestBar is the UTC date of the newest bar any member alias has: the
// last session the stored prices reach.
func newestBar(aliases []repository.AliasLastBar) time.Time {
	var newest time.Time
	for _, alias := range aliases {
		if alias.LastBar == nil {
			continue
		}
		if last := dayOf(*alias.LastBar); last.After(newest) {
			newest = last
		}
	}
	return newest
}

// lastBarAt returns the UTC date of the last bar of the ISIN that holds
// symbol on day. ok is false when no alias of symbol covers day or the ISIN
// has no bar.
func lastBarAt(aliases []repository.AliasLastBar, symbol string, day time.Time) (time.Time, bool) {
	for _, alias := range aliases {
		if alias.Symbol != symbol || alias.LastBar == nil {
			continue
		}
		if day.Before(dayOf(alias.FromDate)) {
			continue
		}
		if alias.ToDate != nil && !day.Before(dayOf(*alias.ToDate)) {
			continue
		}
		return dayOf(*alias.LastBar), true
	}
	return time.Time{}, false
}

func dayOf(t time.Time) time.Time {
	year, month, day := t.UTC().Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

// notices is one line per last-bar date, oldest first, with the symbols
// sorted: "3 symbols were delisted after their last price on 2026-03-31 and were left out: ABC, DEF, GHI."
// A line with a held symbol ends "Positions held were closed at the last price on 2026-03-31."
func (l *leftOut) notices() []string {
	if l == nil || len(l.lastBar) == 0 {
		return nil
	}
	byDate := make(map[time.Time][]string)
	heldOn := make(map[time.Time]bool)
	for symbol, last := range l.lastBar {
		byDate[last] = append(byDate[last], symbol)
		if l.held[symbol] {
			heldOn[last] = true
		}
	}
	dates := make([]time.Time, 0, len(byDate))
	for day := range byDate {
		dates = append(dates, day)
	}
	slices.SortFunc(dates, func(a, b time.Time) int { return a.Compare(b) })
	lines := make([]string, 0, len(dates))
	for _, day := range dates {
		lines = append(lines, leftOutLine(day, byDate[day], heldOn[day]))
	}
	return lines
}

// leftOutLine names every symbol delisted on last. When any of them was
// held, the line says the positions were closed at that day's price.
func leftOutLine(last time.Time, symbols []string, held bool) string {
	slices.Sort(symbols)
	names := strings.Join(symbols, ", ")
	date := last.Format(time.DateOnly)
	line := fmt.Sprintf("%d symbols were delisted after their last price on %s and were left out: %s.", len(symbols), date, names)
	if len(symbols) == 1 {
		line = fmt.Sprintf("1 symbol was delisted after its last price on %s and was left out: %s.", date, names)
	}
	if held {
		line += fmt.Sprintf(" Positions held were closed at the last price on %s.", date)
	}
	return line
}
