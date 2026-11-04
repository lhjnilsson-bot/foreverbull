package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	grafana "github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/data"
	fbPb "github.com/lhjnilsson/foreverbull/pkg/pb"
	backtestpb "github.com/lhjnilsson/foreverbull/pkg/pb/backtest"
	backtestsvc "github.com/lhjnilsson/foreverbull/pkg/pb/backtest/service"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func protoYMD(year, month, day int32) *fbPb.Date {
	return &fbPb.Date{Year: year, Month: month, Day: day}
}

func TestExecutionOverlapsPicker(t *testing.T) {
	from := time.Date(2024, 1, 1, 15, 4, 5, 0, time.UTC)
	to := time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)

	if executionOverlaps(protoYMD(2020, 1, 1), protoYMD(2023, 12, 31), from, to) {
		t.Fatal("execution ending before the picker from must be excluded")
	}
	if !executionOverlaps(protoYMD(2023, 6, 1), protoYMD(2024, 6, 1), from, to) {
		t.Fatal("execution overlapping the picker must be included")
	}
	if !executionOverlaps(protoYMD(2024, 1, 1), protoYMD(2024, 1, 1), from, to) {
		t.Fatal("execution that ends on the picker from date must be included")
	}
	if !executionOverlaps(protoYMD(2026, 1, 1), protoYMD(2026, 6, 1), from, to) {
		t.Fatal("execution that starts on the picker to date must be included")
	}
	if executionOverlaps(protoYMD(2026, 1, 2), protoYMD(2026, 6, 1), from, to) {
		t.Fatal("execution that starts after the picker to date must be excluded")
	}
	if !executionOverlaps(protoYMD(2024, 3, 1), protoYMD(2024, 9, 1), from, to) {
		t.Fatal("execution inside the picker must be included")
	}
	if !executionOverlaps(protoYMD(2020, 1, 1), protoYMD(2027, 1, 1), from, to) {
		t.Fatal("execution covering the picker must be included")
	}
	if executionOverlaps(nil, protoYMD(2024, 6, 1), from, to) {
		t.Fatal("missing start_date must be excluded")
	}

	// Picker instants are UTC dates. 00:30 CET on 1 Jan is still 31 Dec UTC.
	cetFrom := time.Date(2024, 1, 1, 0, 30, 0, 0, time.FixedZone("CET", 3600))
	if !executionOverlaps(protoYMD(2023, 12, 31), protoYMD(2023, 12, 31), cetFrom, to) {
		t.Fatal("overlap uses the UTC calendar date of the picker instant")
	}
	if executionOverlaps(protoYMD(2023, 12, 30), protoYMD(2023, 12, 30), cetFrom, to) {
		t.Fatal("execution ending before the UTC picker from must be excluded")
	}
}

