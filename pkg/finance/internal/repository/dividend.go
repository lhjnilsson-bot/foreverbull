package repository

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/lhjnilsson/foreverbull/internal/postgres"
	"github.com/lhjnilsson/foreverbull/pkg/finance/supplier"
)

// DividendTable holds cash dividends next to the bars of yahoo and alpaca.
// Amounts are on the split basis of the stored bars (raw_amount divided by
// split_factor). A backtest credits the cash on the ex-date; pay_date is kept
// for display only, as the provider sent it. A NULL pay_date is a dividend
// whose provider sends none; it is never estimated. dividend_fetch records the span of stored bars whose
// dividends were fetched, so a reader can tell "no dividends" from "never
// fetched". corporate_action holds the actions of the same fetches that a
// backtest can't book (an Alpaca stock dividend, spin-off or merger): a
// backtest whose window holds one fails when its data is loaded, and one
// that ends before it runs. Stooq bars are dividend-adjusted, so stooq has no
// rows in any of the three tables.
const DividendTable = `CREATE TABLE IF NOT EXISTS dividend (
provider text NOT NULL,
symbol text NOT NULL REFERENCES asset(symbol),
ex_date date NOT NULL,
pay_date date,
record_date date,
declared_date date,
amount numeric NOT NULL,
raw_amount numeric NOT NULL,
split_factor numeric NOT NULL,
currency text NOT NULL,
is_foreign boolean NOT NULL,
source_ref text NOT NULL,
fetched_at timestamptz NOT NULL,
CONSTRAINT dividend_provider_symbol_ex_date_key UNIQUE (provider, symbol, ex_date),
CONSTRAINT dividend_provider_check CHECK (provider IN ('yahoo', 'alpaca')),
CONSTRAINT dividend_amount_check CHECK (amount > 0 AND raw_amount > 0 AND split_factor > 0),
CONSTRAINT dividend_currency_check CHECK (currency = 'USD'));

CREATE TABLE IF NOT EXISTS dividend_fetch (
provider text NOT NULL,
symbol text NOT NULL REFERENCES asset(symbol),
first_date date NOT NULL,
last_date date NOT NULL,
fetched_at timestamptz NOT NULL,
PRIMARY KEY (provider, symbol),
CONSTRAINT dividend_fetch_provider_check CHECK (provider IN ('yahoo', 'alpaca')),
CONSTRAINT dividend_fetch_span_check CHECK (first_date <= last_date));

CREATE TABLE IF NOT EXISTS corporate_action (
provider text NOT NULL,
symbol text NOT NULL REFERENCES asset(symbol),
kind text NOT NULL,
ex_date date NOT NULL,
source_ref text NOT NULL,
fetched_at timestamptz NOT NULL,
PRIMARY KEY (provider, symbol, kind, ex_date),
CONSTRAINT corporate_action_provider_check CHECK (provider IN ('yahoo', 'alpaca')),
CONSTRAINT corporate_action_kind_check CHECK (kind IN
	('stock_dividend', 'spin_off', 'cash_merger', 'stock_merger', 'stock_and_cash_merger')));
`

// dividendCurrency is the one currency a dividend is stored in. The stored
// bars and the backtest account are USD, and nothing converts.
const dividendCurrency = "USD"

// ErrInvalidDividend matches a DividendError.
var ErrInvalidDividend = errors.New("invalid dividend")

// DividendProblem is one refused dividend: its ex-date (zero when it has
// none) and why.
type DividendProblem struct {
	ExDate time.Time
	Reason string
}

// DividendError names every refused dividend of one provider and symbol.
// Nothing of the batch is stored.
type DividendError struct {
	Provider string
	Symbol   string
	Problems []DividendProblem
}

func (e *DividendError) Error() string {
	parts := make([]string, 0, len(e.Problems))
	for _, problem := range e.Problems {
		day := "no ex-date"
		if !problem.ExDate.IsZero() {
			day = "ex-date " + problem.ExDate.UTC().Format(time.DateOnly)
		}
		parts = append(parts, fmt.Sprintf("%s: %s", day, problem.Reason))
	}
	return fmt.Sprintf("%s sent dividends for %s that can't be used: %s. Its prices weren't saved either. "+
		"Try another provider, or report it at %s with this message.",
		priceProvider(e.Provider), e.Symbol, strings.Join(parts, "; "), IssuesURL)
}

