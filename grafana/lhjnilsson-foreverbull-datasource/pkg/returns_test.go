package main

import (
	"context"
	"encoding/json"

	grafana "github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/data"
	fbPb "github.com/lhjnilsson/foreverbull/pkg/pb"
	backtestpb "github.com/lhjnilsson/foreverbull/pkg/pb/backtest"
	backtestsvc "github.com/lhjnilsson/foreverbull/pkg/pb/backtest/service"
	"github.com/stretchr/testify/mock"
)

const returnMetricsQuery = `{"queryType":"GetExecutionMetric","execution":{"id":"ko"},"metrics":[` +
	`{"name":"total_return"},{"name":"price_return"},{"name":"returns"},{"name":"price_returns"},` +
	`{"name":"benchmark"},{"name":"benchmark_price_return"}]}`

// koRun is the KO golden run's two ex-date closes: equity 6,098.50 and
// 6,352.00 on V_0 = 6,002, with 48.50 and 97.00 credited by then.
func koRun(basis backtestpb.ReturnBasis, foreign int32) *backtestsvc.GetExecutionResponse {
	return &backtestsvc.GetExecutionResponse{
		Execution: &backtestpb.Execution{Id: "ko", Backtest: "ko", ReturnBasis: basis, ForeignDividendsGross: foreign},
		Periods: []*backtestpb.Period{
			{
				Date: &fbPb.Date{Year: 2024, Month: 3, Day: 14}, Returns: -0.00221,
				AlgorithmPeriodReturn: 6098.5/6002 - 1, PriceReturn: 6050.0/6002 - 1,
				BenchmarkPeriodReturn: floatPtr(0.02), BenchmarkPriceReturn: floatPtr(0.01),
			},
			{
				Date: &fbPb.Date{Year: 2024, Month: 6, Day: 14}, Returns: 0.00071,
				AlgorithmPeriodReturn: 6352.0/6002 - 1, PriceReturn: 6255.0/6002 - 1,
				BenchmarkPeriodReturn: floatPtr(0.05), BenchmarkPriceReturn: floatPtr(0.03),
			},
		},
	}
}

func (test *DatasouceTest) queryReturns(run *backtestsvc.GetExecutionResponse) *data.Frame {
	test.backend.On("GetExecution", mock.Anything, mock.Anything).Return(run, nil).Once()
	resp, err := test.uut.QueryData(context.Background(), &grafana.QueryDataRequest{
		Queries: []grafana.DataQuery{{RefID: "A", QueryType: GetExecutionMetric, JSON: json.RawMessage(returnMetricsQuery)}},
	})
	test.Require().NoError(err)
	test.Require().NoError(resp.Responses["A"].Error)
	return resp.Responses["A"].Frames[0]
}

func displayNames(frame *data.Frame) map[string]string {
	names := map[string]string{}
	for _, field := range frame.Fields[1:] {
		names[field.Name] = field.Config.DisplayNameFromDS
	}
	return names
}

func noticeTexts(frame *data.Frame) []string {
	texts := []string{}
	for _, notice := range frame.Meta.Notices {
		texts = append(texts, notice.Text)
	}
	return texts
}

// A total-return run shows its total and price return side by side, the
// benchmark's too, and notes that dividends are paid on the ex-date.
func (test *DatasouceTest) TestTotalReturnRunNamesBothReturns() {
	frame := test.queryReturns(koRun(backtestpb.ReturnBasis_RETURN_BASIS_TOTAL, 1))

	test.Equal(map[string]string{
		"total_return":           "ko ko Total return",
		"price_return":           "ko ko Price return",
		"returns":                "ko ko Daily total return",
		"price_returns":          "ko ko Daily price return",
		"benchmark":              "ko ko Benchmark total return",
		"benchmark_price_return": "ko ko Benchmark price return",
	}, displayNames(frame))
	test.Equal([]string{
		"Dividends paid on the ex-date: the cash lands then, not on the pay date.",
		"1 foreign dividend booked gross, before withholding.",
	}, noticeTexts(frame))

	values := map[string][]float64{}
	for _, field := range frame.Fields[1:] {
		test.Equal("percentunit", field.Config.Unit, field.Name)
		for i := range field.Len() {
			value, _ := field.ConcreteAt(i)
			values[field.Name] = append(values[field.Name], value.(float64))
		}
	}
	test.InDelta(6352.0/6002-1, values["total_return"][1], 1e-12)
	test.InDelta(6255.0/6002-1, values["price_return"][1], 1e-12)
	test.InDelta(6255.0/6050-1, values["price_returns"][1], 1e-12)
	test.InDelta(6050.0/6002-1, values["price_returns"][0], 1e-12)
	test.InDelta(0.03, values["benchmark_price_return"][1], 1e-12)
}

// An adjusted-close (stooq) run has one return, named for what it is, no
// price return, and the adjusted-close note.
func (test *DatasouceTest) TestAdjustedCloseRunHasNoPriceReturn() {
	frame := test.queryReturns(koRun(backtestpb.ReturnBasis_RETURN_BASIS_ADJUSTED_CLOSE, 0))

	test.Equal(map[string]string{
		"total_return": "ko ko Adjusted-close return",
		"returns":      "ko ko Daily adjusted-close return",
		"benchmark":    "ko ko Benchmark adjusted-close return",
	}, displayNames(frame))
	test.Equal([]string{"Adjusted-close return (dividends inside prices): no dividends were booked as cash."},
		noticeTexts(frame))
}

// A run stored before dividends were counted keeps its old names and no note.
func (test *DatasouceTest) TestLegacyRunKeepsItsReturnNames() {
	frame := test.queryReturns(koRun(backtestpb.ReturnBasis_RETURN_BASIS_UNSPECIFIED, 0))

	test.Equal(map[string]string{
		"total_return": "ko ko Return before dividends were counted",
		"returns":      "ko ko returns",
		"benchmark":    "ko ko benchmark",
	}, displayNames(frame))
	test.Empty(frame.Meta.Notices)
}
