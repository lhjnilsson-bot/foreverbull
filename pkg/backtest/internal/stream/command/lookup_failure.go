package command

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/lhjnilsson/foreverbull/pkg/finance"
	financeapi "github.com/lhjnilsson/foreverbull/pkg/finance/api"
)

// lookupMiss is one symbol whose listing or delisting lookup failed.
// cause is classified. Its Error text is for the log, not this sentence.
type lookupMiss struct {
	symbol  string
	listing bool
	cause   *finance.LookupError
}

// stepKind is one next step. Several failures join their steps in
// financeapi.Steps order. The wording lives in pkg/finance/api, with the
// causes, so one file holds every sentence a failed lookup or download shows.
type stepKind = financeapi.Step

const (
	stepNetwork   = financeapi.StepNetwork
	stepServer    = financeapi.StepServer
	stepRate      = financeapi.StepRate
	stepAlpacaKey = financeapi.StepPaperKey
	stepRefused   = financeapi.StepRefused
	stepParse     = financeapi.StepParse
)

// lookupCause is the phrase after the colon, and which step it offers.
// Only network, a server error, and a rate limit offer a retry.
func lookupCause(provider string, err *finance.LookupError) (string, stepKind) {
	name := strings.ToLower(strings.TrimSpace(provider))
	switch err.Kind {
	case finance.LookupNetwork:
		return name + " didn't answer", stepNetwork
	case finance.LookupBot:
		return name + " answered with a bot check instead of data", stepRefused
	case finance.LookupParse:
		return name + "'s answer couldn't be read", stepParse
	case finance.LookupHTTP:
		switch {
		case err.Status == http.StatusTooManyRequests:
			return financeapi.RateLimitedCause(name), stepRate
		case err.Status >= 500 && err.Status <= 599:
			return financeapi.ServerErrorCause(name, err.Status), stepServer
		case name == "alpaca" && (err.Status == http.StatusUnauthorized || err.Status == http.StatusForbidden):
			// foreverbull checks the key on alpaca's paper trading API, so a
			// live-only key can be refused there. The cause says which API it was.
			return fmt.Sprintf("alpaca's paper trading API didn't accept foreverbull's API key (HTTP %d)", err.Status), stepAlpacaKey
		default:
			return financeapi.RefusedCause(name, err.Status), stepRefused
		}
	default:
		return name + "'s answer couldn't be read", stepParse
	}
}

func stepText(kind stepKind) string {
	return financeapi.StepText(kind, financeapi.CreateForm)
}

// providerAdvice is the step clause that names --provider. A message offers
// it once: later steps drop it.
const providerAdvice = "try another provider with --provider"

// joinSteps lists each distinct step once, in stepKind order, then extra
// when that sentence is not already one of them. Each step is a full
// sentence, and the steps are joined with a single space. After one step has
// offered another provider, later steps drop that advice (withoutProvider);
// a step left with nothing new is dropped.
func joinSteps(kinds []stepKind, extra string) string {
	steps := make([]string, 0, len(kinds)+1)
	offered := false
	add := func(step string) {
		if offered {
			step = withoutProvider(step)
		}
		if step == "" || slicesContains(steps, step) {
			return
		}
		offered = offered || strings.Contains(strings.ToLower(step), providerAdvice)
		steps = append(steps, step)
	}
	for _, kind := range financeapi.Steps {
		if containsKind(kinds, kind) {
			add(stepText(kind))
		}
	}
	if extra != "" {
		add(extra)
	}
	return strings.Join(steps, " ")
}

// withoutProvider drops the --provider advice from a step that a previous
// step already gave:
//
//	"Try another provider with --provider, or X" -> "Or X"
//	"Try another provider with --provider, then run `fbull backtest create` again. Y" -> "Y"
//	"A, or try another provider with --provider, B" -> "A, B"
func withoutProvider(step string) string {
	const lead = "Try another provider with --provider, "
	if rest, ok := strings.CutPrefix(step, lead); ok {
		if alternative, ok := strings.CutPrefix(rest, "or "); ok {
			return "Or " + alternative
		}
		if more, ok := strings.CutPrefix(rest, "then run `fbull backtest create` again."); ok {
			return strings.TrimSpace(more)
		}
	}
	return strings.Replace(step, ", or "+providerAdvice, "", 1)
}

func containsKind(kinds []stepKind, kind stepKind) bool {
	for _, item := range kinds {
		if item == kind {
			return true
		}
	}
	return false
}

func slicesContains(steps []string, extra string) bool {
	for _, step := range steps {
		if step == extra {
			return true
		}
	}
	return false
}

func missKinds(provider string, misses []lookupMiss) []stepKind {
	kinds := make([]stepKind, 0, len(misses))
	for _, miss := range misses {
		_, kind := lookupCause(provider, miss.cause)
		kinds = append(kinds, kind)
	}
	return kinds
}

func distinctMisses(misses []lookupMiss) int {
	seen := make(map[string]bool, len(misses))
	for _, miss := range misses {
		seen[miss.symbol] = true
	}
	return len(seen)
}

