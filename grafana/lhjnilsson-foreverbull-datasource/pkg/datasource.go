package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	grafana "github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/datasource"
	"github.com/grafana/grafana-plugin-sdk-go/backend/instancemgmt"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	"github.com/grafana/grafana-plugin-sdk-go/data"
	backtestpb "github.com/lhjnilsson/foreverbull/pkg/pb/backtest"
	backtestsvc "github.com/lhjnilsson/foreverbull/pkg/pb/backtest/service"
	strategysvc "github.com/lhjnilsson/foreverbull/pkg/pb/strategy/service"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// Make sure Datasource implements required interfaces. This is important to do
// since otherwise we will only get a not implemented error response from plugin in
// runtime. In this example datasource instance implements backend.QueryDataHandler,
// backend.CheckHealthHandler interfaces. Plugin should not implement all these
// interfaces - only those which are required for a particular task.
var (
	_ grafana.QueryDataHandler      = (*Datasource)(nil)
	_ grafana.CheckHealthHandler    = (*Datasource)(nil)
	_ instancemgmt.InstanceDisposer = (*Datasource)(nil)
)

func NewDatasource(_ context.Context, _ grafana.DataSourceInstanceSettings) (instancemgmt.Instance, error) {
	foreverbull, exists := os.LookupEnv("BROKER_URL")
	if !exists {
		foreverbull = "localhost:50055"
	}

	log := log.DefaultLogger.With("datasource", "foreverbull")

	conn, err := dialBroker(foreverbull)
	if err != nil {
		log.Error("Fail to connecto to foreverbull backend", err)
		return nil, fmt.Errorf("could not connect: %w", err)
	}

	mux := datasource.NewQueryTypeMux()
	ds := &Datasource{
		queryMux: mux,
		backend:  backtestsvc.NewBacktestServicerClient(conn),
		strategy: strategysvc.NewStrategyServicerClient(conn),
		conn:     conn,
		log:      log,
	}
	ds.registerResources()
	mux.HandleFunc(GetExecutionMetric, ds.HandleGetExecutionMetric)
	mux.HandleFunc(ListExecutionOverview, ds.HandleListExecutionOverview)

	return ds, nil
}

type QueryHandlerFunc func(context.Context, grafana.QueryDataRequest, grafana.DataQuery) grafana.DataResponse

func processQueries(ctx context.Context, req *grafana.QueryDataRequest, handler QueryHandlerFunc) *grafana.QueryDataResponse {
	res := grafana.Responses{}
	if req == nil || req.Queries == nil {
		return &grafana.QueryDataResponse{
			Responses: res,
		}
	}
	for _, v := range req.Queries {
		res[v.RefID] = handler(ctx, *req, v)
	}

	return &grafana.QueryDataResponse{
		Responses: res,
	}
}

// backtestAPI is the BacktestServicer methods this plugin calls.
type backtestAPI interface {
	ListBacktests(ctx context.Context, in *backtestsvc.ListBacktestsRequest, opts ...grpc.CallOption) (*backtestsvc.ListBacktestsResponse, error)
	GetBacktest(ctx context.Context, in *backtestsvc.GetBacktestRequest, opts ...grpc.CallOption) (*backtestsvc.GetBacktestResponse, error)
	ListExecutions(ctx context.Context, in *backtestsvc.ListExecutionsRequest, opts ...grpc.CallOption) (*backtestsvc.ListExecutionsResponse, error)
	GetExecution(ctx context.Context, in *backtestsvc.GetExecutionRequest, opts ...grpc.CallOption) (*backtestsvc.GetExecutionResponse, error)
}

// strategyAPI is the StrategyServicer methods this plugin calls.
// Unimplemented (strategy.Module not registered) is treated as an empty list.
type strategyAPI interface {
	ListStrategies(ctx context.Context, in *strategysvc.ListStrategiesRequest, opts ...grpc.CallOption) (*strategysvc.ListStrategiesResponse, error)
	ListExperiments(ctx context.Context, in *strategysvc.ListExperimentsRequest, opts ...grpc.CallOption) (*strategysvc.ListExperimentsResponse, error)
}

