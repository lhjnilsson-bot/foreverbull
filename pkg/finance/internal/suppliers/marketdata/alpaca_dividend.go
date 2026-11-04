package marketdata

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lhjnilsson/foreverbull/pkg/finance/supplier"
	"github.com/rs/zerolog/log"
)

var _ supplier.Dividends = (*AlpacaBars)(nil)

// alpacaDataURL is Alpaca's market data host, which serves corporate
// actions. AlpacaBars.opts.BaseURL replaces it in tests.
const alpacaDataURL = "https://data.alpaca.markets"

// alpacaCorporateActionsPageLimit is the page size asked for, Alpaca's
// maximum. alpacaCorporateActionsPages caps the pages one fetch follows; a
// fetch that would need more fails as truncated rather than store part.
const (
	alpacaCorporateActionsPageLimit = 1000
	alpacaCorporateActionsPages     = 50
	alpacaCorporateActionsBodyLimit = 8 << 20
)

// alpacaCorporateActionTypes are asked for together: cash dividends, the
// splits that re-scale them, and every type a run can't book, so a backtest
// whose window holds one fails instead of leaving it out.
var alpacaCorporateActionTypes = []string{
	"cash_dividend", "forward_split", "reverse_split",
	"stock_dividend", "spin_off", "cash_merger", "stock_merger", "stock_and_cash_merger",
}

type alpacaCashDividend struct {
	ID          string   `json:"id"`
	Symbol      string   `json:"symbol"`
	Rate        *float64 `json:"rate"`
	Foreign     bool     `json:"foreign"`
	Special     bool     `json:"special"`
	Currency    string   `json:"currency"`
	ExDate      string   `json:"ex_date"`
	RecordDate  string   `json:"record_date"`
	PayableDate string   `json:"payable_date"`
}

type alpacaSplit struct {
	ID      string  `json:"id"`
	NewRate float64 `json:"new_rate"`
	OldRate float64 `json:"old_rate"`
	ExDate  string  `json:"ex_date"`
}

// alpacaOtherAction is a type a run can't book. Stock dividends and spin-offs
// carry an ex_date, mergers an effective_date.
type alpacaOtherAction struct {
	ID            string `json:"id"`
	ExDate        string `json:"ex_date"`
	EffectiveDate string `json:"effective_date"`
}

type alpacaCorporateActionsPage struct {
	CorporateActions struct {
		CashDividends       []alpacaCashDividend `json:"cash_dividends"`
		ForwardSplits       []alpacaSplit        `json:"forward_splits"`
		ReverseSplits       []alpacaSplit        `json:"reverse_splits"`
		StockDividends      []alpacaOtherAction  `json:"stock_dividends"`
		SpinOffs            []alpacaOtherAction  `json:"spin_offs"`
		CashMergers         []alpacaOtherAction  `json:"cash_mergers"`
		StockMergers        []alpacaOtherAction  `json:"stock_mergers"`
		StockAndCashMergers []alpacaOtherAction  `json:"stock_and_cash_mergers"`
	} `json:"corporate_actions"`
	NextPageToken *string `json:"next_page_token"`
}

// alpacaActions is every page of one fetch put together.
type alpacaActions struct {
	dividends []alpacaCashDividend
	splits    []alpacaSplit
	// other holds each action a run can't book, by Alpaca type.
	other map[string][]alpacaOtherAction
}

// GetDividends reads Alpaca's cash dividends with an ex-date from the day of
// start through the day of end. Alpaca's start and end filter on the
// process date, not the ex-date, so the request asks from start through
// today and the ex-dates are filtered here. Today is also how far the splits
// reach: the stored bars are split-adjusted as of now, so each dividend is
// divided by every split with a later ex-date up to today.
//
// A stock dividend, spin-off or merger dated inside the span comes back
// marked Unsupported: it is stored, and a backtest whose window holds it
// fails. A failed request, an unreadable page, or more pages than
// alpacaCorporateActionsPages is an error.
func (a *AlpacaBars) GetDividends(ctx context.Context, symbol string, start time.Time, end *time.Time,
) ([]supplier.Dividend, error) {
	asked := strings.ToUpper(strings.TrimSpace(symbol))
	ctx, cancel := context.WithTimeout(ctx, alpacaFetchBudget)
	defer cancel()
	today := calendarDate(clockNow(a.now))
	actions, err := a.corporateActions(ctx, asked, calendarDate(start), today)
	if err != nil {
		return nil, err
	}
	last := today
	if end != nil {
		last = calendarDate(*end)
	}
	return alpacaDividends(asked, actions, calendarDate(start), last, today)
}

