package main

import (
	"fmt"

	"github.com/grafana/grafana-plugin-sdk-go/data"
	backtestpb "github.com/lhjnilsson/foreverbull/pkg/pb/backtest"
)

// Notes under a run's returns, the same words as the CLI summary.
const (
	exDateNote        = "Dividends paid on the ex-date: the cash lands then, not on the pay date."
	adjustedCloseNote = "Adjusted-close return (dividends inside prices): no dividends were booked as cash."
)

// returnMetric is true for the metrics whose name says what the return
// counts.
func returnMetric(name QueryMetric) bool {
	return returnMetrics[name]
}

var returnMetrics = map[QueryMetric]bool{
	Returns: true, Benchmark: true, TotalReturn: true, PriceReturn: true, PriceReturns: true, BenchmarkPriceReturn: true,
}

// returnDisplayName is what a return metric is called for a run of basis. A
// price return exists only for a total-return run: for the others it is ""
// and the field is left out. A run stored before dividends were counted
// keeps the metric's own name.
func returnDisplayName(name QueryMetric, basis backtestpb.ReturnBasis) string {
	switch basis {
	case backtestpb.ReturnBasis_RETURN_BASIS_TOTAL:
		return map[QueryMetric]string{
			Returns:              "Daily total return",
			PriceReturns:         "Daily price return",
			TotalReturn:          "Total return",
			PriceReturn:          "Price return",
			Benchmark:            "Benchmark total return",
			BenchmarkPriceReturn: "Benchmark price return",
		}[name]
	case backtestpb.ReturnBasis_RETURN_BASIS_ADJUSTED_CLOSE:
		return map[QueryMetric]string{
			Returns:     "Daily adjusted-close return",
			TotalReturn: "Adjusted-close return",
			Benchmark:   "Benchmark adjusted-close return",
		}[name]
	case backtestpb.ReturnBasis_RETURN_BASIS_UNSPECIFIED:
		return legacyReturnName(name)
	default:
		return legacyReturnName(name)
	}
}

// legacyReturnName is a return metric's name for a run stored before
// dividends were counted.
func legacyReturnName(name QueryMetric) string {
	return map[QueryMetric]string{
		Returns:     string(Returns),
		TotalReturn: "Return before dividends were counted",
		Benchmark:   string(Benchmark),
	}[name]
}

// appendReturnField adds a return metric under the name that says what it
// counts. A price return of a run that has none is left out.
func appendReturnField(frame *data.Frame, name QueryMetric, basis backtestpb.ReturnBasis, labels data.Labels, values interface{}) {
	display := returnDisplayName(name, basis)
	if display == "" {
		return
	}
	field := data.NewField(string(name), labels, values)
	cfg := &data.FieldConfig{Unit: metricUnit(string(name)), DisplayNameFromDS: display}
	if label := labels["execution"]; label != "" {
		cfg.DisplayNameFromDS = label + " " + display
	}
	field.SetConfig(cfg)
	frame.Fields = append(frame.Fields, field)
}

// dailyPriceReturn is period i's price return: E_i / E_{i-1} - 1, read from
// the cumulative price return (E_i = V_0 * (1 + price_return_i)).
func dailyPriceReturn(periods []*backtestpb.Period, i int) float64 {
	if i == 0 {
		return periods[0].GetPriceReturn()
	}
	return (1+periods[i].GetPriceReturn())/(1+periods[i-1].GetPriceReturn()) - 1
}

// returnNotices are the notes on how a run's returns count dividends.
func returnNotices(execution *backtestpb.Execution) []data.Notice {
	var notes []string
	switch execution.GetReturnBasis() {
	case backtestpb.ReturnBasis_RETURN_BASIS_TOTAL:
		notes = append(notes, exDateNote)
		switch foreign := execution.GetForeignDividendsGross(); {
		case foreign == 1:
			notes = append(notes, "1 foreign dividend booked gross, before withholding.")
		case foreign > 1:
			notes = append(notes, fmt.Sprintf("%d foreign dividends booked gross, before withholding.", foreign))
		}
	case backtestpb.ReturnBasis_RETURN_BASIS_ADJUSTED_CLOSE:
		notes = append(notes, adjustedCloseNote)
	case backtestpb.ReturnBasis_RETURN_BASIS_UNSPECIFIED:
	}
	notices := make([]data.Notice, 0, len(notes))
	for _, note := range notes {
		notices = append(notices, data.Notice{Severity: data.NoticeSeverityInfo, Text: note})
	}
	return notices
}