type Datasource struct {
	queryMux *datasource.QueryTypeMux
	backend  backtestAPI
	strategy strategyAPI
	conn     *grpc.ClientConn
	grafana.CallResourceHandler
	log log.Logger
}

func (d *Datasource) Dispose() {
	if d == nil || d.conn == nil {
		return
	}
	if err := d.conn.Close(); err != nil {
		d.log.Error("fail to close broker connection", err)
	}
	d.conn = nil
}

func (d *Datasource) QueryData(ctx context.Context, req *grafana.QueryDataRequest) (*grafana.QueryDataResponse, error) {
	return d.queryMux.QueryData(ctx, req)
}

// brokerCredentials reads GRAFANA_BROKER_TOKEN. Empty means the dial sends no metadata.
func brokerCredentials() credentials.PerRPCCredentials {
	token := strings.TrimSpace(os.Getenv("GRAFANA_BROKER_TOKEN"))
	if token == "" {
		return nil
	}
	return bearerToken(token)
}

type bearerToken string

func (t bearerToken) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(t)}, nil
}

// The in-cluster broker listener is plaintext. Grafana's NetworkPolicy
// allows only DNS and that gRPC port. The bearer is still sent.
func (bearerToken) RequireTransportSecurity() bool { return false }

func dialBroker(address string, extra ...grpc.DialOption) (*grpc.ClientConn, error) {
	opts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	if creds := brokerCredentials(); creds != nil {
		opts = append(opts, grpc.WithPerRPCCredentials(creds))
	}
	opts = append(opts, extra...)
	return grpc.NewClient(address, opts...)
}

func (d *Datasource) CheckHealth(ctx context.Context, _ *grafana.CheckHealthRequest) (*grafana.CheckHealthResult, error) {
	if d == nil || d.backend == nil {
		return &grafana.CheckHealthResult{
			Status:  grafana.HealthStatusError,
			Message: "broker client is not configured",
		}, nil
	}

	if _, err := d.backend.ListBacktests(ctx, &backtestsvc.ListBacktestsRequest{}); err != nil {
		return &grafana.CheckHealthResult{
			Status:  grafana.HealthStatusError,
			Message: fmt.Sprintf("broker unreachable: %v", err),
		}, nil
	}

	return &grafana.CheckHealthResult{
		Status:  grafana.HealthStatusOk,
		Message: "Data source is working",
	}, nil
}

func (d *Datasource) HandleGetExecutionMetric(ctx context.Context, req *grafana.QueryDataRequest) (*grafana.QueryDataResponse, error) {
	return processQueries(ctx, req, d.handleGetExecutionMetric), nil
}

type QueryMetric string

const (
	PortfolioValue   QueryMetric = "portfolio_value"
	Returns          QueryMetric = "returns"
	PnL              QueryMetric = "pnl"
	Cash             QueryMetric = "cash"
	MaxDrawdown      QueryMetric = "max_drawdown"
	Benchmark        QueryMetric = "benchmark"
	Alpha            QueryMetric = "alpha"
	Beta             QueryMetric = "beta"
	Sharpe           QueryMetric = "sharpe"
	Sortino          QueryMetric = "sortino"
	CapitalUsed      QueryMetric = "capital_used"
	LongCount        QueryMetric = "long_count"
	ShortCount       QueryMetric = "short_count"
	LongValue        QueryMetric = "long_value"
	ShortValue       QueryMetric = "short_value"
	GrossLeverage    QueryMetric = "gross_leverage"
	NetLeverage      QueryMetric = "net_leverage"
	MaxLeverage      QueryMetric = "max_leverage"
	LongExposure     QueryMetric = "long_exposure"
	ShortExposure    QueryMetric = "short_exposure"
	EndingExposure   QueryMetric = "ending_exposure"
	StartingExposure QueryMetric = "starting_exposure"
	// TotalReturn is the run's return from its start to each close.
	TotalReturn QueryMetric = "total_return"
	// PriceReturn is the run's return from its start without the dividend
	// cash it was credited. Only a total-return run has one.
	PriceReturn QueryMetric = "price_return"
	// PriceReturns is each period's price return, the daily counterpart of
	// returns.
	PriceReturns QueryMetric = "price_returns"
	// BenchmarkPriceReturn is the benchmark's return without its dividends.
	BenchmarkPriceReturn QueryMetric = "benchmark_price_return"
)

