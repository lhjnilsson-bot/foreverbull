package command

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lhjnilsson/foreverbull/internal/stream"
	"github.com/lhjnilsson/foreverbull/pkg/backtest/engine"
	"github.com/lhjnilsson/foreverbull/pkg/backtest/internal/repository"
	"github.com/lhjnilsson/foreverbull/pkg/backtest/internal/stream/dependency"
	"github.com/lhjnilsson/foreverbull/pkg/finance"
	"github.com/lhjnilsson/foreverbull/pkg/finance/supplier"
	pb "github.com/lhjnilsson/foreverbull/pkg/pb"
	"github.com/lhjnilsson/foreverbull/pkg/pb/backtest"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// symbolGaps is one symbol's missing days, as indexes into the days
// missingSessions returns, oldest first. closed is set when one of them is
// not a session: a provider reported no price on a holiday. noneBefore is
// the symbol's first day with prices when the period has sessions before
// it, and noneAfter its last day with prices when the period has sessions
// after it. Those sessions are not in sessions: the edge is named once.
// noBars is set when the symbol has no stored bar at all; from and to are
// then the period's first and last sessions.
type symbolGaps struct {
	symbol     string
	sessions   []int
	closed     bool
	noneBefore time.Time
	noneAfter  time.Time
	noBars     bool
	from, to   time.Time
}

// atEdge is true when the symbol has no prices at the start or the end of
// the period.
func (gap symbolGaps) atEdge() bool {
	return !gap.noneBefore.IsZero() || !gap.noneAfter.IsZero()
}

// uncovered is true when part of the period has no prices at all for the
// symbol: an edge, or no bars.
func (gap symbolGaps) uncovered() bool {
	return gap.atEdge() || gap.noBars
}

// providerMissingPrice matches the finance service's sentence for sessions
// the provider sent no price for, anywhere in the range. Only the days are
// read from it; the symbol named to the user is the request symbol.
var providerMissingPrice = regexp.MustCompile(
	`^(?:yahoo|stooq|alpaca) sent no price for [A-Za-z0-9.^=_-]{1,32} on (\d{4}-\d{2}-\d{2}(?:, \d{4}-\d{2}-\d{2})*)$`)

// providerMissingDays reads the days from that sentence.
func providerMissingDays(text string) ([]time.Time, bool) {
	match := providerMissingPrice.FindStringSubmatch(text)
	if match == nil {
		return nil, false
	}
	days := make([]time.Time, 0)
	for _, field := range strings.Split(match[1], ", ") {
		day, err := time.Parse(time.DateOnly, field)
		if err != nil {
			return nil, false
		}
		days = append(days, day)
	}
	return days, true
}

