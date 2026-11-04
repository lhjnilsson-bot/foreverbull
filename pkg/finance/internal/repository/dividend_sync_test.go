package repository_test

import (
	"context"
	"errors"
	"time"

	"github.com/lhjnilsson/foreverbull/pkg/finance/internal/repository"
	"github.com/lhjnilsson/foreverbull/pkg/finance/supplier"
	pb "github.com/lhjnilsson/foreverbull/pkg/pb/finance"
)

func (test *OHLCTests) koBars() []*pb.OHLC {
	return []*pb.OHLC{
		closeBar(day("2024-03-13"), 61.12), closeBar(day("2024-03-14"), 60.50), closeBar(day("2024-03-15"), 60.40),
	}
}

// Bars and dividends land in one transaction, and the dividends cover the
// whole stored span.
func (test *OHLCTests) TestSyncWithDividendsStoresBarsAndDividends() {
	var asked []string
	err := test.ohlcStorage.SyncWithDividends(context.Background(), "yahoo", "ABC",
		day("2024-03-13"), dayPtr("2024-03-15"),
		func(time.Time, *time.Time) ([]*pb.OHLC, error) { return test.koBars(), nil },
		func(from time.Time, to *time.Time) ([]supplier.Dividend, error) {
			test.Nil(to, "dividends are fetched through today")
			asked = append(asked, from.Format(time.DateOnly))
			return []supplier.Dividend{koDividend()}, nil
		})
	test.Require().NoError(err)
	test.Equal([]string{"2024-03-13"}, asked)
	test.Len(test.storedCloses(), 3)
	test.Equal([]storedDividend{{ExDate: "2024-03-14", PayDate: pay("2024-04-01"), Amount: 0.485}},
		test.storedDividends("ABC"))
	first, last, ok := test.fetchedSpan("ABC")
	test.True(ok)
	test.Equal("2024-03-13", first)
	test.Equal("2024-03-15", last)
}

var errFetchFailed = errors.New("fetch failed")

// A failed dividend fetch fails the symbol and stores no bars.
func (test *OHLCTests) TestSyncWithDividendsFetchFailureStoresNothing() {
	err := test.ohlcStorage.SyncWithDividends(context.Background(), "yahoo", "ABC",
		day("2024-03-13"), dayPtr("2024-03-15"),
		func(time.Time, *time.Time) ([]*pb.OHLC, error) { return test.koBars(), nil },
		func(time.Time, *time.Time) ([]supplier.Dividend, error) { return nil, errFetchFailed })
	test.Require().ErrorIs(err, errFetchFailed)
	test.Empty(test.storedCloses())
	test.Empty(test.storedDividends("ABC"))
}

// A refused dividend rolls the bars back too.
func (test *OHLCTests) TestSyncWithDividendsRefusedDividendStoresNoBars() {
	bad := koDividend()
	bad.Amount, bad.RawAmount = -0.485, -0.485
	err := test.ohlcStorage.SyncWithDividends(context.Background(), "yahoo", "ABC",
		day("2024-03-13"), dayPtr("2024-03-15"),
		func(time.Time, *time.Time) ([]*pb.OHLC, error) { return test.koBars(), nil },
		func(time.Time, *time.Time) ([]supplier.Dividend, error) { return []supplier.Dividend{bad}, nil })
	test.Require().ErrorIs(err, repository.ErrInvalidDividend)
	test.Empty(test.storedCloses())
	test.Empty(test.storedDividends("ABC"))
}

// A sync whose bars come back on another scale (a split) replaces every
// stored bar, and the dividends of the whole span are replaced in the same
// transaction with the amounts fetched with them.
func (test *OHLCTests) TestSyncWithDividendsRescalesDividendsWithBars() {
	ctx := context.Background()
	old := supplier.Dividend{ExDate: day("2020-08-07"), Amount: 0.82, RawAmount: 0.82, SplitFactor: 1, Currency: "USD"}
	test.Require().NoError(test.ohlcStorage.SyncWithDividends(ctx, "yahoo", "ABC",
		day("2020-08-06"), dayPtr("2020-08-07"),
		func(time.Time, *time.Time) ([]*pb.OHLC, error) {
			return []*pb.OHLC{closeBar(day("2020-08-06"), 455.61), closeBar(day("2020-08-07"), 444.45)}, nil
		},
		func(time.Time, *time.Time) ([]supplier.Dividend, error) { return []supplier.Dividend{old}, nil }))

	rescaled := supplier.Dividend{ExDate: day("2020-08-07"), Amount: 0.205, RawAmount: 0.205, SplitFactor: 1, Currency: "USD"}
	var asked []string
	test.Require().NoError(test.ohlcStorage.SyncWithDividends(ctx, "yahoo", "ABC",
		day("2020-08-06"), dayPtr("2020-09-01"),
		func(time.Time, *time.Time) ([]*pb.OHLC, error) {
			return []*pb.OHLC{
				closeBar(day("2020-08-06"), 113.90), closeBar(day("2020-08-07"), 111.11),
				closeBar(day("2020-08-31"), 129.04), closeBar(day("2020-09-01"), 134.18),
			}, nil
		},
		func(from time.Time, _ *time.Time) ([]supplier.Dividend, error) {
			asked = append(asked, from.Format(time.DateOnly))
			return []supplier.Dividend{rescaled}, nil
		}))
	test.Equal([]string{"2020-08-06"}, asked)
	test.InDelta(111.11, test.storedCloses()["2020-08-07"], 1e-9)
	test.Equal([]storedDividend{{ExDate: "2020-08-07", Amount: 0.205}}, test.storedDividends("ABC"))
	first, last, ok := test.fetchedSpan("ABC")
	test.True(ok)
	test.Equal("2020-08-06", first)
	test.Equal("2020-09-01", last)
}

