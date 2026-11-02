package marketdata

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/rs/zerolog/log"
)

// StatusError is an HTTP status from a listing or delisting lookup.
// Error names the provider, the lookup, and the symbol. It does not include
// the response body or the request URL.
type StatusError struct {
	Lookup string
	Symbol string
	Code   int
	Status string
}

func (e *StatusError) Error() string {
	switch e.Lookup {
	case "search":
		return fmt.Sprintf("yahoo search for %s failed: HTTP %s", e.Symbol, e.Status)
	case "listing date":
		return fmt.Sprintf("yahoo listing date for %s failed: HTTP %s", e.Symbol, e.Status)
	case "asset status":
		return fmt.Sprintf("alpaca asset status for %s failed: HTTP %s", e.Symbol, e.Status)
	default:
		return fmt.Sprintf("%s for %s failed: HTTP %s", e.Lookup, e.Symbol, e.Status)
	}
}

// lookupTransport is a failed request before a status arrived. The returned
// error names the lookup and the symbol. A cancelled call uses ctx's error,
// so the client error's URL stays out of the message. The raw error is logged.
func lookupTransport(lookup, symbol string, ctx context.Context, err error) error {
	log.Warn().Err(err).Str("lookup", lookup).Str("symbol", symbol).Msg("listing lookup request failed")
	cause := err
	if ctx.Err() != nil {
		cause = ctx.Err()
	} else {
		var urlErr *url.Error
		if errors.As(err, &urlErr) && urlErr.Err != nil {
			cause = urlErr.Err
		}
	}
	switch lookup {
	case "search":
		return fmt.Errorf("yahoo search for %s: %w", symbol, cause)
	case "listing date":
		return fmt.Errorf("yahoo listing date for %s: %w", symbol, cause)
	case "asset status":
		return fmt.Errorf("alpaca asset status for %s: %w", symbol, cause)
	default:
		return fmt.Errorf("%s for %s: %w", lookup, symbol, cause)
	}
}

// lookupStatus is a non-200 answer. The body is not part of the error. The
// status is logged without the body or the URL.
func lookupStatus(lookup, symbol string, code int, status string) error {
	log.Warn().Str("lookup", lookup).Str("symbol", symbol).Int("status", code).
		Msg("listing lookup returned a status")
	return &StatusError{Lookup: lookup, Symbol: symbol, Code: code, Status: status}
}
