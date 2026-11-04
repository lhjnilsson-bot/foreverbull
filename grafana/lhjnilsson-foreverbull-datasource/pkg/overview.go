package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	grafana "github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/data"
	fbPb "github.com/lhjnilsson/foreverbull/pkg/pb"
	backtestpb "github.com/lhjnilsson/foreverbull/pkg/pb/backtest"
	backtestsvc "github.com/lhjnilsson/foreverbull/pkg/pb/backtest/service"
)

// executionLinkURL is filled per row from the execution UUID and backtest name.
// An empty name leaves var-backtest blank. The execution dashboard treats that
// as absent, so a link with only var-execution still opens the run.
const executionLinkURL = "/d/ee9ep4jxtwkcgf?var-execution=${__data.fields.execution}&var-backtest=${__data.fields.backtest}"

type overviewQuery struct {
	Backtest string `json:"backtest"`
}

type overviewRow struct {
	executionID            string
	backtest               string
	start                  string
	end                    string
	shortID                string
	algorithmPeriodReturn  *float64
	returnBasis            string
	priceReturn            *float64
	maxDrawdown            *float64
	sharpe                 *float64
	sortino                *float64
	startingPortfolioValue *float64
	portfolioValue         *float64
	benchmark              string
	activeDays             string
}

func (d *Datasource) HandleListExecutionOverview(ctx context.Context, req *grafana.QueryDataRequest) (*grafana.QueryDataResponse, error) {
	return processQueries(ctx, req, d.handleListExecutionOverview), nil
}

func (d *Datasource) handleListExecutionOverview(ctx context.Context, _ grafana.QueryDataRequest, q grafana.DataQuery) grafana.DataResponse {
	if d == nil || d.backend == nil || d.log == nil {
		return grafana.ErrDataResponse(grafana.StatusBadRequest, "broker client is not configured")
	}
	log := d.log.With("ListExecutionOverview")
	from, to := q.TimeRange.From, q.TimeRange.To
	if from.IsZero() || to.IsZero() {
		return grafana.ErrDataResponse(grafana.StatusBadRequest, "time range is required")
	}

	var model overviewQuery
	if len(q.JSON) > 0 {
		if err := json.Unmarshal(q.JSON, &model); err != nil {
			log.Error("fail to parse json", err)
			return grafana.ErrDataResponse(grafana.StatusBadRequest, fmt.Sprintf("json unmarshal: %v", err.Error()))
		}
	}
	backtestName := overviewBacktestName(model.Backtest)

	rsp, err := d.backend.ListExecutions(ctx, &backtestsvc.ListExecutionsRequest{Backtest: backtestName})
	if err != nil {
		log.Error("fail to list executions", err)
		return grafana.ErrDataResponse(grafana.StatusBadRequest, fmt.Sprintf("list executions: %v", err.Error()))
	}

	rows := make([]overviewRow, 0)
	benchmarks := map[string]string{}
	for _, execution := range rsp.GetExecutions() {
		if execution == nil || execution.GetId() == "" {
			continue
		}
		if backtestName != "" && execution.GetBacktest() != backtestName {
			continue
		}
		// Overlap is the execution's start_date..end_date, not period timestamps.
		if !executionOverlaps(execution.GetStartDate(), execution.GetEndDate(), from, to) {
			continue
		}
		got, err := d.backend.GetExecution(ctx, &backtestsvc.GetExecutionRequest{ExecutionId: execution.GetId()})
		if err != nil {
			log.Error("fail to get execution", err)
			return grafana.ErrDataResponse(grafana.StatusBadRequest, fmt.Sprintf("get execution: %v", err.Error()))
		}
		ticker := d.overviewBenchmark(ctx, execution.GetBacktest(), benchmarks)
		row := overviewRowFrom(execution, got.GetPeriods(), ticker)
		withReturnBasis(&row, got.GetExecution().GetReturnBasis(), lastStoredPeriod(got.GetPeriods()))
		rows = append(rows, row)
	}
	sortOverviewRows(rows)

	frame := overviewFrame(q.RefID, rows)
	return grafana.DataResponse{
		Frames: []*data.Frame{frame},
		Status: grafana.StatusOK,
	}
}