// missingSessions lists, per symbol in the order given, the period's
// sessions with no bar, and every day the provider reported with no price.
// sessions are the period's, capped at the last completed session. bars and
// reported are keyed by the symbols given, which are what the user sees.
//
// With coverage set, every session of the period counts. Sessions before
// the symbol's first stored bar or after its last, over all stored time,
// are a gap at the edge of the period. A late first bar or an early last
// bar is a listing or a delisting only when the provider confirms it.
// A symbol with no stored bar at all has no prices for the period. Without
// coverage (Zipline's refusal, which names its own days) only the reported
// days count.
//
// A day that is not a session (a holiday) is missing only when the
// provider reported it: it failed that symbol's download, so it is never
// passed over. A reported symbol that is not in symbols comes last. The
// days are the sessions with the reported days that are not sessions, in
// order; gaps index them.
func missingSessions(sessions []time.Time, symbols []string, bars map[string]*repository.SymbolBars,
	reported map[string][]time.Time, coverage bool,
) ([]time.Time, []symbolGaps) {
	isSession := make(map[time.Time]bool, len(sessions))
	for _, session := range sessions {
		isSession[session] = true
	}
	days := slices.Clone(sessions)
	listed := make(map[string]bool, len(symbols))
	for _, symbol := range symbols {
		listed[symbol] = true
	}
	order := slices.Clone(symbols)
	extra := make([]string, 0)
	for symbol, reportedDays := range reported {
		if !listed[symbol] && len(reportedDays) > 0 {
			extra = append(extra, symbol)
		}
		for _, day := range reportedDays {
			if !isSession[day] && !slices.Contains(days, day) {
				days = append(days, day)
			}
		}
	}
	slices.Sort(extra)
	order = append(order, extra...)
	slices.SortFunc(days, func(a, b time.Time) int { return a.Compare(b) })

	gaps := make([]symbolGaps, 0)
	for _, symbol := range order {
		gap := symbolGaps{symbol: symbol}
		stored := bars[symbol]
		sent := make(map[time.Time]bool)
		for _, day := range reported[symbol] {
			sent[day] = true
		}
		if coverage && stored == nil && len(sent) == 0 && len(sessions) > 0 && listed[symbol] {
			gap.noBars, gap.from, gap.to = true, sessions[0], sessions[len(sessions)-1]
			gaps = append(gaps, gap)
			continue
		}
		have := make(map[time.Time]bool)
		if stored != nil {
			for _, day := range stored.Days {
				have[day] = true
			}
		}
		for i, day := range days {
			switch {
			case sent[day]:
				gap.sessions = append(gap.sessions, i)
				gap.closed = gap.closed || !isSession[day]
			case !coverage || !isSession[day] || stored == nil || have[day]:
			case day.Before(stored.First):
				gap.noneBefore = stored.First
			case day.After(stored.Last):
				gap.noneAfter = stored.Last
			default:
				gap.sessions = append(gap.sessions, i)
			}
		}
		if len(gap.sessions) > 0 || gap.atEdge() {
			gaps = append(gaps, gap)
		}
	}
	return days, gaps
}

// sessionRanges writes missing days as dates, and a run of days in a row as
// "A to B". A weekend or holiday inside a run does not break it.
func sessionRanges(sessions []time.Time, missing []int) string {
	parts := make([]string, 0, len(missing))
	for i := 0; i < len(missing); {
		j := i
		for j+1 < len(missing) && missing[j+1] == missing[j]+1 {
			j++
		}
		first := sessions[missing[i]].Format(time.DateOnly)
		if j == i {
			parts = append(parts, first)
		} else {
			parts = append(parts, first+" to "+sessions[missing[j]].Format(time.DateOnly))
		}
		i = j + 1
	}
	return strings.Join(parts, ", ")
}

// listedOrder keeps the benchmark, which ingestSymbols puts first, and sorts
// the rest A to Z. The order a symbol list was written in is not stored, and
// ETF members are already read A to Z, so this is the same order every time.
func listedOrder(symbols []string) []string {
	if len(symbols) == 0 {
		return symbols
	}
	rest := slices.Clone(symbols[1:])
	slices.Sort(rest)
	return append([]string{symbols[0]}, rest...)
}

const missingSessionsNextStep = "Try another provider with --provider, or change the backtest dates, " +
	"then run `fbull backtest create` again."

// missingSessionsMessage is the failed DOWNLOADING step's message. One symbol
// with one cause fits one sentence. More get one line each under the lead,
// in the order given, with every day shown. The CLI prints the message as
// is. When a named day is not a session (a provider reported a holiday),
// the days aren't called trading days. Part of the period with no prices at
// all has its own copy (coverageMessage). When the backtest couldn't be
// removed, it was partly saved, and the step says to delete it first.
func missingSessionsMessage(name, provider string, days []time.Time, gaps []symbolGaps, removed bool) string {
	for _, gap := range gaps {
		if gap.uncovered() {
			return coverageMessage(name, provider, days, gaps, removed)
		}
	}
	saved, next := savedAndNext(name, missingSessionsNextStep, removed)
	trading := true
	for _, gap := range gaps {
		trading = trading && !gap.closed
	}
	if len(gaps) == 1 {
		gap := gaps[0]
		named := sessionRanges(days, gap.sessions)
		switch {
		case len(gap.sessions) > 1 && trading:
			named = fmt.Sprintf("%d trading days: %s", len(gap.sessions), named)
		case len(gap.sessions) > 1:
			named = fmt.Sprintf("%d days: %s", len(gap.sessions), named)
		case trading:
			named += ", a trading day"
		}
		return fmt.Sprintf("Couldn't load data for backtest '%s': %s is missing prices for %s on %s. %s %s",
			name, provider, gap.symbol, named, saved, next)
	}
	lines := []string{missingSessionsLead(name, provider, len(gaps), trading, saved)}
	for _, gap := range gaps {
		lines = append(lines, fmt.Sprintf("  %s: %s", gap.symbol, sessionRanges(days, gap.sessions)))
	}
	return strings.Join(append(lines, next), "\n")
}

