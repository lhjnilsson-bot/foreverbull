package api

import (
	"fmt"
	"net/http"
	"strings"

	servicePb "github.com/lhjnilsson/foreverbull/pkg/pb/finance/service"
)

// This file is the one place for the user-facing cause and next-step
// wording of a failed price download or listing lookup. Backtest create and
// the DownloadHistoricalData STATUS_ERROR build their sentences from it.

// Form is where a step is shown: the command it says to run again, how it
// offers another provider, and what it calls the dates.
type Form struct {
	again    string
	provider string
	dates    string
}

var (
	// CreateForm is a backtest create message.
	CreateForm = Form{
		again:    "run `fbull backtest create` again",
		provider: "try another provider with --provider",
		dates:    "the backtest dates",
	}
	// DownloadForm is a DownloadHistoricalData STATUS_ERROR, which RPC
	// callers see. It names no CLI flag and no backtest.
	DownloadForm = Form{
		again:    "run the download again",
		provider: "try another provider",
		dates:    "the dates",
	}
)

// issuesURL is where a user reports a failure that a retry can't explain.
const issuesURL = "https://github.com/lhjnilsson/foreverbull/issues"

// RateLimitedCause is a provider that kept answering 429.
func RateLimitedCause(provider string) string {
	return provider + " limited how many requests foreverbull can make"
}

// ServerErrorCause is a provider that answered with a 5xx.
func ServerErrorCause(provider string, status int) string {
	return fmt.Sprintf("%s had a server error (HTTP %d)", provider, status)
}

// RefusedCause is any status without a cause of its own.
func RefusedCause(provider string, status int) string {
	return fmt.Sprintf("%s refused the request (HTTP %d)", provider, status)
}

// RejectedCause is a 422: the provider found the request invalid.
func RejectedCause(provider string, status int) string {
	return fmt.Sprintf("%s rejected the request as invalid (HTTP %d)", provider, status)
}

// KeyRefusedCause is a 401 or a 403 from the provider's market data API.
func KeyRefusedCause(provider string, status int) string {
	return fmt.Sprintf("%s didn't accept foreverbull's API key (HTTP %d)", provider, status)
}

// Step is one next step. Several failures join their steps in Steps order.
type Step int

const (
	StepNetwork Step = iota
	StepServer
	StepRate
	// StepPaperKey is a key refused by alpaca's paper trading API, where the
	// listing and delisting lookups check asset status.
	StepPaperKey
	// StepKey is a key refused by alpaca's market data API, where bars come from.
	StepKey
	StepRefused
	StepRejected
	StepParse
)

// Steps is every step in the order a message lists them.
var Steps = []Step{StepNetwork, StepServer, StepRate, StepPaperKey, StepKey, StepRefused, StepRejected, StepParse}

// StepText is the step as a full sentence in form.
func StepText(step Step, form Form) string {
	again, provider := form.again, form.provider
	switch step {
	case StepNetwork:
		return "Check your internet connection, then " + again + "."
	case StepServer:
		return capitalize(again) + " in a few minutes, or " + provider + "."
	case StepRate:
		return "Wait a few minutes, then " + again + ", or " + provider + "."
	case StepPaperKey:
		return "Check that your alpaca API key and secret work for paper trading, or " + provider + ", then " + again + "."
	case StepKey:
		return "Check your alpaca API key and secret, or " + provider + ", then " + again + "."
	case StepRefused:
		return capitalize(provider) + ", then " + again + "."
	case StepRejected:
		return "Check the symbol and " + form.dates + ", or " + provider + ", then " + again + ". " +
			"If it keeps happening, please report it at " + issuesURL + " with this message."
	case StepParse:
		return capitalize(provider) + ", then " + again + ". " +
			"If it keeps happening, please report it at " + issuesURL + " with this message."
	default:
		return StepText(StepParse, form)
	}
}

func capitalize(text string) string {
	if text == "" {
		return text
	}
	return strings.ToUpper(text[:1]) + text[1:]
}

// BarsStatusError is a provider's HTTP error status for one symbol's bars,
// after the provider's bounded retries. It crosses DownloadHistoricalData as
// DownloadHistoricalDataResponse.provider_status, and DownloadFailure reads
// it back, so callers match on the type, not on the text. Error is the
// STATUS_ERROR sentence. Neither carries the provider's body, the URL, a
// key, or the number of tries; those are logged.
type BarsStatusError struct {
	Provider string
	Symbol   string
	Status   int
}

// Cause is the phrase that names what went wrong. It never names the symbol.
func (e *BarsStatusError) Cause() string {
	provider := strings.ToLower(strings.TrimSpace(e.Provider))
	switch {
	case e.Status == http.StatusUnprocessableEntity:
		return RejectedCause(provider, e.Status)
	case e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden:
		return KeyRefusedCause(provider, e.Status)
	case e.Status == http.StatusTooManyRequests:
		return RateLimitedCause(provider)
	case e.Status >= 500 && e.Status <= 599:
		return ServerErrorCause(provider, e.Status)
	default:
		return RefusedCause(provider, e.Status)
	}
}

// Step is the next step the cause offers. Only a rate limit and a server
// error offer a plain retry.
func (e *BarsStatusError) Step() Step {
	switch {
	case e.Status == http.StatusUnprocessableEntity:
		return StepRejected
	case e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden:
		return StepKey
	case e.Status == http.StatusTooManyRequests:
		return StepRate
	case e.Status >= 500 && e.Status <= 599:
		return StepServer
	default:
		return StepRefused
	}
}

// Error is the DownloadHistoricalData STATUS_ERROR sentence.
func (e *BarsStatusError) Error() string {
	return fmt.Sprintf("foreverbull couldn't download prices for %s from %s: %s. %s",
		e.Symbol, strings.ToLower(strings.TrimSpace(e.Provider)), e.Cause(), StepText(e.Step(), DownloadForm))
}

// ProviderStatus is the proto form of e, for a STATUS_ERROR response.
func (e *BarsStatusError) ProviderStatus() *servicePb.DownloadHistoricalDataResponse_ProviderStatus {
	return &servicePb.DownloadHistoricalDataResponse_ProviderStatus{
		Provider:   e.Provider,
		HttpStatus: int32(e.Status), //nolint:gosec // an HTTP status code fits in int32
	}
}

// DownloadFailure is the typed error of a STATUS_ERROR response: a
// *BarsStatusError when provider_status is set, otherwise nil. The caller
// keeps its own handling for every other STATUS_ERROR.
func DownloadFailure(rsp *servicePb.DownloadHistoricalDataResponse) error {
	if rsp.GetStatus() != servicePb.DownloadHistoricalDataResponse_STATUS_ERROR {
		return nil
	}
	status := rsp.GetProviderStatus()
	if status == nil || status.GetHttpStatus() < 300 || status.GetHttpStatus() > 599 {
		return nil
	}
	return &BarsStatusError{Provider: status.GetProvider(), Symbol: rsp.GetSymbol(), Status: int(status.GetHttpStatus())}
}