var listedMetrics = []QueryMetric{
	PortfolioValue,
	Returns,
	PnL,
	Cash,
	MaxDrawdown,
	Benchmark,
	Alpha,
	Beta,
	Sharpe,
	Sortino,
	CapitalUsed,
	LongCount,
	ShortCount,
	LongValue,
	ShortValue,
	GrossLeverage,
	NetLeverage,
	MaxLeverage,
	LongExposure,
	ShortExposure,
	EndingExposure,
	StartingExposure,
	TotalReturn,
	PriceReturn,
	PriceReturns,
	BenchmarkPriceReturn,
}

// Execution is the canonical query-JSON object for an execution id.
// Canonical body (ANALYSIS.md §6):
//
//	{"queryType":"GetExecutionMetric","execution":{"id":"<uuid>"},"metrics":[{"name":"portfolio_value"}]}
//
// Legacy dashboards may send "executionId" as a string; that is still accepted.
type Execution struct {
	ID string `json:"id"`
}

type Metric struct {
	Name QueryMetric `json:"name"`
}

type queryModel struct {
	ExecutionID string
	Metrics     []Metric
}

func parseExecutionRef(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var id string
	if err := json.Unmarshal(raw, &id); err == nil {
		return id
	}
	var obj Execution
	if err := json.Unmarshal(raw, &obj); err == nil {
		return obj.ID
	}
	return ""
}

func parseMetrics(raw json.RawMessage) ([]Metric, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var names []string
	if err := json.Unmarshal(raw, &names); err == nil {
		metrics := make([]Metric, 0, len(names))
		for _, name := range names {
			metrics = append(metrics, Metric{Name: QueryMetric(name)})
		}
		return metrics, nil
	}
	var metrics []Metric
	if err := json.Unmarshal(raw, &metrics); err != nil {
		return nil, err
	}
	return metrics, nil
}

