package repository_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/lhjnilsson/foreverbull/pkg/finance/internal/repository"
	"github.com/lhjnilsson/foreverbull/pkg/finance/supplier"
	"github.com/stretchr/testify/require"
)

func day(value string) time.Time {
	parsed, err := time.Parse(time.DateOnly, value)
	if err != nil {
		panic(err)
	}
	return parsed
}

func dayPtr(value string) *time.Time {
	parsed := day(value)
	return &parsed
}

// koDividend is KO's 2024-03-14 dividend: 0.485 per share, paid 2024-04-01.
func koDividend() supplier.Dividend {
	return supplier.Dividend{
		ExDate: day("2024-03-14"), PayDate: dayPtr("2024-04-01"), RecordDate: dayPtr("2024-03-15"),
		Amount: 0.485, RawAmount: 0.485, SplitFactor: 1, Currency: "USD", SourceRef: "test",
	}
}

func TestValidateDividendsAcceptsAValidBatch(t *testing.T) {
	noPayDate := koDividend()
	noPayDate.ExDate, noPayDate.PayDate, noPayDate.Currency = day("2024-06-14"), nil, ""
	split := supplier.Dividend{ExDate: day("2020-08-07"), Amount: 0.205, RawAmount: 0.82, SplitFactor: 4, Currency: "usd"}
	// The pay date is display only: one before the ex-date is stored as sent.
	earlyPay := koDividend()
	earlyPay.ExDate, earlyPay.PayDate = day("2024-09-13"), dayPtr("2024-09-12")
	require.NoError(t, repository.ValidateDividends("yahoo", "KO", []supplier.Dividend{koDividend(), noPayDate, split, earlyPay}))
}

func TestValidateDividendsNamesEveryRefusedDividend(t *testing.T) {
	missing := koDividend()
	missing.ExDate = time.Time{}
	zero := koDividend()
	zero.ExDate, zero.Amount, zero.RawAmount = day("2024-01-02"), 0, 0
	nan := koDividend()
	nan.ExDate, nan.Amount = day("2024-01-03"), math.NaN()
	euro := koDividend()
	euro.ExDate, euro.Currency = day("2024-01-04"), "EUR"
	unscaled := koDividend()
	unscaled.ExDate, unscaled.RawAmount, unscaled.SplitFactor = day("2024-01-08"), 0.82, 4
	twice := koDividend()

	err := repository.ValidateDividends("alpaca", "KO",
		[]supplier.Dividend{missing, zero, nan, euro, unscaled, koDividend(), twice})
	require.ErrorIs(t, err, repository.ErrInvalidDividend)
	require.EqualError(t, err, "alpaca sent dividends for KO that can't be used: "+
		"no ex-date: missing ex-date; "+
		"ex-date 2024-01-02: amount is 0, negative or not a number; "+
		"ex-date 2024-01-02: raw amount or split factor is 0, negative or not a number; "+
		"ex-date 2024-01-03: amount is 0, negative or not a number; "+
		"ex-date 2024-01-04: currency EUR is not USD; "+
		"ex-date 2024-01-08: the amount doesn't match the split-adjusted amount; "+
		"ex-date 2024-03-14: two dividends on one ex-date. Its prices weren't saved either. "+
		"Try another provider, or report it at https://github.com/lhjnilsson/foreverbull/issues with this message.")
}

type storedDividend struct {
	ExDate  string
	PayDate *string
	Amount  float64
	Foreign bool
}

func (test *OHLCTests) storedDividends(symbol string) []storedDividend {
	rows, err := test.conn.Query(context.Background(),
		`SELECT to_char(ex_date, 'YYYY-MM-DD'), to_char(pay_date, 'YYYY-MM-DD'), amount::float8, is_foreign
		 FROM dividend WHERE symbol = $1 ORDER BY ex_date`, symbol)
	test.Require().NoError(err)
	defer rows.Close()
	var stored []storedDividend
	for rows.Next() {
		var row storedDividend
		test.Require().NoError(rows.Scan(&row.ExDate, &row.PayDate, &row.Amount, &row.Foreign))
		stored = append(stored, row)
	}
	test.Require().NoError(rows.Err())
	return stored
}

func (test *OHLCTests) fetchedSpan(symbol string) (string, string, bool) {
	var first, last string
	err := test.conn.QueryRow(context.Background(),
		`SELECT to_char(first_date, 'YYYY-MM-DD'), to_char(last_date, 'YYYY-MM-DD')
		 FROM dividend_fetch WHERE symbol = $1`, symbol).Scan(&first, &last)
	if err != nil {
		return "", "", false
	}
	return first, last, true
}

func pay(value string) *string { return &value }