func symbolNoun(n int) string {
	if n == 1 {
		return "symbol"
	}
	return "symbols"
}

// lookupCheck is "whether it was delisted" or "when it was listed".
func lookupCheck(listing bool) string {
	if listing {
		return "when it was listed"
	}
	return "whether it was delisted"
}

// lookupFailureMessage is the create error when every failure is a lookup.
// One symbol is one sentence. Two or more are a block. removed selects the
// partly-saved sentence.
func lookupFailureMessage(name, provider string, misses []lookupMiss, removed bool) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	saved, next := savedAndNext(name, joinSteps(missKinds(provider, misses), ""), removed)
	if len(misses) == 1 {
		miss := misses[0]
		cause, _ := lookupCause(provider, miss.cause)
		verb := "whether"
		what := "delisted"
		if miss.listing {
			verb = "when"
			what = "listed"
		}
		return fmt.Sprintf("Couldn't load data for backtest '%s': foreverbull couldn't check with %s %s %s was %s: %s. %s %s",
			name, provider, verb, miss.symbol, what, cause, saved, next)
	}
	n := distinctMisses(misses)
	lines := []string{fmt.Sprintf("Couldn't load data for backtest '%s': foreverbull couldn't check %d %s with %s. %s",
		name, n, symbolNoun(n), provider, saved)}
	for _, miss := range misses {
		cause, _ := lookupCause(provider, miss.cause)
		lines = append(lines, fmt.Sprintf("  %s: couldn't check %s: %s.", miss.symbol, lookupCheck(miss.listing), cause))
	}
	return strings.Join(append(lines, next), "\n")
}

// mixedCoverageMessage is the create error when lookup or download failures
// share the message with missing prices, an unconfirmed edge, or each
// other. The step is the union of all of them.
func mixedCoverageMessage(name, provider string, days []time.Time, gaps []symbolGaps, misses []lookupMiss,
	downloads []downloadMiss, removed bool,
) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	saved, _ := savedAndNext(name, "", removed)
	// savedAndNext's next is rebuilt below so the union, not one step, follows
	// the delete-first sentence when the backtest couldn't be removed.
	// Lookup and download failures can share the block with no gap at all,
	// and then there is no coverage step.
	coverage := ""
	if len(gaps) > 0 {
		coverage = coverageStep(days, gaps)
	}
	next := joinSteps(append(missKinds(provider, misses), downloadKinds(downloads)...), coverage)
	if !removed {
		next = deleteFirst(name, next)
	}
	order, edges, rest := mixedLines(provider, days, gaps, misses, downloads)
	n := len(order)
	verb := "are"
	if n == 1 {
		verb = "is"
	}
	lead := fmt.Sprintf("Couldn't load data for backtest '%s': %d %s %s missing prices or couldn't be checked. %s",
		name, n, symbolNoun(n), verb, saved)
	before, after := false, false
	for _, gap := range gaps {
		before = before || !gap.noneBefore.IsZero()
		after = after || !gap.noneAfter.IsZero()
	}
	lines := append([]string{lead}, edges...)
	if before || after {
		lines = append(lines, unconfirmedReason(provider, before, after))
	}
	lines = append(lines, rest...)
	return strings.Join(append(lines, next), "\n")
}

// mixedLines lists the symbols in the order they are counted, and their
// lines: first the unconfirmed edges, whose reason line follows them, then
// the other gaps, the failed lookups, and the failed downloads.
func mixedLines(provider string, days []time.Time, gaps []symbolGaps, misses []lookupMiss,
	downloads []downloadMiss,
) ([]string, []string, []string) {
	var order []string
	seen := make(map[string]bool)
	add := func(symbol string) {
		if seen[symbol] {
			return
		}
		seen[symbol] = true
		order = append(order, symbol)
	}
	edges := make([]string, 0, len(gaps))
	for _, gap := range gaps {
		add(gap.symbol)
		if gap.atEdge() {
			edges = append(edges, coverageLine(days, gap))
		}
	}
	missBy := make(map[string][]lookupMiss)
	for _, miss := range misses {
		add(miss.symbol)
		missBy[miss.symbol] = append(missBy[miss.symbol], miss)
	}
	downloadBy := make(map[string][]downloadMiss)
	for _, download := range downloads {
		add(download.symbol)
		downloadBy[download.symbol] = append(downloadBy[download.symbol], download)
	}
	gapBy := make(map[string]symbolGaps, len(gaps))
	for _, gap := range gaps {
		if !gap.atEdge() {
			gapBy[gap.symbol] = gap
		}
	}
	rest := make([]string, 0, len(order))
	for _, symbol := range order {
		if gap, ok := gapBy[symbol]; ok {
			rest = append(rest, coverageLine(days, gap))
		}
		for _, miss := range missBy[symbol] {
			cause, _ := lookupCause(provider, miss.cause)
			rest = append(rest, fmt.Sprintf("  %s: couldn't check %s: %s.", miss.symbol, lookupCheck(miss.listing), cause))
		}
		for _, download := range downloadBy[symbol] {
			rest = append(rest, downloadLine(download))
		}
	}
	return order, edges, rest
}