// Asking for cash dividends on stooq fails with the reason, before anything
// is fetched or stored.
func (test *OHLCTests) TestSyncWithDividendsRefusesStooq() {
	for _, provider := range []string{"stooq", " Stooq "} {
		err := test.ohlcStorage.SyncWithDividends(context.Background(), provider, "ABC",
			day("2024-03-13"), dayPtr("2024-03-15"),
			func(time.Time, *time.Time) ([]*pb.OHLC, error) {
				test.Fail("no bars are fetched")
				return test.koBars(), nil
			},
			func(time.Time, *time.Time) ([]supplier.Dividend, error) {
				test.Fail("no dividends are fetched")
				return []supplier.Dividend{koDividend()}, nil
			})
		test.Require().EqualError(err,
			"foreverbull was asked to store cash dividends for stooq, whose prices already include them, "+
				"so they'd be counted twice. Nothing was stored. "+
				"Please report it at https://github.com/lhjnilsson/foreverbull/issues with this message.")
		test.Require().ErrorIs(err, repository.ErrStooqCashDividends)
	}
	test.Empty(test.storedCloses())
	test.Empty(test.storedDividends("ABC"))
	_, _, ok := test.fetchedSpan("ABC")
	test.False(ok)
}

// A refetch that re-scales the bars but brings a dividend that can't be used
// rolls back: the stored bars and dividend stay on their old scale, together.
func (test *OHLCTests) TestSyncWithDividendsRefusedRescaleKeepsBarsAndDividends() {
	ctx := context.Background()
	old := supplier.Dividend{ExDate: day("2020-08-07"), Amount: 0.82, RawAmount: 0.82, SplitFactor: 1, Currency: "USD"}
	test.Require().NoError(test.ohlcStorage.SyncWithDividends(ctx, "yahoo", "ABC",
		day("2020-08-06"), dayPtr("2020-08-07"),
		func(time.Time, *time.Time) ([]*pb.OHLC, error) {
			return []*pb.OHLC{closeBar(day("2020-08-06"), 455.61), closeBar(day("2020-08-07"), 444.45)}, nil
		},
		func(time.Time, *time.Time) ([]supplier.Dividend, error) { return []supplier.Dividend{old}, nil }))

	invalid := supplier.Dividend{ExDate: day("2020-08-07"), Amount: 0.205, RawAmount: 0.205, SplitFactor: 1, Currency: "EUR"}
	err := test.ohlcStorage.SyncWithDividends(ctx, "yahoo", "ABC",
		day("2020-08-06"), dayPtr("2020-09-01"),
		func(time.Time, *time.Time) ([]*pb.OHLC, error) {
			return []*pb.OHLC{
				closeBar(day("2020-08-06"), 113.90), closeBar(day("2020-08-07"), 111.11),
				closeBar(day("2020-08-31"), 129.04), closeBar(day("2020-09-01"), 134.18),
			}, nil
		},
		func(time.Time, *time.Time) ([]supplier.Dividend, error) { return []supplier.Dividend{invalid}, nil })
	test.Require().ErrorIs(err, repository.ErrInvalidDividend)

	closes := test.storedCloses()
	test.Len(closes, 2)
	test.InDelta(455.61, closes["2020-08-06"], 1e-9)
	test.InDelta(444.45, closes["2020-08-07"], 1e-9)
	test.Equal([]storedDividend{{ExDate: "2020-08-07", Amount: 0.82}}, test.storedDividends("ABC"))
	first, last, ok := test.fetchedSpan("ABC")
	test.True(ok)
	test.Equal("2020-08-06", first)
	test.Equal("2020-08-07", last)
}
