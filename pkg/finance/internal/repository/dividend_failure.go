package repository

import (
	"context"
	"errors"
	"net"
	"net/url"

	"github.com/lhjnilsson/foreverbull/internal/redact"
	financeapi "github.com/lhjnilsson/foreverbull/pkg/finance/api"
	"github.com/lhjnilsson/foreverbull/pkg/finance/internal/suppliers/marketdata"
	"github.com/rs/zerolog/log"
)

// DividendFetchFailure is the error a failed dividend fetch of provider and
// symbol is shown as: a *financeapi.DividendFetchError naming the cause in
// foreverbull's words. The provider's own text is logged, never shown. A
// blocked provider keeps its own sentence.
func DividendFetchFailure(provider, symbol string, err error) error {
	var blocked *marketdata.BlockedError
	if errors.As(err, &blocked) {
		return err
	}
	provider = priceProvider(provider)
	log.Error().Err(redact.URLError(err)).Str("provider", provider).Str("symbol", symbol).
		Msg("dividend download failed")
	failure := &financeapi.DividendFetchError{Provider: provider, Symbol: symbol, Kind: financeapi.DividendUnreadable}
	var status *marketdata.DividendStatusError
	switch {
	case errors.As(err, &status):
		failure.Kind, failure.Status = financeapi.DividendHTTPStatus, status.HTTPCode
	case noAnswer(err):
		failure.Kind = financeapi.DividendNoAnswer
	}
	return failure
}

// noAnswer reports a request that got no answer: a timeout, a cancel, or a
// connection that failed.
func noAnswer(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var urlErr *url.Error
	var netErr net.Error
	return errors.As(err, &urlErr) || errors.As(err, &netErr) || marketdata.IsProviderTimeout(err)
}
