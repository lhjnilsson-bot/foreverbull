package finance

import (
	"context"
	"errors"
	"net"
	"net/url"
	"regexp"
	"strconv"

	"github.com/lhjnilsson/foreverbull/pkg/finance/internal/suppliers/marketdata"
)

// AlpacaPaperHost is the trading host an Alpaca asset-status lookup uses.
// A live-only key can be refused there. That note is logged, not shown.
const AlpacaPaperHost = marketdata.AlpacaPaperHost

// LookupKind classifies a failed listing or delisting lookup. The user
// sentence is built from these fields. Err is for the log.
type LookupKind int

const (
	// LookupNetwork is a timeout, a refused connection, or a cancelled call
	// while the caller's context is still live.
	LookupNetwork LookupKind = iota
	// LookupHTTP is an HTTP status. Status is the code.
	LookupHTTP
	// LookupBot is a bot-check page instead of data.
	LookupBot
	// LookupParse is a body that could not be read as the lookup's answer.
	LookupParse
)

// LookupError is a failed listing or delisting lookup. Error is the provider
// text for the log. Callers that speak to the user use Kind, Status, and
// Provider, not Error.
type LookupError struct {
	Kind     LookupKind
	Provider string
	Symbol   string
	Status   int
	Err      error
}

func (e *LookupError) Error() string {
	if e == nil || e.Err == nil {
		return "listing lookup failed"
	}
	return e.Err.Error()
}

func (e *LookupError) Unwrap() error { return e.Err }

// httpStatusText is a status code named in a provider error, such as GetOHLC's
// "HTTP 503 Service Unavailable" from Yahoo or Stooq. The typed StatusError
// and AlpacaBarsError win when they are present.
var httpStatusText = regexp.MustCompile(`\bHTTP (\d{3})\b`)

// asLookup classifies a provider error. The raw error stays on LookupError
// for the log and is not a sentence for the user. When ctx is done the
// caller cancelled or ran out of time, so ctx.Err() is returned unchanged
// and is never classified as a network failure.
func asLookup(ctx context.Context, provider, symbol string, err error) error {
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	var already *LookupError
	if errors.As(err, &already) {
		return already
	}
	kind := LookupParse
	status := 0
	var blocked *marketdata.BlockedError
	var httpErr *marketdata.StatusError
	var barsErr *marketdata.AlpacaBarsError
	switch {
	case errors.As(err, &blocked):
		kind = LookupBot
	case errors.As(err, &httpErr):
		kind = LookupHTTP
		status = httpErr.Code
	case errors.As(err, &barsErr):
		kind = LookupHTTP
		status = barsErr.HTTPCode
	case lookupNetwork(err):
		kind = LookupNetwork
	default:
		if code, ok := httpStatusOf(err); ok {
			kind = LookupHTTP
			status = code
		}
	}
	return &LookupError{Kind: kind, Provider: provider, Symbol: symbol, Status: status, Err: err}
}

func lookupNetwork(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var urlErr *url.Error
	return errors.As(err, &urlErr)
}

func httpStatusOf(err error) (int, bool) {
	match := httpStatusText.FindStringSubmatch(err.Error())
	if match == nil {
		return 0, false
	}
	code, convErr := strconv.Atoi(match[1])
	if convErr != nil {
		return 0, false
	}
	return code, true
}