func (q *queryModel) UnmarshalJSON(b []byte) error {
	var raw struct {
		Execution   json.RawMessage `json:"execution"`
		ExecutionID json.RawMessage `json:"executionId"`
		Metrics     json.RawMessage `json:"metrics"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	q.ExecutionID = parseExecutionRef(raw.Execution)
	if q.ExecutionID == "" {
		q.ExecutionID = parseExecutionRef(raw.ExecutionID)
	}
	metrics, err := parseMetrics(raw.Metrics)
	if err != nil {
		return err
	}
	q.Metrics = metrics
	return nil
}

func periodTime(period *backtestpb.Period) time.Time {
	if period == nil || period.Date == nil {
		return time.Time{}
	}
	return time.Date(int(period.Date.Year), time.Month(int(period.Date.Month)), int(period.Date.Day), 0, 0, 0, 0, time.UTC)
}

func (d *Datasource) handleGetExecutionMetric(ctx context.Context, _ grafana.QueryDataRequest, q grafana.DataQuery) grafana.DataResponse {
	log := d.log.With("GetExecutionMetric")

	var qm queryModel
	err := json.Unmarshal(q.JSON, &qm)
	if err != nil {
		log.Error("fail to parse json", err)
		return grafana.ErrDataResponse(grafana.StatusBadRequest, fmt.Sprintf("json unmarshal: %v", err.Error()))
	}

	if qm.ExecutionID == "" {
		log.Error("executionId is missing")
		return grafana.ErrDataResponse(grafana.StatusBadRequest, "executionId is missing")
	}

	if len(qm.Metrics) == 0 {
		log.Error("metrics is missing")
		return grafana.ErrDataResponse(grafana.StatusBadRequest, "metrics is missing")
	}

	execution, err := d.backend.GetExecution(ctx, &backtestsvc.GetExecutionRequest{ExecutionId: qm.ExecutionID})
	if err != nil {
		return grafana.ErrDataResponse(grafana.StatusBadRequest, fmt.Sprintf("get execution: %v", err.Error()))
	}

	// Overlay is repeated GetExecutionMetric targets (refId A/B), not a second query type.
	seriesLabel := executionLabel(execution.GetExecution())
	if seriesLabel == "" {
		seriesLabel = qm.ExecutionID
	}
	seriesLabels := data.Labels{
		"execution":    seriesLabel,
		"execution_id": qm.ExecutionID,
	}

	periods := execution.GetPeriods()
	times := make([]time.Time, len(periods))
	portfolioValue := make([]float64, len(periods))
	returns := make([]float64, len(periods))
	pnl := make([]float64, len(periods))
	cash := make([]float64, len(periods))
	maxDrawdown := make([]float64, len(periods))
	benchmark := make([]*float64, len(periods))
	alpha := make([]*float64, len(periods))
	beta := make([]*float64, len(periods))
	sharpe := make([]*float64, len(periods))
	sortino := make([]*float64, len(periods))
	capitalUsed := make([]float64, len(periods))
	longCount := make([]int32, len(periods))
	shortCount := make([]int32, len(periods))
	longValue := make([]float64, len(periods))
	shortValue := make([]float64, len(periods))
	grossLeverage := make([]float64, len(periods))
	netLeverage := make([]float64, len(periods))
	maxLeverage := make([]float64, len(periods))
	longExposure := make([]float64, len(periods))
	shortExposure := make([]float64, len(periods))
	endingExposure := make([]float64, len(periods))
	startingExposure := make([]float64, len(periods))
	totalReturn := make([]float64, len(periods))
	priceReturn := make([]float64, len(periods))
	priceReturns := make([]float64, len(periods))
	benchmarkPriceReturn := make([]*float64, len(periods))

	for i, period := range periods {
		times[i] = periodTime(period)
		portfolioValue[i] = period.PortfolioValue
		returns[i] = period.Returns
		pnl[i] = period.PNL
		cash[i] = period.EndingCash
		maxDrawdown[i] = period.MaxDrawdown
		benchmark[i] = period.BenchmarkPeriodReturn
		alpha[i] = period.Alpha
		beta[i] = period.Beta
		sharpe[i] = period.Sharpe
		sortino[i] = period.Sortino
		capitalUsed[i] = period.CapitalUsed
		longCount[i] = period.LongsCount
		shortCount[i] = period.ShortsCount
		longValue[i] = period.LongValue
		shortValue[i] = period.ShortValue
		grossLeverage[i] = period.GrossLeverage
		netLeverage[i] = period.NetLeverage
		maxLeverage[i] = period.MaxLeverage
		longExposure[i] = period.LongExposure
		shortExposure[i] = period.ShortExposure
		endingExposure[i] = period.EndingExposure
		startingExposure[i] = period.StartingExposure
		totalReturn[i] = period.AlgorithmPeriodReturn
		priceReturn[i] = period.PriceReturn
		priceReturns[i] = dailyPriceReturn(periods, i)
		benchmarkPriceReturn[i] = period.BenchmarkPriceReturn
	}
	basis := execution.GetExecution().GetReturnBasis()

	frame := data.NewFrame(seriesLabel)
	frame.Fields = append(frame.Fields, data.NewField("time", nil, times))
	frame.Meta = &data.FrameMeta{Notices: returnNotices(execution.GetExecution())}
	for _, metric := range qm.Metrics {
		if returnMetric(metric.Name) {
			appendReturnField(frame, metric.Name, basis, seriesLabels, map[QueryMetric]interface{}{
				Returns: returns, Benchmark: benchmark, TotalReturn: totalReturn, PriceReturn: priceReturn,
				PriceReturns: priceReturns, BenchmarkPriceReturn: benchmarkPriceReturn,
			}[metric.Name])
			continue
		}
		switch metric.Name {
		case PortfolioValue:
			appendMetricField(frame, string(PortfolioValue), seriesLabels, portfolioValue)
		case PnL:
			appendMetricField(frame, string(PnL), seriesLabels, pnl)
		case Cash:
			appendMetricField(frame, string(Cash), seriesLabels, cash)
		case MaxDrawdown:
			appendMetricField(frame, string(MaxDrawdown), seriesLabels, maxDrawdown)
		case Alpha:
			appendMetricField(frame, string(Alpha), seriesLabels, alpha)
		case Beta:
			appendMetricField(frame, string(Beta), seriesLabels, beta)
		case Sharpe:
			appendMetricField(frame, string(Sharpe), seriesLabels, sharpe)
		case Sortino:
			appendMetricField(frame, string(Sortino), seriesLabels, sortino)
		case CapitalUsed:
			appendMetricField(frame, string(CapitalUsed), seriesLabels, capitalUsed)
		case LongCount:
			appendMetricField(frame, string(LongCount), seriesLabels, longCount)
		case ShortCount:
			appendMetricField(frame, string(ShortCount), seriesLabels, shortCount)
		case LongValue:
			appendMetricField(frame, string(LongValue), seriesLabels, longValue)
		case ShortValue:
			appendMetricField(frame, string(ShortValue), seriesLabels, shortValue)
		case GrossLeverage:
			appendMetricField(frame, string(GrossLeverage), seriesLabels, grossLeverage)
		case NetLeverage:
			appendMetricField(frame, string(NetLeverage), seriesLabels, netLeverage)
		case MaxLeverage:
			appendMetricField(frame, string(MaxLeverage), seriesLabels, maxLeverage)
		case LongExposure:
			appendMetricField(frame, string(LongExposure), seriesLabels, longExposure)
		case ShortExposure:
			appendMetricField(frame, string(ShortExposure), seriesLabels, shortExposure)
		case EndingExposure:
			appendMetricField(frame, string(EndingExposure), seriesLabels, endingExposure)
		case StartingExposure:
			appendMetricField(frame, string(StartingExposure), seriesLabels, startingExposure)
		}
	}

	return grafana.DataResponse{
		Frames: []*data.Frame{frame},
		Status: grafana.StatusOK,
	}
}

func appendMetricField(frame *data.Frame, name string, labels data.Labels, values interface{}) {
	field := data.NewField(name, labels, values)
	// Display unit only. Stored numbers are not rescaled.
	cfg := &data.FieldConfig{Unit: metricUnit(name)}
	if label := labels["execution"]; label != "" {
		cfg.DisplayNameFromDS = label + " " + name
	}
	field.SetConfig(cfg)
	frame.Fields = append(frame.Fields, field)
}

// metricUnit matches the zipline period writer (engine.py copies the columns unchanged).
// returns, max_drawdown, benchmark_period_return, and annualized alpha are fractions (0.01 is 1 percent).
// Sharpe, Sortino, and beta are ratios. gross/net/max leverage are exposure/portfolio_value (1 is 1x).
// portfolio_value, pnl, cash, capital_used, long_value, short_value, and exposure fields are currency.
func metricUnit(name string) string {
	switch QueryMetric(name) {
	case PortfolioValue, PnL, Cash, CapitalUsed, LongValue, ShortValue, LongExposure, ShortExposure, EndingExposure, StartingExposure:
		return "currencyUSD"
	case Returns, MaxDrawdown, Benchmark, Alpha, TotalReturn, PriceReturn, PriceReturns, BenchmarkPriceReturn:
		return "percentunit"
	case Sharpe, Sortino, Beta, GrossLeverage, NetLeverage, MaxLeverage:
		return "none"
	default:
		return ""
	}
}