func TestOverviewBacktestName(t *testing.T) {
	cases := map[string]string{
		"":           "",
		"  ":         "",
		"*":          "",
		"$__all":     "",
		"$backtest":  "",
		"spy":        "spy",
		" eu-small ": "eu-small",
		"All":        "All",
	}
	for in, want := range cases {
		if got := overviewBacktestName(in); got != want {
			t.Fatalf("overviewBacktestName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLastStoredPeriodUsesLatestDate(t *testing.T) {
	earlierSharpe := 3.0
	periods := []*backtestpb.Period{
		{
			Date:                  protoYMD(2024, 6, 1),
			AlgorithmPeriodReturn: 0.12,
			MaxDrawdown:           -0.08,
			PortfolioValue:        1120,
		},
		{
			Date:                  protoYMD(2024, 1, 1),
			AlgorithmPeriodReturn: 0.5,
			PortfolioValue:        10,
			Sharpe:                &earlierSharpe,
		},
	}
	last := lastStoredPeriod(periods)
	if last == nil || last.GetAlgorithmPeriodReturn() != 0.12 {
		t.Fatalf("last period return = %v", last.GetAlgorithmPeriodReturn())
	}
	if last.Sharpe != nil {
		t.Fatal("nil sharpe on the last stored period must stay nil")
	}
	if last.GetPortfolioValue() != 1120 {
		t.Fatalf("portfolio = %v", last.GetPortfolioValue())
	}
}

func TestOverviewRowUsesFirstOpenAndLastClose(t *testing.T) {
	earlierSharpe := 9.0
	// Later session is first in the slice. Start still comes from the earliest date.
	later := &backtestpb.Period{
		Date:                  protoYMD(2024, 6, 1),
		StartingCash:          200,
		StartingValue:         800,
		EndingValue:           900,
		PortfolioValue:        1120,
		AlgorithmPeriodReturn: 0.12,
		MaxDrawdown:           -0.08,
		LongsCount:            2,
	}
	earlier := &backtestpb.Period{
		Date:                  protoYMD(2024, 1, 1),
		StartingCash:          1000,
		StartingValue:         50,
		PortfolioValue:        1000,
		AlgorithmPeriodReturn: 0.5,
		Sharpe:                &earlierSharpe,
	}
	row := overviewRowFrom(&backtestpb.Execution{Id: "abc", Backtest: "spy"}, []*backtestpb.Period{later, nil, earlier}, "SPY")
	if row.startingPortfolioValue == nil || *row.startingPortfolioValue != 1050 {
		t.Fatalf("start = %v", row.startingPortfolioValue)
	}
	if row.portfolioValue == nil || *row.portfolioValue != 1120 {
		t.Fatalf("end = %v, want last portfolio_value 1120", row.portfolioValue)
	}
	if row.algorithmPeriodReturn == nil || *row.algorithmPeriodReturn != 0.12 {
		t.Fatalf("return = %v", row.algorithmPeriodReturn)
	}
	if row.maxDrawdown == nil || *row.maxDrawdown != -0.08 {
		t.Fatalf("drawdown = %v", row.maxDrawdown)
	}
	if row.sharpe != nil {
		t.Fatal("nil sharpe on the last stored period must stay nil")
	}
	if row.sortino != nil {
		t.Fatal("nil sortino on the last stored period must stay nil")
	}
	if row.activeDays != "1 of 2" {
		t.Fatalf("activity = %q", row.activeDays)
	}
	if row.benchmark != "not available" {
		t.Fatalf("benchmark = %q", row.benchmark)
	}
}

func TestOverviewRowTradelessKeepsZeroReturnAndAbsentRatios(t *testing.T) {
	flat := &backtestpb.Period{
		Date:                  protoYMD(2024, 1, 2),
		StartingCash:          100000,
		StartingValue:         0,
		PortfolioValue:        100000,
		AlgorithmPeriodReturn: 0,
		MaxDrawdown:           0,
	}
	row := overviewRowFrom(&backtestpb.Execution{Id: "flat"}, []*backtestpb.Period{flat, flat}, "")
	if row.sharpe != nil || row.sortino != nil {
		t.Fatal("tradeless sharpe and sortino stay absent")
	}
	if row.algorithmPeriodReturn == nil || *row.algorithmPeriodReturn != 0 {
		t.Fatalf("return = %v", row.algorithmPeriodReturn)
	}
	if row.maxDrawdown == nil || *row.maxDrawdown != 0 {
		t.Fatalf("drawdown = %v", row.maxDrawdown)
	}
	if row.startingPortfolioValue == nil || *row.startingPortfolioValue != 100000 {
		t.Fatalf("start = %v", row.startingPortfolioValue)
	}
	if row.portfolioValue == nil || *row.portfolioValue != 100000 {
		t.Fatalf("end = %v", row.portfolioValue)
	}
	if row.activeDays != "0 of 2" {
		t.Fatalf("activity = %q", row.activeDays)
	}
	if row.benchmark != "not set" {
		t.Fatalf("benchmark = %q", row.benchmark)
	}
}

func TestBenchmarkCell(t *testing.T) {
	if benchmarkCell("", nil) != "not set" {
		t.Fatalf("empty ticker = %q", benchmarkCell("", nil))
	}
	if benchmarkCell("SPY", &backtestpb.Period{}) != "not available" {
		t.Fatal("missing return must stay not available")
	}
	ret := 0.031
	if got := benchmarkCell("SPY", &backtestpb.Period{BenchmarkPeriodReturn: &ret}); got != "+3.1%" {
		t.Fatalf("benchmark = %q", got)
	}
	zero := 0.0
	if got := benchmarkCell("SPY", &backtestpb.Period{BenchmarkPeriodReturn: &zero}); got != "+0.0%" {
		t.Fatalf("zero benchmark = %q", got)
	}
}

func TestSortOverviewRowsRanksBySharpe(t *testing.T) {
	low := 0.2
	high := 1.4
	rows := []overviewRow{
		{backtest: "b", executionID: "2", sharpe: &low},
		{backtest: "a", executionID: "1"},
		{backtest: "c", executionID: "3", sharpe: &high},
	}
	sortOverviewRows(rows)
	if rows[0].backtest != "c" || rows[1].backtest != "b" || rows[2].backtest != "a" {
		t.Fatalf("order = %s %s %s", rows[0].backtest, rows[1].backtest, rows[2].backtest)
	}
}

func (test *DatasouceTest) TestOverviewExcludesExecutionEndingBeforePicker() {
	const (
		excludedID = "aaaaaaaa-1111-1111-1111-111111111111"
		includedID = "22222222-2222-2222-2222-222222222222"
	)
	from := time.Date(2024, 1, 1, 15, 4, 5, 0, time.UTC)
	to := time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)

	test.backend.On("ListExecutions", mock.Anything, mock.MatchedBy(func(req *backtestsvc.ListExecutionsRequest) bool {
		return req.GetBacktest() == ""
	})).Return(&backtestsvc.ListExecutionsResponse{
		Executions: []*backtestpb.Execution{
			{
				Id:        excludedID,
				Backtest:  "old-spy",
				StartDate: protoYMD(2020, 1, 1),
				EndDate:   protoYMD(2023, 12, 31),
				Result: &backtestpb.Period{
					AlgorithmPeriodReturn: 9,
					MaxDrawdown:           -9,
					PortfolioValue:        1,
					Sharpe:                floatPtr(9),
					Sortino:               floatPtr(9),
				},
			},
			{
				Id:        includedID,
				Backtest:  "eu-small",
				StartDate: protoYMD(2023, 6, 1),
				EndDate:   protoYMD(2024, 6, 1),
				Result: &backtestpb.Period{
					AlgorithmPeriodReturn: 9,
					MaxDrawdown:           -9,
					PortfolioValue:        1,
					Sharpe:                floatPtr(9),
					Sortino:               floatPtr(9),
				},
			},
		},
	}, nil).Once()

	test.backend.On("GetExecution", mock.Anything, mock.MatchedBy(func(req *backtestsvc.GetExecutionRequest) bool {
		return req.GetExecutionId() == includedID
	})).Return(&backtestsvc.GetExecutionResponse{
		Periods: []*backtestpb.Period{
			{
				Date:                  protoYMD(2023, 6, 1),
				AlgorithmPeriodReturn: 0.01,
				MaxDrawdown:           -0.02,
				PortfolioValue:        100,
				StartingCash:          1000,
				StartingValue:         50,
				Sharpe:                floatPtr(0.2),
				Sortino:               floatPtr(0.3),
			},
			{
				Date:                  protoYMD(2024, 6, 1),
				AlgorithmPeriodReturn: 0.12,
				MaxDrawdown:           -0.08,
				PortfolioValue:        1120,
				EndingValue:           900,
				StartingCash:          200,
				StartingValue:         800,
				Sharpe:                floatPtr(1.4),
				Sortino:               floatPtr(1.8),
				BenchmarkPeriodReturn: floatPtr(0.031),
				LongsCount:            3,
			},
		},
	}, nil).Once()
	test.backend.On("GetBacktest", mock.Anything, mock.MatchedBy(func(req *backtestsvc.GetBacktestRequest) bool {
		return req.GetName() == "eu-small"
	})).Return(&backtestsvc.GetBacktestResponse{
		Backtest: &backtestpb.Backtest{Benchmark: "SPY"},
	}, nil).Once()

	resp, err := test.uut.QueryData(context.Background(), &grafana.QueryDataRequest{
		Queries: []grafana.DataQuery{
			{
				RefID:     "A",
				QueryType: ListExecutionOverview,
				TimeRange: grafana.TimeRange{From: from, To: to},
				JSON:      json.RawMessage(`{"queryType":"ListExecutionOverview","backtest":"*"}`),
			},
		},
	})
	test.Require().NoError(err)
	test.Require().Equal(grafana.StatusOK, resp.Responses["A"].Status)
	test.Require().Len(resp.Responses["A"].Frames, 1)
	frame := resp.Responses["A"].Frames[0]
	test.Equal(data.FrameTypeTable, frame.Meta.Type)
	test.Require().NotEmpty(frame.Fields)
	test.Equal(1, frame.Fields[0].Len(), "execution overlapping the picker must be included; a run ending before the picker from is excluded")

	for _, field := range frame.Fields {
		test.NotEqual("time", field.Name)
		test.NotEqual("positions", field.Name)
		for i := 0; i < field.Len(); i++ {
			text, ok := field.At(i).(string)
			if !ok {
				continue
			}
			test.NotContains(text, excludedID, "execution ending before the picker from must be excluded")
			test.NotContains(text, "old-spy", "execution ending before the picker from must be excluded")
		}
	}

	test.Equal("eu-small", test.overviewString(frame, "backtest", 0))
	test.Equal("2023-06-01", test.overviewString(frame, "start", 0))
	test.Equal("2024-06-01", test.overviewString(frame, "end", 0))
	test.Equal("22222222", test.overviewString(frame, "id", 0))
	test.Equal(includedID, test.overviewString(frame, "execution", 0), "row link uses the full execution id")
	test.InDelta(0.12, test.overviewFloat(frame, "algorithm_period_return", 0), 1e-9)
	test.InDelta(-0.08, test.overviewFloat(frame, "max_drawdown", 0), 1e-9)
	test.InDelta(1.4, test.overviewFloat(frame, "sharpe", 0), 1e-9)
	test.InDelta(1.8, test.overviewFloat(frame, "sortino", 0), 1e-9)
	test.InDelta(1120, test.overviewFloat(frame, "portfolio_value", 0), 1e-9)
	test.InDelta(1050, test.overviewFloat(frame, "starting_portfolio_value", 0), 1e-9)
	test.Equal("1 of 2", test.overviewString(frame, "active_days", 0))
	test.Equal("+3.1%", test.overviewString(frame, "benchmark", 0))

	test.Equal("percentunit", overviewUnit(frame, "algorithm_period_return"))
	test.Equal("percentunit", overviewUnit(frame, "max_drawdown"))
	test.Equal("none", overviewUnit(frame, "sharpe"))
	test.Equal("none", overviewUnit(frame, "sortino"))
	test.Equal("currencyUSD", overviewUnit(frame, "portfolio_value"))
	test.Equal("currencyUSD", overviewUnit(frame, "starting_portfolio_value"))
	test.Equal("Start", overviewField(frame, "start").Config.DisplayNameFromDS)
	test.Equal("End", overviewField(frame, "end").Config.DisplayNameFromDS)
	test.Equal("Start value", overviewField(frame, "starting_portfolio_value").Config.DisplayNameFromDS)
	test.Equal("End value", overviewField(frame, "portfolio_value").Config.DisplayNameFromDS)
	test.Equal("Total return", overviewField(frame, "algorithm_period_return").Config.DisplayNameFromDS)
	test.Equal("Return basis", overviewField(frame, "return_basis").Config.DisplayNameFromDS)
	test.Equal("Price return", overviewField(frame, "price_return").Config.DisplayNameFromDS)
	test.Equal("percentunit", overviewUnit(frame, "price_return"))
	test.Equal("Benchmark", overviewField(frame, "benchmark").Config.DisplayNameFromDS)
	test.Equal("Sharpe", overviewField(frame, "sharpe").Config.DisplayNameFromDS)
	test.Equal("Sortino", overviewField(frame, "sortino").Config.DisplayNameFromDS)
	test.Equal("Max drawdown", overviewField(frame, "max_drawdown").Config.DisplayNameFromDS)
	test.Equal("Active days", overviewField(frame, "active_days").Config.DisplayNameFromDS)
	test.Equal(executionLinkURL, overviewField(frame, "backtest").Config.Links[0].URL)
	test.Contains(executionLinkURL, "/d/ee9ep4jxtwkcgf?var-execution=")
	test.Contains(executionLinkURL, "var-backtest=")
	test.NotContains(executionLinkURL, "executionId")
}

func (test *DatasouceTest) TestOverviewFiltersByBacktestName() {
	const spyID = "33333333-3333-3333-3333-333333333333"
	from := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	test.backend.On("ListExecutions", mock.Anything, mock.MatchedBy(func(req *backtestsvc.ListExecutionsRequest) bool {
		return req.GetBacktest() == "spy"
	})).Return(&backtestsvc.ListExecutionsResponse{
		Executions: []*backtestpb.Execution{
			{
				Id:        spyID,
				Backtest:  "spy",
				StartDate: protoYMD(2024, 2, 1),
				EndDate:   protoYMD(2024, 8, 1),
			},
			{
				Id:        "44444444-4444-4444-4444-444444444444",
				Backtest:  "eu-small",
				StartDate: protoYMD(2024, 2, 1),
				EndDate:   protoYMD(2024, 8, 1),
			},
		},
	}, nil).Once()
	test.backend.On("GetExecution", mock.Anything, mock.MatchedBy(func(req *backtestsvc.GetExecutionRequest) bool {
		return req.GetExecutionId() == spyID
	})).Return(&backtestsvc.GetExecutionResponse{
		Periods: []*backtestpb.Period{{
			Date:                  protoYMD(2024, 8, 1),
			AlgorithmPeriodReturn: 0.04,
			MaxDrawdown:           -0.01,
			PortfolioValue:        50,
			Sharpe:                floatPtr(0.7),
		}},
	}, nil).Once()
	test.backend.On("GetBacktest", mock.Anything, mock.MatchedBy(func(req *backtestsvc.GetBacktestRequest) bool {
		return req.GetName() == "spy"
	})).Return(&backtestsvc.GetBacktestResponse{
		Backtest: &backtestpb.Backtest{Benchmark: "SPY"},
	}, nil).Once()

	resp, err := test.uut.QueryData(context.Background(), &grafana.QueryDataRequest{
		Queries: []grafana.DataQuery{{
			RefID:     "A",
			QueryType: ListExecutionOverview,
			TimeRange: grafana.TimeRange{From: from, To: to},
			JSON:      json.RawMessage(`{"queryType":"ListExecutionOverview","backtest":"spy"}`),
		}},
	})
	test.Require().NoError(err)
	test.Require().Equal(grafana.StatusOK, resp.Responses["A"].Status)
	frame := resp.Responses["A"].Frames[0]
	test.Equal(1, frame.Fields[0].Len())
	test.Equal("spy", test.overviewString(frame, "backtest", 0))
	test.Nil(test.overviewFloatPtr(frame, "sortino", 0))
	test.Equal("not available", test.overviewString(frame, "benchmark", 0))
}

func overviewField(frame *data.Frame, name string) *data.Field {
	for _, field := range frame.Fields {
		if field.Name == name {
			return field
		}
	}
	return nil
}

func overviewUnit(frame *data.Frame, name string) string {
	field := overviewField(frame, name)
	if field == nil || field.Config == nil {
		return ""
	}
	return field.Config.Unit
}

func (test *DatasouceTest) overviewString(frame *data.Frame, name string, i int) string {
	test.T().Helper()
	field := overviewField(frame, name)
	test.Require().NotNil(field, name)
	return field.At(i).(string)
}

func (test *DatasouceTest) overviewFloat(frame *data.Frame, name string, i int) float64 {
	test.T().Helper()
	value := test.overviewFloatPtr(frame, name, i)
	test.Require().NotNil(value, name)
	return *value
}

func (test *DatasouceTest) overviewFloatPtr(frame *data.Frame, name string, i int) *float64 {
	test.T().Helper()
	field := overviewField(frame, name)
	test.Require().NotNil(field, name)
	switch value := field.At(i).(type) {
	case *float64:
		return value
	case float64:
		return &value
	default:
		test.Failf(name, "unexpected type %T", field.At(i))
		return nil
	}
}

func TestOverviewLabelsTheReturnBasis(t *testing.T) {
	last := &backtestpb.Period{AlgorithmPeriodReturn: 0.128124, PriceReturn: 0.111963}

	total := overviewRow{}
	withReturnBasis(&total, backtestpb.ReturnBasis_RETURN_BASIS_TOTAL, last)
	require.Equal(t, "Total return (dividends paid on the ex-date)", total.returnBasis)
	require.NotNil(t, total.priceReturn)
	require.InDelta(t, 0.111963, *total.priceReturn, 1e-12)

	adjusted := overviewRow{}
	withReturnBasis(&adjusted, backtestpb.ReturnBasis_RETURN_BASIS_ADJUSTED_CLOSE, last)
	require.Equal(t, "Adjusted-close return (dividends inside prices)", adjusted.returnBasis)
	require.Nil(t, adjusted.priceReturn)

	legacy := overviewRow{}
	withReturnBasis(&legacy, backtestpb.ReturnBasis_RETURN_BASIS_UNSPECIFIED, last)
	require.Equal(t, "Return before dividends were counted", legacy.returnBasis)
	require.Nil(t, legacy.priceReturn)
}