// missingSessionsLead is the first line when the message lists symbols.
func missingSessionsLead(name, provider string, symbols int, trading bool, saved string) string {
	noun := "symbols"
	if symbols == 1 {
		noun = "symbol"
	}
	if trading {
		return fmt.Sprintf("Couldn't load data for backtest '%s': %s is missing prices on trading days for %d %s. %s",
			name, provider, symbols, noun, saved)
	}
	return fmt.Sprintf("Couldn't load data for backtest '%s': %s is missing prices for %d %s. %s",
		name, provider, symbols, noun, saved)
}

// unconfirmedClause is the end of one symbol's sentence when the provider
// did not confirm the edge. The provider is the one the backtest asked.
func unconfirmedClause(provider string, before, after bool) string {
	switch {
	case before && after:
		return provider + " didn't confirm a listing or delisting, so foreverbull treats it as missing data."
	case before:
		return provider + " didn't confirm it was listed then, so foreverbull treats it as missing data."
	default:
		return provider + " didn't confirm it was delisted then, so foreverbull treats it as missing data."
	}
}

// unconfirmedReason is the shared sentence under a block of symbols.
func unconfirmedReason(provider string, before, after bool) string {
	switch {
	case before && after:
		return provider + " didn't confirm a listing or delisting, so foreverbull treats it as missing data."
	case before:
		return provider + " didn't confirm a listing, so foreverbull treats it as missing data."
	default:
		return provider + " didn't confirm a delisting, so foreverbull treats it as missing data."
	}
}

// edgeStep is the next step for gaps at the edges of the period only: a
// start on or after the latest first day with prices, an end on or before
// the earliest last day with prices, or the symbols removed. When no such
// dates leave a period inside first through last (the period's sessions),
// only removing the symbols is offered.
func edgeStep(gaps []symbolGaps, first, last time.Time) string {
	var start, end time.Time
	symbols := make([]string, 0, len(gaps))
	for _, gap := range gaps {
		if gap.noneBefore.After(start) {
			start = gap.noneBefore
		}
		if !gap.noneAfter.IsZero() && (end.IsZero() || gap.noneAfter.Before(end)) {
			end = gap.noneAfter
		}
		symbols = append(symbols, gap.symbol)
	}
	fits := (start.IsZero() || end.IsZero() || !start.After(end)) &&
		(last.IsZero() || !start.After(last)) && (first.IsZero() || end.IsZero() || !end.Before(first))
	if !fits {
		return fmt.Sprintf("Remove %s, then run `fbull backtest create` again.", joinSymbols(symbols))
	}
	dates := make([]string, 0, 2)
	if !start.IsZero() {
		dates = append(dates, "a start on or after "+start.Format(time.DateOnly))
	}
	if !end.IsZero() {
		dates = append(dates, "an end on or before "+end.Format(time.DateOnly))
	}
	return fmt.Sprintf("Choose %s, or remove %s, then run `fbull backtest create` again.",
		strings.Join(dates, " and "), joinSymbols(symbols))
}

// joinSymbols writes A, A and B, or A, B and C.
func joinSymbols(symbols []string) string {
	if len(symbols) < 2 {
		return strings.Join(symbols, "")
	}
	return strings.Join(symbols[:len(symbols)-1], ", ") + " and " + symbols[len(symbols)-1]
}

// edgeCause is "no prices before A", "no prices after B", or "no prices
// before A or after B".
func edgeCause(gap symbolGaps) string {
	parts := make([]string, 0, 2)
	if !gap.noneBefore.IsZero() {
		parts = append(parts, "before "+gap.noneBefore.Format(time.DateOnly))
	}
	if !gap.noneAfter.IsZero() {
		parts = append(parts, "after "+gap.noneAfter.Format(time.DateOnly))
	}
	return "no prices " + strings.Join(parts, " or ")
}

