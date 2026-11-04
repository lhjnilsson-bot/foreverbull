package marketdata

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/lhjnilsson/foreverbull/pkg/finance/supplier"
)

var _ supplier.Dividends = (*YahooClient)(nil)

// yahooDividendBodyLimit bounds one chart answer read for dividends. A
// chart with every daily bar since 1962 is about 2 MiB.
const yahooDividendBodyLimit = 16 << 20

type yahooDividendResponse struct {
	Chart struct {
		Result []struct {
			Meta struct {
				Currency string `json:"currency"`
			} `json:"meta"`
			Events struct {
				Dividends map[string]struct {
					Amount *float64 `json:"amount"`
					Date   *int64   `json:"date"`
				} `json:"dividends"`
			} `json:"events"`
		} `json:"result"`
		Error *struct {
			Code        string `json:"code"`
			Description string `json:"description"`
		} `json:"error"`
	} `json:"chart"`
}

// DividendStatusError is a dividend request the provider answered with an
// HTTP error status. Callers match on the type to name the cause; Error is
// for the log.
type DividendStatusError struct {
	Provider string
	// Request names the request in the log, such as "yahoo dividends".
	Request string
	Symbol  string
	// HTTPCode is the status code, such as 503.
	HTTPCode int
	// HTTPStatus is the status line, such as "503 Service Unavailable".
	HTTPStatus string
}

func (e *DividendStatusError) Error() string {
	return fmt.Sprintf("%s for %s failed: HTTP %s", e.Request, e.Symbol, e.HTTPStatus)
}

// GetDividends reads the chart's dividend events (events=div), the same
// unofficial endpoint and session as the bars. Yahoo's amounts are already
// on the split-adjusted basis of its close, so SplitFactor is 1. Yahoo sends
// only the ex-date, stamped at the 09:30 New York open, and no pay, record,
// or declared date. Any failed request, chart error ("Not Found" included:
// the bars were found under this ticker), or unreadable answer is an error,
// never "no dividends".
func (y *YahooClient) GetDividends(ctx context.Context, symbol string, start time.Time, end *time.Time,
) ([]supplier.Dividend, error) {
	asked := strings.ToUpper(symbol)
	chartURL := y.chartURL
	if chartURL == "" {
		chartURL = defaultYahooChartURL
	}
	until := clockNow(y.now)
	if end != nil {
		until = inclusiveFetchEnd(*end)
	}
	resp, err := y.doRequest(ctx, chartURL+url.PathEscape(yahooSymbol(symbol)),
		fmt.Sprintf("period1=%d", start.Unix()), fmt.Sprintf("period2=%d", until.Unix()),
		"interval=1d", "events=div")
	if err != nil {
		return nil, fmt.Errorf("yahoo dividends for %s: %w", asked, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, yahooDividendBodyLimit+1))
	if err != nil {
		return nil, fmt.Errorf("yahoo dividends for %s couldn't be read: %w", asked, err)
	}
	if len(body) > yahooDividendBodyLimit {
		return nil, fmt.Errorf("yahoo dividends for %s: answer is larger than %d bytes", asked, yahooDividendBodyLimit)
	}
	if err := blockedError("yahoo", asked, resp.Header.Get("Content-Type"), body); err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &DividendStatusError{
			Provider: "yahoo", Request: "yahoo dividends", Symbol: asked,
			HTTPCode: resp.StatusCode, HTTPStatus: resp.Status,
		}
	}
	return yahooDividends(asked, body, start, end)
}

// yahooDividends turns the chart's dividend events into dividends with an
// ex-date from the day of start through the day of end, oldest first. An
// event without an amount or a date is an error naming it.
func yahooDividends(symbol string, body []byte, start time.Time, end *time.Time) ([]supplier.Dividend, error) {
	var data yahooDividendResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("yahoo dividends for %s couldn't be read: %w", symbol, err)
	}
	if data.Chart.Error != nil && data.Chart.Error.Code != "" {
		return nil, fmt.Errorf("yahoo dividends for %s failed: %s", symbol, data.Chart.Error.Description)
	}
	if len(data.Chart.Result) == 0 {
		return nil, fmt.Errorf("yahoo dividends for %s failed: no result", symbol)
	}
	result := data.Chart.Result[0]
	first := calendarDate(start)
	dividends := make([]supplier.Dividend, 0, len(result.Events.Dividends))
	for key, event := range result.Events.Dividends {
		if event.Amount == nil || event.Date == nil {
			return nil, fmt.Errorf("yahoo dividends for %s: event %s has no amount or date", symbol, key)
		}
		exDate := newYorkDate(time.Unix(*event.Date, 0))
		if exDate.Before(first) || (end != nil && exDate.After(calendarDate(*end))) {
			continue
		}
		dividends = append(dividends, supplier.Dividend{
			ExDate:      exDate,
			Amount:      *event.Amount,
			RawAmount:   *event.Amount,
			SplitFactor: 1,
			Currency:    result.Meta.Currency,
			SourceRef:   "yahoo:chart:" + key,
		})
	}
	sort.Slice(dividends, func(i, j int) bool { return dividends[i].ExDate.Before(dividends[j].ExDate) })
	return dividends, nil
}

// newYorkDate is the New York calendar date of t at 00:00 UTC, the form
// dividend dates are stored in.
func newYorkDate(t time.Time) time.Time {
	year, month, day := t.In(xnysZone).Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

// calendarDate is the UTC calendar date of a requested start or end, the
// form the callers pass dates in.
func calendarDate(t time.Time) time.Time {
	year, month, day := t.UTC().Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}