// overviewBacktestName is empty when the dashboard variable means every backtest.
// "*" is the All value (a truthy allValue). "$__all" and an uninterpolated
// "$backtest" are the same choice.
func overviewBacktestName(raw string) string {
	name := strings.TrimSpace(raw)
	switch name {
	case "", "*", "$__all", "$backtest":
		return ""
	default:
		return name
	}
}

// executionOverlaps reports whether inclusive UTC calendar dates
// start_date..end_date overlap the picker [from, to].
// An execution that ends on the picker's from date is included.
// One that ends the day before is not. Times of day do not shrink the date.
func executionOverlaps(start, end *fbPb.Date, from, to time.Time) bool {
	startDay, okStart := utcProtoDate(start)
	endDay, okEnd := utcProtoDate(end)
	if !okStart || !okEnd || from.IsZero() || to.IsZero() {
		return false
	}
	if endDay.Before(startDay) {
		startDay, endDay = endDay, startDay
	}
	fromDay := utcDay(from)
	toDay := utcDay(to)
	if toDay.Before(fromDay) {
		fromDay, toDay = toDay, fromDay
	}
	return !startDay.After(toDay) && !endDay.Before(fromDay)
}

func utcDay(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

func utcProtoDate(d *fbPb.Date) (time.Time, bool) {
	if d == nil || d.GetYear() == 0 {
		return time.Time{}, false
	}
	return time.Date(int(d.GetYear()), time.Month(d.GetMonth()), int(d.GetDay()), 0, 0, 0, 0, time.UTC), true
}

// lastStoredPeriod is the period with the latest date.
// A nil Sharpe on that period stays nil; earlier periods are not borrowed.
// ListExecutions Result is not used: that join keeps one global period.
func lastStoredPeriod(periods []*backtestpb.Period) *backtestpb.Period {
	var best *backtestpb.Period
	var bestAt time.Time
	bestIndex := -1
	for i, period := range periods {
		if period == nil {
			continue
		}
		at := periodTime(period)
		if best == nil || at.After(bestAt) || (at.Equal(bestAt) && i > bestIndex) {
			best = period
			bestAt = at
			bestIndex = i
		}
	}
	return best
}

// firstStoredPeriod is the period with the earliest date.
// An equal date keeps the earlier slice entry. A later session is not the open.
func firstStoredPeriod(periods []*backtestpb.Period) *backtestpb.Period {
	var best *backtestpb.Period
	var bestAt time.Time
	for _, period := range periods {
		if period == nil {
			continue
		}
		at := periodTime(period)
		if best == nil || at.Before(bestAt) {
			best = period
			bestAt = at
		}
	}
	return best
}

// startingPortfolioValue is cash plus positions value at the session open.
// Zipline's starting_value is positions value, not the account. The earliest
// session's pair is the capital the run opened with.
func startingPortfolioValue(period *backtestpb.Period) float64 {
	return period.GetStartingCash() + period.GetStartingValue()
}

// activityLabel is "N of M": sessions that closed with a long or a short,
// of the stored sessions. Zipline fills a daily order on the next bar, so a
// fill shows up in those counts. Nil periods are not sessions.
func activityLabel(periods []*backtestpb.Period) string {
	total := 0
	active := 0
	for _, period := range periods {
		if period == nil {
			continue
		}
		total++
		if period.GetLongsCount() != 0 || period.GetShortsCount() != 0 {
			active++
		}
	}
	if total == 0 {
		return ""
	}
	return fmt.Sprintf("%d of %d", active, total)
}

// overviewBenchmark is the backtest's benchmark ticker. A failed lookup is
// an unset benchmark, so the cell stays "not set" and the board still loads.
func (d *Datasource) overviewBenchmark(ctx context.Context, name string, cache map[string]string) string {
	if name == "" {
		return ""
	}
	if ticker, ok := cache[name]; ok {
		return ticker
	}
	ticker := ""
	rsp, err := d.backend.GetBacktest(ctx, &backtestsvc.GetBacktestRequest{Name: name})
	if err == nil && rsp != nil && rsp.GetBacktest() != nil {
		ticker = rsp.GetBacktest().GetBenchmark()
	}
	cache[name] = ticker
	return ticker
}

// benchmarkCell matches the CLI summary. An empty ticker is "not set".
// A ticker with no stored benchmark_period_return is "not available".
// A stored fraction prints as a signed percent, the same as Return's source.
func benchmarkCell(ticker string, period *backtestpb.Period) string {
	if strings.TrimSpace(ticker) == "" {
		return "not set"
	}
	if period == nil || period.BenchmarkPeriodReturn == nil {
		return "not available"
	}
	return fmt.Sprintf("%+.1f%%", period.GetBenchmarkPeriodReturn()*100)
}

func overviewRowFrom(execution *backtestpb.Execution, periods []*backtestpb.Period, benchmarkTicker string) overviewRow {
	row := overviewRow{
		executionID: execution.GetId(),
		backtest:    execution.GetBacktest(),
		start:       formatDate(execution.GetStartDate()),
		end:         formatDate(execution.GetEndDate()),
		shortID:     shortExecutionID(execution.GetId()),
	}
	// Return, drawdown, ratios, and ending value stay on the last stored period.
	// A nil Sharpe or Sortino is not replaced with 0 or with an earlier day.
	last := lastStoredPeriod(periods)
	if last != nil {
		row.algorithmPeriodReturn = newFloat(last.GetAlgorithmPeriodReturn())
		row.maxDrawdown = newFloat(last.GetMaxDrawdown())
		row.portfolioValue = newFloat(last.GetPortfolioValue())
		row.sharpe = cloneFloat(last.Sharpe)
		row.sortino = cloneFloat(last.Sortino)
	}
	row.benchmark = benchmarkCell(benchmarkTicker, last)
	if first := firstStoredPeriod(periods); first != nil {
		row.startingPortfolioValue = newFloat(startingPortfolioValue(first))
	}
	row.activeDays = activityLabel(periods)
	return row
}

// Return basis labels. They name what the Total return column measures for
// each row: the CLI's return label ("Total return", "Adjusted-close return",
// "Return") with its note on dividends.
const (
	totalReturnLabel         = "Total return (dividends paid on the ex-date)"
	adjustedCloseReturnLabel = "Adjusted-close return (dividends inside prices)"
	beforeDividendsLabel     = "Return before dividends were counted"
)

// withReturnBasis says what a row's Return measures. Only a total-return
// run has a price return; the others leave it empty rather than repeating
// Return under another name.
func withReturnBasis(row *overviewRow, basis backtestpb.ReturnBasis, last *backtestpb.Period) {
	switch basis {
	case backtestpb.ReturnBasis_RETURN_BASIS_TOTAL:
		row.returnBasis = totalReturnLabel
		if last != nil {
			row.priceReturn = newFloat(last.GetPriceReturn())
		}
	case backtestpb.ReturnBasis_RETURN_BASIS_ADJUSTED_CLOSE:
		row.returnBasis = adjustedCloseReturnLabel
	case backtestpb.ReturnBasis_RETURN_BASIS_UNSPECIFIED:
		row.returnBasis = beforeDividendsLabel
	default:
		row.returnBasis = beforeDividendsLabel
	}
}

func newFloat(v float64) *float64 {
	copied := v
	return &copied
}

func cloneFloat(v *float64) *float64 {
	if v == nil {
		return nil
	}
	return newFloat(*v)
}

func sortOverviewRows(rows []overviewRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		left, right := rows[i], rows[j]
		switch {
		case left.sharpe == nil && right.sharpe == nil:
			return overviewLess(left, right)
		case left.sharpe == nil:
			return false
		case right.sharpe == nil:
			return true
		case *left.sharpe != *right.sharpe:
			return *left.sharpe > *right.sharpe
		default:
			return overviewLess(left, right)
		}
	})
}