// noBarsCause is the period a symbol with no stored bar has no prices for.
func noBarsCause(gap symbolGaps) string {
	return fmt.Sprintf("no prices between %s and %s", gap.from.Format(time.DateOnly), gap.to.Format(time.DateOnly))
}

// coverageLine is one symbol's line under the lead: its edges or its empty
// period as a sentence, with any missing day inside after it, or the
// list of missing days alone.
func coverageLine(days []time.Time, gap symbolGaps) string {
	switch {
	case gap.noBars:
		return fmt.Sprintf("  %s: %s.", gap.symbol, noBarsCause(gap))
	case gap.atEdge() && len(gap.sessions) > 0:
		return fmt.Sprintf("  %s: %s, and none on %s.", gap.symbol, edgeCause(gap), sessionRanges(days, gap.sessions))
	case gap.atEdge():
		return fmt.Sprintf("  %s: %s.", gap.symbol, edgeCause(gap))
	default:
		return fmt.Sprintf("  %s: %s", gap.symbol, sessionRanges(days, gap.sessions))
	}
}

// coverageStep is the next step for gaps that are not lookup failures.
func coverageStep(days []time.Time, gaps []symbolGaps) string {
	inside, noBars := false, false
	edges := make([]symbolGaps, 0, len(gaps))
	for _, gap := range gaps {
		inside = inside || len(gap.sessions) > 0
		noBars = noBars || gap.noBars
		if gap.atEdge() {
			edges = append(edges, gap)
		}
	}
	switch {
	case inside:
		return missingSessionsNextStep
	case noBars:
		return noPriceDataNextStep
	default:
		return edgeStep(edges, periodSession(days, true), periodSession(days, false))
	}
}