func (e *DividendError) Unwrap() error { return ErrInvalidDividend }

// unsupportedKinds are the actions a backtest can't book, as stored in
// corporate_action.kind, and what each is called in an error.
var unsupportedKinds = map[string]string{
	"stock_dividend": "stock dividend", "spin_off": "spin-off", "cash_merger": "cash merger",
	"stock_merger": "stock merger", "stock_and_cash_merger": "stock and cash merger",
}

// ValidateDividends checks every dividend before any is stored and names each
// refused one. A dividend needs an ex-date, a positive finite amount, raw
// amount and split factor that agree, and USD (empty is USD). Its pay date is
// not checked: the cash is credited on the ex-date. Two dividends on one
// ex-date are refused: the table keys on it and nothing adds them up. An
// unsupported action needs an ex-date and a known kind.
func ValidateDividends(provider, symbol string, dividends []supplier.Dividend) error {
	var problems []DividendProblem
	seen := make(map[string]bool, len(dividends))
	for _, dividend := range dividends {
		if dividend.Unsupported != "" {
			for _, reason := range actionProblems(dividend) {
				problems = append(problems, DividendProblem{ExDate: dividend.ExDate, Reason: reason})
			}
			continue
		}
		for _, reason := range dividendProblems(dividend) {
			problems = append(problems, DividendProblem{ExDate: dividend.ExDate, Reason: reason})
		}
		if dividend.ExDate.IsZero() {
			continue
		}
		day := tradingDay(dividend.ExDate)
		if seen[day] {
			problems = append(problems, DividendProblem{ExDate: dividend.ExDate, Reason: "two dividends on one ex-date"})
		}
		seen[day] = true
	}
	if len(problems) > 0 {
		return &DividendError{Provider: provider, Symbol: symbol, Problems: problems}
	}
	return nil
}

func actionProblems(action supplier.Dividend) []string {
	var reasons []string
	noun, known := unsupportedKinds[action.Unsupported]
	if !known {
		noun = "corporate action"
	}
	if action.ExDate.IsZero() {
		reasons = append(reasons, "a "+noun+" with no ex-date")
	}
	if !known {
		reasons = append(reasons, fmt.Sprintf("unknown corporate action %q", action.Unsupported))
	}
	return reasons
}

func dividendProblems(dividend supplier.Dividend) []string {
	var reasons []string
	if dividend.ExDate.IsZero() {
		reasons = append(reasons, "missing ex-date")
	}
	if !positivePrice(dividend.Amount) {
		reasons = append(reasons, "amount is 0, negative or not a number")
	}
	if !positivePrice(dividend.RawAmount) || !positivePrice(dividend.SplitFactor) {
		reasons = append(reasons, "raw amount or split factor is 0, negative or not a number")
	} else if math.Abs(dividend.RawAmount/dividend.SplitFactor-dividend.Amount) > 1e-9*dividend.RawAmount {
		reasons = append(reasons, "the amount doesn't match the split-adjusted amount")
	}
	if currency := dividendCurrencyOf(dividend); currency != dividendCurrency {
		reasons = append(reasons, fmt.Sprintf("currency %s is not USD", currency))
	}
	return reasons
}

// dividendCurrencyOf is the dividend's currency in upper case. Empty is USD:
// the providers that leave it out quote US listings.
func dividendCurrencyOf(dividend supplier.Dividend) string {
	currency := strings.ToUpper(strings.TrimSpace(dividend.Currency))
	if currency == "" {
		return dividendCurrency
	}
	return currency
}

// Dividends stores cash dividends. Conn is the Sync transaction when bars and
// dividends must land on one split basis.
type Dividends struct {
	Conn postgres.Query
}