func overviewLess(left, right overviewRow) bool {
	if left.backtest != right.backtest {
		return left.backtest < right.backtest
	}
	return left.executionID < right.executionID
}

func overviewFrame(refID string, rows []overviewRow) *data.Frame {
	n := len(rows)
	backtests := make([]string, n)
	starts := make([]string, n)
	ends := make([]string, n)
	ids := make([]string, n)
	executions := make([]string, n)
	returns := make([]*float64, n)
	returnBases := make([]string, n)
	priceReturns := make([]*float64, n)
	drawdowns := make([]*float64, n)
	sharpes := make([]*float64, n)
	sortinos := make([]*float64, n)
	startingValues := make([]*float64, n)
	values := make([]*float64, n)
	benchmarkCells := make([]string, n)
	activeDays := make([]string, n)
	for i, row := range rows {
		backtests[i] = row.backtest
		starts[i] = row.start
		ends[i] = row.end
		ids[i] = row.shortID
		executions[i] = row.executionID
		returns[i] = row.algorithmPeriodReturn
		returnBases[i] = row.returnBasis
		priceReturns[i] = row.priceReturn
		drawdowns[i] = row.maxDrawdown
		sharpes[i] = row.sharpe
		sortinos[i] = row.sortino
		startingValues[i] = row.startingPortfolioValue
		values[i] = row.portfolioValue
		benchmarkCells[i] = row.benchmark
		activeDays[i] = row.activeDays
	}

	// Dates keep Start and End. Portfolio columns are Start value and End
	// value so the two pairs do not share a header.
	frame := data.NewFrame("Executions",
		overviewTextField("backtest", "Backtest", backtests, true),
		overviewTextField("start", "Start", starts, true),
		overviewTextField("end", "End", ends, true),
		overviewTextField("id", "ID", ids, true),
		overviewNumberField("starting_portfolio_value", "Start value", "currencyUSD", startingValues),
		overviewNumberField("portfolio_value", "End value", "currencyUSD", values),
		overviewNumberField("algorithm_period_return", "Total return", "percentunit", returns),
		overviewTextField("return_basis", "Return basis", returnBases, false),
		overviewNumberField("price_return", "Price return", "percentunit", priceReturns),
		overviewTextField("benchmark", "Benchmark", benchmarkCells, false),
		overviewNumberField("sharpe", "Sharpe", "none", sharpes),
		overviewNumberField("sortino", "Sortino", "none", sortinos),
		overviewNumberField("max_drawdown", "Max drawdown", "percentunit", drawdowns),
		overviewTextField("active_days", "Active days", activeDays, false),
		overviewHiddenText("execution", "Execution", executions),
	)
	frame.RefID = refID
	frame.Meta = &data.FrameMeta{Type: data.FrameTypeTable}
	return frame
}

func overviewLinks() []data.DataLink {
	return []data.DataLink{{
		Title: "Open execution",
		URL:   executionLinkURL,
	}}
}

func overviewTextField(name, display string, values []string, link bool) *data.Field {
	field := data.NewField(name, nil, values)
	cfg := &data.FieldConfig{DisplayNameFromDS: display}
	if link {
		cfg.Links = overviewLinks()
	}
	field.SetConfig(cfg)
	return field
}

func overviewHiddenText(name, display string, values []string) *data.Field {
	field := data.NewField(name, nil, values)
	field.SetConfig(&data.FieldConfig{
		DisplayNameFromDS: display,
		Custom:            map[string]interface{}{"hidden": true},
	})
	return field
}

func overviewNumberField(name, display, unit string, values []*float64) *data.Field {
	field := data.NewField(name, nil, values)
	field.SetConfig(&data.FieldConfig{
		DisplayNameFromDS: display,
		Unit:              unit,
	})
	return field
}
