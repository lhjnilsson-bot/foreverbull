package api

import (
	"fmt"
	"net/http"
	"strings"

	servicePb "github.com/lhjnilsson/foreverbull/pkg/pb/finance/service"
)

// This file is the one place for the user-facing cause and next-step
// wording of a failed price or dividend download or listing lookup. Backtest create and
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

// DividendFetchKind is why a symbol's dividends failed.
type DividendFetchKind int

const (
	// DividendHTTPStatus is an HTTP error status from the provider, in Status.
	DividendHTTPStatus DividendFetchKind = iota + 1
	// DividendNoAnswer is a provider that didn't answer.
	DividendNoAnswer
	// DividendUnreadable is an answer that couldn't be read.
	DividendUnreadable
	// DividendInvalid is dividends that can't be used, such as an amount
	// of 0 or a currency other than USD. The finance service's own sentence
	// names each one; this cause doesn't.
	DividendInvalid
)

// DividendFetchError is a symbol whose dividends failed, so its prices
// weren't stored either. It crosses DownloadHistoricalData as
// DownloadHistoricalDataResponse.dividend_failure, and DividendFailure reads
// it back, so callers match on the type, not on the text. Neither carries
// the provider's text; that is logged.
type DividendFetchError struct {
	Provider string
	Symbol   string
	Kind     DividendFetchKind
	// Status is the HTTP status of a DividendHTTPStatus failure.
	Status int
}

func (e *DividendFetchError) provider() string {
	return strings.ToLower(strings.TrimSpace(e.Provider))
}

// Cause is the phrase that names what went wrong. It never names the symbol.
// An HTTP status has the same cause as a price download's.
func (e *DividendFetchError) Cause() string {
	provider := e.provider()
	switch e.Kind {
	case DividendHTTPStatus:
		return (&BarsStatusError{Provider: provider, Status: e.Status}).Cause()
	case DividendNoAnswer:
		return provider + " didn't answer"
	case DividendInvalid:
		return provider + " sent dividends that can't be used"
	case DividendUnreadable:
		return provider + "'s answer couldn't be read"
	default:
		return provider + "'s answer couldn't be read"
	}
}

// Step is the next step the cause offers, the same as a price download's
// for an HTTP status.
func (e *DividendFetchError) Step() Step {
	switch e.Kind {
	case DividendHTTPStatus:
		return (&BarsStatusError{Provider: e.Provider, Status: e.Status}).Step()
	case DividendNoAnswer:
		return StepNetwork
	case DividendUnreadable, DividendInvalid:
		return StepParse
	default:
		return StepParse
	}
}

// Error is the DownloadHistoricalData STATUS_ERROR sentence.
func (e *DividendFetchError) Error() string {
	return fmt.Sprintf("foreverbull couldn't download dividends for %s from %s: %s. Its prices weren't saved either. %s",
		e.Symbol, e.provider(), e.Cause(), StepText(e.Step(), DownloadForm))
}

var dividendFailureKinds = map[DividendFetchKind]servicePb.DownloadHistoricalDataResponse_DividendFailure_Kind{
	DividendHTTPStatus: servicePb.DownloadHistoricalDataResponse_DividendFailure_KIND_HTTP_STATUS,
	DividendNoAnswer:   servicePb.DownloadHistoricalDataResponse_DividendFailure_KIND_NO_ANSWER,
	DividendUnreadable: servicePb.DownloadHistoricalDataResponse_DividendFailure_KIND_UNREADABLE,
	DividendInvalid:    servicePb.DownloadHistoricalDataResponse_DividendFailure_KIND_INVALID,
}

// DividendFailureProto is the proto form of e, for a STATUS_ERROR response.
func (e *DividendFetchError) DividendFailureProto() *servicePb.DownloadHistoricalDataResponse_DividendFailure {
	return &servicePb.DownloadHistoricalDataResponse_DividendFailure{
		Provider:   e.Provider,
		Kind:       dividendFailureKinds[e.Kind],
		HttpStatus: int32(e.Status), //nolint:gosec // an HTTP status code fits in int32
	}
}

// DividendFailure is the typed dividend error of a STATUS_ERROR response: a
// *DividendFetchError when dividend_failure is set with a known kind,
// otherwise nil.
func DividendFailure(rsp *servicePb.DownloadHistoricalDataResponse) error {
	if rsp.GetStatus() != servicePb.DownloadHistoricalDataResponse_STATUS_ERROR {
		return nil
	}
	failure := rsp.GetDividendFailure()
	if failure == nil {
		return nil
	}
	for kind, proto := range dividendFailureKinds {
		if proto != failure.GetKind() {
			continue
		}
		status := int(failure.GetHttpStatus())
		if kind == DividendHTTPStatus && (status < 300 || status > 599) {
			return nil
		}
		return &DividendFetchError{Provider: failure.GetProvider(), Symbol: rsp.GetSymbol(), Kind: kind, Status: status}
	}
	return nil
}