// corporateActions follows every page of the request for symbol with a
// process date from start through today.
func (a *AlpacaBars) corporateActions(ctx context.Context, symbol string, start, today time.Time) (alpacaActions, error) {
	base := a.opts.BaseURL
	if base == "" {
		base = alpacaDataURL
	}
	query := url.Values{}
	query.Set("symbols", alpacaSymbol(symbol))
	query.Set("types", strings.Join(alpacaCorporateActionTypes, ","))
	query.Set("start", start.Format(time.DateOnly))
	query.Set("end", today.Format(time.DateOnly))
	query.Set("limit", strconv.Itoa(alpacaCorporateActionsPageLimit))
	query.Set("sort", "asc")
	actions := alpacaActions{other: map[string][]alpacaOtherAction{}}
	for page := 0; page < alpacaCorporateActionsPages; page++ {
		next, err := a.corporateActionsPage(ctx, symbol, strings.TrimRight(base, "/")+"/v1/corporate-actions?"+query.Encode())
		if err != nil {
			return alpacaActions{}, err
		}
		found := next.CorporateActions
		actions.dividends = append(actions.dividends, found.CashDividends...)
		actions.splits = append(actions.splits, found.ForwardSplits...)
		actions.splits = append(actions.splits, found.ReverseSplits...)
		actions.other["stock_dividend"] = append(actions.other["stock_dividend"], found.StockDividends...)
		actions.other["spin_off"] = append(actions.other["spin_off"], found.SpinOffs...)
		actions.other["cash_merger"] = append(actions.other["cash_merger"], found.CashMergers...)
		actions.other["stock_merger"] = append(actions.other["stock_merger"], found.StockMergers...)
		actions.other["stock_and_cash_merger"] = append(actions.other["stock_and_cash_merger"], found.StockAndCashMergers...)
		if next.NextPageToken == nil || *next.NextPageToken == "" {
			return actions, nil
		}
		query.Set("page_token", *next.NextPageToken)
	}
	return alpacaActions{}, fmt.Errorf("alpaca corporate actions for %s: more than %d pages, so the list would be truncated",
		symbol, alpacaCorporateActionsPages)
}

func (a *AlpacaBars) corporateActionsPage(ctx context.Context, symbol, endpoint string) (alpacaCorporateActionsPage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return alpacaCorporateActionsPage{}, lookupTransport("alpaca corporate actions", symbol, ctx, err)
	}
	req.Header.Set("APCA-API-KEY-ID", a.opts.APIKey)
	req.Header.Set("APCA-API-SECRET-KEY", a.opts.APISecret)
	resp, err := a.statusClient(ctx).Do(req)
	if err != nil {
		return alpacaCorporateActionsPage{}, lookupTransport("alpaca corporate actions", symbol, ctx, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, alpacaCorporateActionsBodyLimit+1))
	if err != nil {
		return alpacaCorporateActionsPage{}, fmt.Errorf("alpaca corporate actions for %s couldn't be read", symbol)
	}
	if len(body) > alpacaCorporateActionsBodyLimit {
		return alpacaCorporateActionsPage{}, fmt.Errorf("alpaca corporate actions for %s: page is larger than %d bytes",
			symbol, alpacaCorporateActionsBodyLimit)
	}
	if resp.StatusCode != http.StatusOK {
		return alpacaCorporateActionsPage{}, &DividendStatusError{
			Provider: "alpaca", Request: "alpaca corporate actions", Symbol: symbol,
			HTTPCode: resp.StatusCode, HTTPStatus: resp.Status,
		}
	}
	var page alpacaCorporateActionsPage
	if err := json.Unmarshal(body, &page); err != nil {
		return alpacaCorporateActionsPage{}, fmt.Errorf("alpaca corporate actions for %s couldn't be read", symbol)
	}
	return page, nil
}

// alpacaDividends turns the fetched actions into dividends with an ex-date
// from first through last, divided by every split with a later ex-date up to
// today, and the actions a run can't book dated from first through last.
func alpacaDividends(symbol string, actions alpacaActions, first, last, today time.Time) ([]supplier.Dividend, error) {
	unsupported, err := unsupportedActions(symbol, actions.other, first, last)
	if err != nil {
		return nil, err
	}
	splits, err := parseSplits(symbol, actions.splits, today)
	if err != nil {
		return nil, err
	}
	dividends := make([]supplier.Dividend, 0, len(actions.dividends))
	for _, raw := range actions.dividends {
		dividend, err := alpacaDividend(symbol, raw, splits)
		if err != nil {
			return nil, err
		}
		if dividend.ExDate.Before(first) || dividend.ExDate.After(last) {
			continue
		}
		dividends = append(dividends, dividend)
	}
	dividends = append(dividends, unsupported...)
	sort.SliceStable(dividends, func(i, j int) bool { return dividends[i].ExDate.Before(dividends[j].ExDate) })
	return dividends, nil
}