// coverageMessage is the message when part of the period has no prices at
// all for some symbol: before its first bar, after its last, or no bar.
// One symbol with that one cause fits one sentence:
//
//	Couldn't load data for backtest 'b': NEWCO has no prices before 2024-02-15, and yahoo didn't confirm it was
//	listed then, so foreverbull treats it as missing data. The backtest wasn't saved. Choose a start on or after
//	2024-02-15, or remove NEWCO, then run `fbull backtest create` again.
//
// Anything more lists every symbol's causes, one line each, under the lead,
// with the reason and the step last. A missing day inside a span makes the
// step the provider or dates one, and a symbol with no bars the symbol,
// dates or provider one.
func coverageMessage(name, provider string, days []time.Time, gaps []symbolGaps, removed bool) string {
	before, after, trading := false, false, true
	for _, gap := range gaps {
		before = before || !gap.noneBefore.IsZero()
		after = after || !gap.noneAfter.IsZero()
		trading = trading && !gap.closed
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	saved, next := savedAndNext(name, coverageStep(days, gaps), removed)
	if gap := gaps[0]; len(gaps) == 1 && len(gap.sessions) == 0 {
		if gap.noBars {
			return fmt.Sprintf("Couldn't load data for backtest '%s': %s has no prices for %s between %s and %s. %s %s",
				name, provider, gap.symbol, gap.from.Format(time.DateOnly), gap.to.Format(time.DateOnly), saved, next)
		}
		return fmt.Sprintf("Couldn't load data for backtest '%s': %s has %s, and %s %s %s",
			name, gap.symbol, edgeCause(gap), unconfirmedClause(provider, before, after), saved, next)
	}
	lines := []string{missingSessionsLead(name, provider, len(gaps), trading, saved)}
	for _, gap := range gaps {
		lines = append(lines, coverageLine(days, gap))
	}
	reason := ""
	if before || after {
		reason = unconfirmedReason(provider, before, after) + " "
	}
	return strings.Join(append(lines, reason+next), "\n")
}

// periodSession is the first (or last) day of days, the period's sessions
// with any reported day, or the zero time when there is none.
func periodSession(days []time.Time, first bool) time.Time {
	if len(days) == 0 {
		return time.Time{}
	}
	if first {
		return days[0]
	}
	return days[len(days)-1]
}

// sessionClock is the clock the check caps the period with.
var sessionClock = time.Now

// checkUnit is one instrument the check reads: the name the user sees and
// every ticker its bars may be stored under.
type checkUnit struct {
	name    string
	tickers []repository.Ticker
}

// checkUnits groups the request symbols by instrument. A symbol that
// belongs to one instrument in asset_symbol is read with that instrument's
// tickers, since a bar may be stored under any of them (Yahoo keeps FB's
// history under META, universe.md §7.4), each only inside the dates it was
// the instrument's ticker. A ticker another instrument also held is never
// in a group. Such a group is named
// by its latest ticker with the others after it: "META (earlier FB)". Any
// other symbol is read alone under its stored name and named as requested.
// alias maps each request symbol to its unit's name.
func checkUnits(symbols []string, tickers map[string][]repository.Ticker) ([]checkUnit, map[string]string) {
	units := make([]checkUnit, 0, len(symbols))
	alias := make(map[string]string, len(symbols))
	owner := make(map[string]string)
	for _, symbol := range symbols {
		stored := finance.StoredSymbol(symbol)
		if name, ok := owner[stored]; ok {
			alias[symbol] = name
			continue
		}
		group := tickers[stored]
		name := symbol
		if len(group) > 1 {
			earlier := make([]string, 0, len(group)-1)
			for _, ticker := range group[:len(group)-1] {
				earlier = append(earlier, ticker.Symbol)
			}
			name = fmt.Sprintf("%s (earlier %s)", group[len(group)-1].Symbol, strings.Join(earlier, ", "))
		} else {
			// Alone, a symbol's own bars count on every date.
			group = []repository.Ticker{{Symbol: stored}}
		}
		for _, ticker := range group {
			owner[ticker.Symbol] = name
		}
		alias[symbol] = name
		units = append(units, checkUnit{name: name, tickers: group})
	}
	return units, alias
}

// unitBars joins the bars of each unit's tickers: the earliest first bar,
// the latest last bar, and every bar day once, oldest first. A unit with no
// bar under any ticker has no entry.
func unitBars(units []checkUnit, bars map[string]*repository.SymbolBars) map[string]*repository.SymbolBars {
	joined := make(map[string]*repository.SymbolBars, len(units))
	for _, unit := range units {
		var out *repository.SymbolBars
		for _, ticker := range unit.tickers {
			stored := bars[ticker.Symbol]
			if stored == nil {
				continue
			}
			if out == nil {
				out = &repository.SymbolBars{First: stored.First, Last: stored.Last}
			}
			if stored.First.Before(out.First) {
				out.First = stored.First
			}
			if stored.Last.After(out.Last) {
				out.Last = stored.Last
			}
			out.Days = append(out.Days, stored.Days...)
		}
		if out != nil {
			slices.SortFunc(out.Days, func(a, b time.Time) int { return a.Compare(b) })
			out.Days = slices.CompactFunc(out.Days, func(a, b time.Time) bool { return a.Equal(b) })
			joined[unit.name] = out
		}
	}
	return joined
}

// closedPeriod is the backtest period as the check reads it: its end capped
// at the last completed session, which every provider's fetch stops at, so
// today's unfinished session is not missing, and the period's sessions up
// to there.
type closedPeriod struct {
	start, end time.Time
	sessions   []time.Time
}

// ziplineFrom is the engine the check and the ingest share.
func ziplineFrom(ctx context.Context, msg stream.Message) (engine.Engine, error) {
	eng, err := msg.Call(ctx, dependency.GetEngineKey)
	if err != nil {
		return nil, fmt.Errorf("error getting zipline engine: %v", err)
	}
	ziplineEngine, isEngine := eng.(engine.Engine)
	if !isEngine {
		return nil, errors.New("error casting zipline engine")
	}
	return ziplineEngine, nil
}

// readClosedPeriod reads the period's sessions up to the last completed one
// before any provider is asked. A period with none fails create here, once,
// so no provider's no-prices sentence follows on the same run: either
// nothing in it has closed yet, or it has no trading day at all.
func readClosedPeriod(ctx context.Context, msg stream.Message, backtests repository.Backtest,
	name, id string, start, end time.Time,
) (closedPeriod, error) {
	last := finance.LastCompletedSessionDate(sessionClock())
	period := closedPeriod{start: start, end: end}
	if last.Before(end) {
		period.end = last
	}
	// Ingest's failure copy, since both calls reach the same Zipline pod.
	ziplineEngine, err := ziplineFrom(ctx, msg)
	if err != nil {
		return period, discardFailedIngest(ctx, backtests, name, id, err)
	}
	if !period.end.Before(start) {
		period.sessions, err = ziplineEngine.Sessions(ctx, start, period.end)
		if err != nil {
			return period, discardFailedIngest(ctx, backtests, name, id, err)
		}
	}
	if len(period.sessions) > 0 {
		return period, nil
	}
	notClosed, err := hasUnclosedSession(ctx, ziplineEngine, start, end, last)
	if err != nil {
		return period, discardFailedIngest(ctx, backtests, name, id, err)
	}
	var lastSession time.Time
	if notClosed {
		// The step names the last session that has closed: the last
		// completed date can be a weekend or holiday.
		recent, err := ziplineEngine.Sessions(ctx, last.AddDate(0, 0, -recentSessionDays), last)
		if err != nil {
			return period, discardFailedIngest(ctx, backtests, name, id, err)
		}
		if len(recent) == 0 {
			return period, discardFailedIngest(ctx, backtests, name, id,
				fmt.Errorf("no XNYS session in the %d days to %s", recentSessionDays, last.Format(time.DateOnly)))
		}
		lastSession = recent[len(recent)-1]
	}
	err = deleteAfterFailure(ctx, backtests, name, id)
	removed := err == nil || errors.Is(err, pgx.ErrNoRows)
	if !removed {
		log.Warn().Err(err).Str("backtest", name).Msg("error deleting backtest after an empty period")
	}
	if notClosed {
		return period, plainSentence(notClosedMessage(name, start, end, lastSession, removed))
	}
	return period, plainSentence(noSessionMessage(name, start, end, removed))
}

// recentSessionDays is how far back from the last completed date the last
// closed session is looked for: longer than any XNYS closure.
const recentSessionDays = 14

// hasUnclosedSession is true when the requested period, before the cap at
// last, holds a real XNYS session after last: one that hasn't closed yet,
// such as today's open session or a future weekday. A period after last
// with no session in it, such as today's weekend, has no trading day
// instead. Zipline's calendar lists sessions about a year ahead, so the
// read stops recentSessionDays after last: any span that long holds a
// session, and the read never goes past the calendar's end.
func hasUnclosedSession(ctx context.Context, ziplineEngine engine.Engine, start, end, last time.Time) (bool, error) {
	from := last.AddDate(0, 0, 1)
	if start.After(from) {
		from = start
	}
	if end.Before(from) {
		return false, nil
	}
	to := from.AddDate(0, 0, recentSessionDays)
	if end.Before(to) {
		to = end
	}
	sessions, err := ziplineEngine.Sessions(ctx, from, to)
	if err != nil {
		return false, err
	}
	return len(sessions) > 0, nil
}

// notClosedMessage is the message when no session of the period has closed
// yet: the period holds a real session after the last completed one.
func notClosedMessage(name string, start, end, lastSession time.Time, removed bool) string {
	saved, next := savedAndNext(name, fmt.Sprintf("Choose a start on or before %s, then run `fbull backtest create` again.",
		lastSession.Format(time.DateOnly)), removed)
	return fmt.Sprintf("Couldn't load data for backtest '%s': %s. %s %s", name,
		finance.NoClosedSessionReason(start, end), saved, next)
}

// noSessionMessage is the message when a period has no trading day: none
// that has closed and none still to close.
func noSessionMessage(name string, start, end time.Time, removed bool) string {
	saved, next := savedAndNext(name, "Choose dates that include a trading day, then run `fbull backtest create` again.", removed)
	return fmt.Sprintf("Couldn't load data for backtest '%s': there's no trading session between %s and %s. %s %s",
		name, start.Format(time.DateOnly), end.Format(time.DateOnly), saved, next)
}

// checkMissingSessions fails create when a symbol has no bar on a session
// of the backtest period, up to the last completed session, so a backtest
// never runs on a price nobody sent (see universe.md §7.5). An edge gap is
// kept when the provider confirms a listing or a delisting; that evidence
// is stored on the backtest. Anything short of that evidence stays the
// missing-session sentence. A failed lookup is its own sentence and is
// never read as listed or delisted. A symbol with no stored bar at all
// still fails. It runs after the download, before Ingest writes the bundle,
// collects every cause, and deletes the backtest that the CREATING step
// stored. readClosedPeriod read the period before the download and failed
// it when it had no session.
func checkMissingSessions(ctx context.Context, msg stream.Message, db *pgxpool.Pool, backtests repository.Backtest,
	name, id, provider, profile string, symbols []string, reported map[string][]time.Time, period closedPeriod,
) error {
	start, end := period.start, period.end
	order := listedOrder(symbols)
	stored := make([]string, 0, len(order))
	for _, symbol := range order {
		stored = append(stored, finance.StoredSymbol(symbol))
	}
	barDays := repository.BarDays{Conn: db}
	tickers, err := barDays.Tickers(ctx, stored)
	if err != nil {
		return discardFailedIngest(ctx, backtests, name, id, fmt.Errorf("error reading tickers: %v", err))
	}
	units, alias := checkUnits(order, tickers)
	read := make([]repository.Ticker, 0, len(units))
	names := make([]string, 0, len(units))
	for _, unit := range units {
		read = append(read, unit.tickers...)
		names = append(names, unit.name)
	}
	bars, err := barDays.List(ctx, provider, read, start, end)
	if err != nil {
		return discardFailedIngest(ctx, backtests, name, id, fmt.Errorf("error reading bar days: %v", err))
	}
	byUnit := make(map[string][]time.Time, len(reported))
	for symbol, days := range reported {
		if unit, ok := alias[symbol]; ok {
			symbol = unit
		}
		byUnit[symbol] = append(byUnit[symbol], days...)
	}
	joined := unitBars(units, bars)
	days, gaps := missingSessions(period.sessions, names, joined, byUnit, true)
	var misses []lookupMiss
	if len(gaps) > 0 {
		eng, err := ziplineFrom(ctx, msg)
		if err != nil {
			return discardFailedIngest(ctx, backtests, name, id, err)
		}
		benchmark := ""
		if len(names) > 0 {
			benchmark = names[0]
		}
		var events []repository.SymbolEvent
		gaps, events, misses, err = excuseEdges(ctx, db, eng, provider, profile, benchmark, period.sessions, joined, gaps)
		if err != nil {
			return discardFailedIngest(ctx, backtests, name, id, err)
		}
		if len(gaps) == 0 && len(misses) == 0 {
			if err := backtests.SaveSymbolEvents(ctx, name, events); err != nil {
				return discardFailedIngest(ctx, backtests, name, id, err)
			}
			return nil
		}
	}
	if len(gaps) == 0 && len(misses) == 0 {
		return nil
	}
	err = deleteAfterFailure(ctx, backtests, name, id)
	removed := err == nil || errors.Is(err, pgx.ErrNoRows)
	if !removed {
		log.Warn().Err(err).Str("backtest", name).Msg("error deleting backtest after missing sessions")
	}
	if len(misses) > 0 && len(gaps) > 0 {
		return plainSentence(mixedCoverageMessage(name, provider, days, gaps, misses, removed))
	}
	if len(misses) > 0 {
		return plainSentence(lookupFailureMessage(name, provider, misses, removed))
	}
	return plainSentence(missingSessionsMessage(name, provider, days, gaps, removed))
}

// ziplineMissingSessions matches the reason of Zipline's MissingSessionsError,
// sent with DATA_LOSS: "missing prices on trading days: AAA 2024-02-14;
// BBB 2024-02-20,2024-03-05".
var ziplineMissingSessions = regexp.MustCompile(
	`^missing prices on trading days: ([A-Za-z0-9.^=_-]{1,32} \d{4}-\d{2}-\d{2}(?:,\d{4}-\d{2}-\d{2})*` +
		`(?:; [A-Za-z0-9.^=_-]{1,32} \d{4}-\d{2}-\d{2}(?:,\d{4}-\d{2}-\d{2})*)*)$`)

// dataLossReason is the reason of a DATA_LOSS ingest error, cut before its
// details. It is false for any other error.
func dataLossReason(cause error) (string, bool) {
	// errors.As, not status.FromError, which puts the wrapping text in the
	// message (readIngestFailure does the same).
	var grpcErr interface{ GRPCStatus() *status.Status }
	if !errors.As(cause, &grpcErr) {
		return "", false
	}
	st := grpcErr.GRPCStatus()
	if st.Code() != codes.DataLoss {
		return "", false
	}
	reason, _, _ := strings.Cut(st.Message(), ingestDetailsMarker)
	return strings.TrimSpace(reason), true
}

// ingestMissingDays reads the gaps from the reason of Zipline's
// MissingSessionsError, keyed by the ticker Zipline names.
func ingestMissingDays(reason string) (map[string][]time.Time, bool) {
	match := ziplineMissingSessions.FindStringSubmatch(reason)
	if match == nil {
		return nil, false
	}
	gaps := make(map[string][]time.Time)
	for _, entry := range strings.Split(match[1], "; ") {
		symbol, list, _ := strings.Cut(entry, " ")
		for _, field := range strings.Split(list, ",") {
			day, err := time.Parse(time.DateOnly, field)
			if err != nil {
				return nil, false
			}
			gaps[symbol] = append(gaps[symbol], day)
		}
	}
	return gaps, true
}

// ingestMissingSessions turns Zipline's refusal of a sid with missing
// sessions into the create's missing-session sentence, so a gap that
// appeared after the download check reads the same as one it caught. It is
// nil when err is another failure, or when the sentence would not be the
// whole list: Zipline named a ticker that isn't in this backtest, or a day
// that isn't a session. The caller then uses the ingest failure copy. Every
// DATA_LOSS outcome is logged with Zipline's cause, redacted.
func ingestMissingSessions(ctx context.Context, eng engine.Engine, db *pgxpool.Pool, backtests repository.Backtest,
	b *backtest.Backtest, id string, cause error,
) error {
	reason, isDataLoss := dataLossReason(cause)
	if !isDataLoss {
		return nil
	}
	outcome, refused := refuseMissingSessions(ctx, eng, db, backtests, b, id, reason)
	log.Warn().Str("backtest", b.Name).Str("outcome", outcome).Str("cause", redactURLPasswords(cause.Error())).
		Msg("zipline refused the ingest with data loss")
	return refused
}

// refuseMissingSessions builds the sentence for ingestMissingSessions, and
// says why it didn't when it is nil.
func refuseMissingSessions(ctx context.Context, eng engine.Engine, db *pgxpool.Pool, backtests repository.Backtest,
	b *backtest.Backtest, id, reason string,
) (string, error) {
	reported, ok := ingestMissingDays(reason)
	if !ok {
		return "not a missing-session refusal", nil
	}
	symbols, err := ingestSymbols(ctx, db, b, id)
	if err != nil {
		return "could not read the backtest's symbols", nil
	}
	start := pb.DateToTime(b.Periods[0].StartDate)
	end := pb.DateToTime(b.Periods[len(b.Periods)-1].EndDate)
	sessions, err := eng.Sessions(ctx, start, end)
	if err != nil {
		return "could not read the calendar", nil
	}
	known := make(map[string]bool, len(symbols))
	for _, symbol := range symbols {
		known[symbol] = true
	}
	isSession := make(map[time.Time]bool, len(sessions))
	for _, session := range sessions {
		isSession[session] = true
	}
	for symbol, days := range reported {
		if !known[symbol] {
			return "named a ticker that isn't in the backtest", nil
		}
		for _, day := range days {
			if !isSession[day] {
				return "named a day that isn't a session", nil
			}
		}
	}
	days, gaps := missingSessions(sessions, listedOrder(symbols), nil, reported, false)
	if len(gaps) == 0 {
		return "named no days", nil
	}
	err = deleteAfterFailure(ctx, backtests, b.Name, id)
	removed := err == nil || errors.Is(err, pgx.ErrNoRows)
	if !removed {
		log.Warn().Err(err).Str("backtest", b.Name).Msg("error deleting backtest after missing sessions")
	}
	provider := strings.ToLower(strings.TrimSpace(b.GetProvider()))
	if provider == "" {
		provider = supplier.DefaultProvider
	}
	return "missing sessions", plainSentence(missingSessionsMessage(b.Name, provider, days, gaps, removed))
}
