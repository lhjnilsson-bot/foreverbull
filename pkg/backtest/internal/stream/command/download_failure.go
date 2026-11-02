package command

import (
	"fmt"
	"strings"

	financeapi "github.com/lhjnilsson/foreverbull/pkg/finance/api"
)

// downloadMiss is one symbol whose price download failed because the
// provider answered with an HTTP error status. The finance service sends it
// as a typed provider_status (financeapi.DownloadFailure), so it is matched
// on that type, not on the STATUS_ERROR text. Create collects every one
// before it fails.
type downloadMiss struct {
	symbol string
	cause  *financeapi.BarsStatusError
}

func downloadKinds(downloads []downloadMiss) []stepKind {
	kinds := make([]stepKind, 0, len(downloads))
	for _, download := range downloads {
		kinds = append(kinds, download.cause.Step())
	}
	return kinds
}

// downloadFailureMessage is the create error when every failure is a
// download the provider refused. One symbol is one sentence; two or more
// are a block, one line per symbol. The symbol is in the lead or at the
// start of its line, never in the cause.
func downloadFailureMessage(name, provider string, downloads []downloadMiss, removed bool) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	saved, next := savedAndNext(name, joinSteps(downloadKinds(downloads), ""), removed)
	if len(downloads) == 1 {
		download := downloads[0]
		return fmt.Sprintf("Couldn't load data for backtest '%s': foreverbull couldn't download prices for %s from %s: %s. %s %s",
			name, download.symbol, provider, download.cause.Cause(), saved, next)
	}
	lines := []string{fmt.Sprintf("Couldn't load data for backtest '%s': foreverbull couldn't download prices for %d %s from %s. %s",
		name, len(downloads), symbolNoun(len(downloads)), provider, saved)}
	for _, download := range downloads {
		lines = append(lines, fmt.Sprintf("  %s: %s.", download.symbol, download.cause.Cause()))
	}
	return strings.Join(append(lines, next), "\n")
}

// downloadLine is a failed download's line in a mixed block, where the
// other lines name what couldn't be checked.
func downloadLine(download downloadMiss) string {
	return fmt.Sprintf("  %s: couldn't download prices: %s.", download.symbol, download.cause.Cause())
}

// withoutFailedDownloads drops the symbols whose download failed from the
// stored-bar check: they have no bars because of that failure, which the
// message already names.
func withoutFailedDownloads(symbols []string, downloads []downloadMiss) []string {
	if len(downloads) == 0 {
		return symbols
	}
	failed := make(map[string]bool, len(downloads))
	for _, download := range downloads {
		failed[download.symbol] = true
	}
	kept := make([]string, 0, len(symbols))
	for _, symbol := range symbols {
		if !failed[symbol] {
			kept = append(kept, symbol)
		}
	}
	return kept
}