type parsedSplit struct {
	exDate time.Time
	ratio  float64
}

// parseSplits reads each split's ex-date and new over old ratio. A split
// with an ex-date after today is not in the bars yet and is left out.
func parseSplits(symbol string, splits []alpacaSplit, today time.Time) ([]parsedSplit, error) {
	parsed := make([]parsedSplit, 0, len(splits))
	for _, split := range splits {
		exDate, err := alpacaDate(split.ExDate)
		if err != nil {
			return nil, fmt.Errorf("alpaca split %s for %s has no readable ex-date", split.ID, symbol)
		}
		if !positiveRate(split.NewRate) || !positiveRate(split.OldRate) {
			return nil, fmt.Errorf("alpaca split for %s on %s has a rate that is 0, negative or not a number",
				symbol, exDate.Format(time.DateOnly))
		}
		if exDate.After(today) {
			continue
		}
		parsed = append(parsed, parsedSplit{exDate: exDate, ratio: split.NewRate / split.OldRate})
	}
	return parsed, nil
}

// alpacaDividend reads one cash dividend and puts its rate on the bars'
// split basis. Alpaca's rate is the declared amount, before any later split.
func alpacaDividend(symbol string, raw alpacaCashDividend, splits []parsedSplit) (supplier.Dividend, error) {
	exDate, err := alpacaDate(raw.ExDate)
	if err != nil {
		return supplier.Dividend{}, fmt.Errorf("alpaca dividend %s for %s has no readable ex-date", raw.ID, symbol)
	}
	if raw.Rate == nil {
		return supplier.Dividend{}, fmt.Errorf("alpaca dividend for %s on %s has no rate", symbol, exDate.Format(time.DateOnly))
	}
	factor := 1.0
	for _, split := range splits {
		if split.exDate.After(exDate) {
			factor *= split.ratio
		}
	}
	dividend := supplier.Dividend{
		ExDate:      exDate,
		Amount:      *raw.Rate / factor,
		RawAmount:   *raw.Rate,
		SplitFactor: factor,
		Currency:    raw.Currency,
		Foreign:     raw.Foreign,
		SourceRef:   "alpaca:" + raw.ID,
	}
	// The cash is credited on the ex-date, so the payable and record dates
	// are for display only: one that can't be read is stored as unknown
	// (NULL), never estimated.
	dividend.PayDate = displayAlpacaDate(symbol, "payable_date", raw.PayableDate)
	dividend.RecordDate = displayAlpacaDate(symbol, "record_date", raw.RecordDate)
	return dividend, nil
}

// unsupportedActions is every stock dividend, spin-off and merger dated from
// first through last, marked Unsupported with its Alpaca type. One with no
// readable date is an error: nobody could tell which backtests it fails.
func unsupportedActions(symbol string, other map[string][]alpacaOtherAction, first, last time.Time,
) ([]supplier.Dividend, error) {
	kinds := make([]string, 0, len(other))
	for kind := range other {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	var found []supplier.Dividend
	for _, kind := range kinds {
		for _, action := range other[kind] {
			raw := action.ExDate
			if raw == "" {
				raw = action.EffectiveDate
			}
			on, err := alpacaDate(raw)
			if err != nil {
				return nil, fmt.Errorf("alpaca %s for %s has no readable date", kind, symbol)
			}
			if on.Before(first) || on.After(last) {
				continue
			}
			found = append(found, supplier.Dividend{ExDate: on, Unsupported: kind, SourceRef: "alpaca:" + action.ID})
		}
	}
	return found, nil
}

func alpacaDate(raw string) (time.Time, error) {
	parsed, err := time.Parse(time.DateOnly, strings.TrimSpace(raw))
	if err != nil {
		return time.Time{}, fmt.Errorf("alpaca date %q: %w", raw, err)
	}
	return parsed, nil
}

// displayAlpacaDate is a date kept for display: nil when absent or
// unreadable. An unreadable one is logged.
func displayAlpacaDate(symbol, field, raw string) *time.Time {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parsed, err := alpacaDate(raw)
	if err != nil {
		log.Warn().Str("symbol", symbol).Str("field", field).Str("value", raw).
			Msg("alpaca dividend date can't be read; stored as unknown")
		return nil
	}
	return &parsed
}

// positiveRate is true for a real positive split rate.
func positiveRate(rate float64) bool {
	return rate > 0 && !math.IsNaN(rate) && !math.IsInf(rate, 0)
}
