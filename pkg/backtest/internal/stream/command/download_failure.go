package command

import (
	"fmt"
	"strings"

	financeapi "github.com/lhjnilsson/foreverbull/pkg/finance/api"
)

// downloadMiss is one symbol whose price or dividend download failed. The
// finance service sends the cause typed, as provider_status
// (financeapi.DownloadFailure) for prices and dividend_failure
// (financeapi.DividendFailure) for dividends, so it is matched on that type,
// not on the STATUS_ERROR text. Create collects every one before it fails.
type downloadMiss struct {
	symbol    string
	cause     downloadCause
	dividends bool
}

// downloadCause is a typed download failure: its cause in foreverbull's
// words and the step that fixes it.
type downloadCause interface {
	Cause() string
	Step() financeapi.Step
}

// what is what the download fetched: "prices" or "dividends".
func (download downloadMiss) what() string {
	if download.dividends {
		return "dividends"
	}
	return "prices"
}

// downloadsWhat names what the failed downloads fetched: "prices",
// "dividends", or "prices or dividends" when both failed.
func downloadsWhat(downloads []downloadMiss) string {
	prices, dividends := false, false
	for _, download := range downloads {
		prices = prices || !download.dividends
		dividends = dividends || download.dividends
	}
	switch {
	case prices && dividends:
		return "prices or dividends"
	case dividends:
		return "dividends"
	}
	return "prices"
}

func downloadKinds(downloads []downloadMiss) []stepKind {
	kinds := make([]stepKind, 0, len(downloads))
	for _, download := range downloads {
		kinds = append(kinds, download.cause.Step())
	}
	return kinds
}

// downloadFailureMessage is the create error when every failure is a
// download the provider refused or answered with something unusable. One symbol is one sentence; two or more
// are a block, one line per symbol. The symbol is in the lead or at the
// start of its line, never in the cause.
func downloadFailureMessage(name, provider string, downloads []downloadMiss, removed bool) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	saved, next := savedAndNext(name, joinSteps(downloadKinds(downloads), ""), removed)
	if len(downloads) == 1 {
		download := downloads[0]
		return fmt.Sprintf("Couldn't load data for backtest '%s': foreverbull couldn't download %s for %s from %s: %s. %s %s",
			name, download.what(), download.symbol, provider, download.cause.Cause(), saved, next)
	}
	what := downloadsWhat(downloads)
	lines := []string{fmt.Sprintf("Couldn't load data for backtest '%s': foreverbull couldn't download %s for %d %s from %s. %s",
		name, what, len(downloads), symbolNoun(len(downloads)), provider, saved)}
	for _, download := range downloads {
		if what == "prices or dividends" {
			lines = append(lines, downloadLine(download))
			continue
		}
		lines = append(lines, fmt.Sprintf("  %s: %s.", download.symbol, download.cause.Cause()))
	}
	return strings.Join(append(lines, next), "\n")
}

// downloadLine is a failed download's line in a mixed block, where the
// other lines name what couldn't be checked.
func downloadLine(download downloadMiss) string {
	return fmt.Sprintf("  %s: couldn't download %s: %s.", download.symbol, download.what(), download.cause.Cause())
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
