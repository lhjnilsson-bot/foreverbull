package api_test

import (
	"context"
	"time"

	"github.com/lhjnilsson/foreverbull/pkg/finance/supplier"
)

// dividendFree is a yahoo or alpaca price source whose answers have no
// dividends, for tests of bar storage that don't look at dividends. A real
// yahoo or alpaca source must fetch them, so a bare mock is refused.
type dividendFree struct {
	supplier.Marketdata
}

func (dividendFree) GetDividends(context.Context, string, time.Time, *time.Time) ([]supplier.Dividend, error) {
	return []supplier.Dividend{}, nil
}