// Replace makes dividends the stored dividends of provider and symbol with an
// ex-date from the day of first through the day of last, and the unsupported
// actions among them its stored corporate actions in that span, and records
// that span as fetched. A dividend outside the span is left out: it has no stored
// bars to sit next to. The batch is validated before the first statement.
// Callers run it inside the transaction that stored the bars, so the two
// never end up on different split bases.
func (db *Dividends) Replace(ctx context.Context, provider, symbol string, first, last time.Time,
	dividends []supplier.Dividend, fetchedAt time.Time,
) error {
	if err := ValidateDividends(provider, symbol, dividends); err != nil {
		return err
	}
	firstDay, lastDay := tradingDay(first), tradingDay(last)
	provider = priceProvider(provider)
	if _, err := db.Conn.Exec(ctx,
		`DELETE FROM dividend WHERE provider = $1 AND symbol = $2 AND ex_date >= $3::date AND ex_date <= $4::date`,
		provider, symbol, firstDay, lastDay); err != nil {
		return fmt.Errorf("failed to replace dividends: %w", err)
	}
	if _, err := db.Conn.Exec(ctx,
		`DELETE FROM corporate_action WHERE provider = $1 AND symbol = $2 AND ex_date >= $3::date AND ex_date <= $4::date`,
		provider, symbol, firstDay, lastDay); err != nil {
		return fmt.Errorf("failed to replace corporate actions: %w", err)
	}
	for _, dividend := range dividends {
		day := tradingDay(dividend.ExDate)
		if day < firstDay || day > lastDay {
			continue
		}
		insert := db.insert
		if dividend.Unsupported != "" {
			insert = db.insertAction
		}
		if err := insert(ctx, provider, symbol, dividend, fetchedAt); err != nil {
			return err
		}
	}
	if _, err := db.Conn.Exec(ctx,
		`INSERT INTO dividend_fetch (provider, symbol, first_date, last_date, fetched_at)
		 VALUES ($1, $2, $3::date, $4::date, $5)
		 ON CONFLICT (provider, symbol) DO UPDATE
		 SET first_date = EXCLUDED.first_date, last_date = EXCLUDED.last_date, fetched_at = EXCLUDED.fetched_at`,
		provider, symbol, firstDay, lastDay, fetchedAt); err != nil {
		return fmt.Errorf("failed to record fetched dividends: %w", err)
	}
	return nil
}

func (db *Dividends) insert(ctx context.Context, provider, symbol string, dividend supplier.Dividend, fetchedAt time.Time) error {
	_, err := db.Conn.Exec(ctx,
		`INSERT INTO dividend (provider, symbol, ex_date, pay_date, record_date, declared_date,
			amount, raw_amount, split_factor, currency, is_foreign, source_ref, fetched_at)
		 VALUES ($1, $2, $3::date, $4::date, $5::date, $6::date, $7, $8, $9, $10, $11, $12, $13)`,
		provider, symbol, tradingDay(dividend.ExDate), optionalDay(dividend.PayDate), optionalDay(dividend.RecordDate),
		optionalDay(dividend.DeclaredDate), dividend.Amount, dividend.RawAmount, dividend.SplitFactor,
		dividendCurrencyOf(dividend), dividend.Foreign, dividend.SourceRef, fetchedAt)
	if err != nil {
		return fmt.Errorf("failed to store dividend: %w", err)
	}
	return nil
}

// insertAction stores an action a backtest can't book. One listed twice is
// stored once.
func (db *Dividends) insertAction(ctx context.Context, provider, symbol string, action supplier.Dividend, fetchedAt time.Time) error {
	_, err := db.Conn.Exec(ctx,
		`INSERT INTO corporate_action (provider, symbol, kind, ex_date, source_ref, fetched_at)
		 VALUES ($1, $2, $3, $4::date, $5, $6)
		 ON CONFLICT (provider, symbol, kind, ex_date) DO NOTHING`,
		provider, symbol, action.Unsupported, tradingDay(action.ExDate), action.SourceRef, fetchedAt)
	if err != nil {
		return fmt.Errorf("failed to store corporate action: %w", err)
	}
	return nil
}

func optionalDay(day *time.Time) *string {
	if day == nil {
		return nil
	}
	value := tradingDay(*day)
	return &value
}
