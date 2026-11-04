package supplier

import (
	"context"
	"time"
)

// Dividend is one cash dividend per share, on the same split basis as the
// bars stored for its provider at fetch time. Dates are New York calendar
// dates at 00:00 UTC. A nil PayDate is a dividend whose pay date the
// provider does not send (always on Yahoo); it is never estimated.
type Dividend struct {
	ExDate       time.Time
	PayDate      *time.Time
	RecordDate   *time.Time
	DeclaredDate *time.Time
	// Amount is per share on the stored bars' split basis: RawAmount divided
	// by SplitFactor.
	Amount float64
	// RawAmount is the amount as the provider sent it.
	RawAmount float64
	// SplitFactor is the product of new over old rate of every split with an
	// ex-date after this dividend's, up to fetch time. 1 when the provider
	// already sends split-adjusted amounts (Yahoo).
	SplitFactor float64
	// Currency is the ISO code. Only USD is stored.
	Currency string
	// Foreign is a dividend of a foreign issuer. It is booked gross, before
	// any withholding tax.
	Foreign bool
	// SourceRef names where the row came from, such as the provider's id.
	SourceRef string
	// Unsupported is set on an action a backtest can't book instead of a
	// cash dividend: an Alpaca stock_dividend, spin_off, cash_merger,
	// stock_merger or stock_and_cash_merger. Such a row has an ExDate and a
	// SourceRef only. It is stored so that a backtest whose window holds it
	// fails, while one that ends before it still runs.
	Unsupported string
}

// Dividends is a price source whose bars are not dividend-adjusted, so it
// sends cash dividends next to them: yahoo and alpaca. Stooq's bars already
// include dividends and it does not implement this.
//
// GetDividends returns every cash dividend with an ex-date from the day of
// start through the day of end (nil end: through today), and every action a
// backtest can't book in that span, marked Unsupported. An empty slice with
// a nil error is an answer that had no dividends; any failed or truncated
// request is an error.
type Dividends interface {
	GetDividends(ctx context.Context, symbol string, start time.Time, end *time.Time) ([]Dividend, error)
}