// Replace keeps only ex-dates inside the stored span, replaces what was
// there, and records the span so "no dividends" differs from "not fetched".
func (test *OHLCTests) TestReplaceDividendsStoresTheSpanAndRecordsIt() {
	ctx := context.Background()
	dividends := repository.Dividends{Conn: test.conn}
	outside := koDividend()
	outside.ExDate, outside.PayDate = day("2024-07-15"), nil
	foreign := koDividend()
	foreign.ExDate, foreign.PayDate, foreign.Foreign = day("2024-06-14"), nil, true
	test.Require().NoError(dividends.Replace(ctx, "alpaca", "ABC", day("2024-03-01"), day("2024-06-28"),
		[]supplier.Dividend{koDividend(), foreign, outside}, time.Now()))
	test.Equal([]storedDividend{
		{ExDate: "2024-03-14", PayDate: pay("2024-04-01"), Amount: 0.485},
		{ExDate: "2024-06-14", Amount: 0.485, Foreign: true},
	}, test.storedDividends("ABC"))
	first, last, ok := test.fetchedSpan("ABC")
	test.True(ok)
	test.Equal("2024-03-01", first)
	test.Equal("2024-06-28", last)

	test.Require().NoError(dividends.Replace(ctx, "alpaca", "ABC", day("2024-03-01"), day("2024-06-28"),
		[]supplier.Dividend{koDividend()}, time.Now()))
	test.Equal([]storedDividend{{ExDate: "2024-03-14", PayDate: pay("2024-04-01"), Amount: 0.485}},
		test.storedDividends("ABC"))
}

// A refused dividend stores nothing, not even the fetched span.
func (test *OHLCTests) TestReplaceDividendsRefusesAndStoresNothing() {
	bad := koDividend()
	bad.Currency = "SEK"
	dividends := repository.Dividends{Conn: test.conn}
	err := dividends.Replace(context.Background(), "yahoo", "ABC", day("2024-03-01"), day("2024-06-28"),
		[]supplier.Dividend{bad}, time.Now())
	test.Require().ErrorIs(err, repository.ErrInvalidDividend)
	test.Empty(test.storedDividends("ABC"))
	_, _, ok := test.fetchedSpan("ABC")
	test.False(ok)
}

func (test *OHLCTests) storedActions(symbol string) []string {
	rows, err := test.conn.Query(context.Background(),
		`SELECT kind || ' ' || to_char(ex_date, 'YYYY-MM-DD') FROM corporate_action WHERE symbol = $1 ORDER BY ex_date, kind`,
		symbol)
	test.Require().NoError(err)
	defer rows.Close()
	var stored []string
	for rows.Next() {
		var row string
		test.Require().NoError(rows.Scan(&row))
		stored = append(stored, row)
	}
	test.Require().NoError(rows.Err())
	return stored
}

func spinOff(exDate string) supplier.Dividend {
	return supplier.Dividend{ExDate: day(exDate), Unsupported: "spin_off", SourceRef: "alpaca:spin_off"}
}

// An action a backtest can't book is stored next to the dividends, not
// refused at fetch: whether it fails a backtest depends on the backtest's
// window. A refetch replaces it with the dividends.
func (test *OHLCTests) TestReplaceDividendsStoresUnsupportedActions() {
	ctx := context.Background()
	dividends := repository.Dividends{Conn: test.conn}
	test.Require().NoError(dividends.Replace(ctx, "alpaca", "ABC", day("2024-03-01"), day("2024-06-28"),
		[]supplier.Dividend{koDividend(), spinOff("2024-05-06"), spinOff("2024-05-06"), spinOff("2024-07-15")}, time.Now()))
	test.Equal([]string{"spin_off 2024-05-06"}, test.storedActions("ABC"))
	test.Equal([]storedDividend{{ExDate: "2024-03-14", PayDate: pay("2024-04-01"), Amount: 0.485}},
		test.storedDividends("ABC"))

	test.Require().NoError(dividends.Replace(ctx, "alpaca", "ABC", day("2024-03-01"), day("2024-06-28"),
		[]supplier.Dividend{koDividend()}, time.Now()))
	test.Empty(test.storedActions("ABC"))
}

// An action with no ex-date or of a kind nobody knows is refused, and the
// batch stores nothing.
func (test *OHLCTests) TestReplaceDividendsRefusesAnUnreadableAction() {
	undated := spinOff("2024-05-06")
	undated.ExDate = time.Time{}
	unknown := spinOff("2024-05-07")
	unknown.Unsupported = "rights_offering"
	err := (&repository.Dividends{Conn: test.conn}).Replace(context.Background(), "alpaca", "ABC",
		day("2024-03-01"), day("2024-06-28"), []supplier.Dividend{koDividend(), undated, unknown}, time.Now())
	test.Require().EqualError(err, "alpaca sent dividends for ABC that can't be used: no ex-date: a spin-off with no ex-date; "+
		"ex-date 2024-05-07: unknown corporate action \"rights_offering\". Its prices weren't saved either. "+
		"Try another provider, or report it at https://github.com/lhjnilsson/foreverbull/issues with this message.")
	test.Empty(test.storedDividends("ABC"))
	test.Empty(test.storedActions("ABC"))
}
