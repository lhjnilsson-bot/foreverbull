package backtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lhjnilsson/foreverbull/pkg/backtest/internal/repository"
	commonPB "github.com/lhjnilsson/foreverbull/pkg/pb"
	pb "github.com/lhjnilsson/foreverbull/pkg/pb/backtest"
	servicePb "github.com/lhjnilsson/foreverbull/pkg/pb/backtest/service"
	"github.com/lhjnilsson/foreverbull/pkg/pb/finance"
	"github.com/lhjnilsson/foreverbull/pkg/service/worker"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type fakeMemberSymbols struct {
	byDate    map[string][]string
	intervals []repository.MemberInterval
	err       error
}

func (f fakeMemberSymbols) SymbolsAt(_ context.Context, _ string, t time.Time) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.byDate[t.UTC().Format("2006-01-02")], nil
}

func (f fakeMemberSymbols) List(_ context.Context, _ string) ([]repository.MemberInterval, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.intervals, nil
}

// etfMembership marks a backtest as ETF-built: it has universe_member rows.
var etfMembership = []repository.MemberInterval{{ISIN: "US0378331005", FromDate: time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC)}}

type fakeBacktests struct {
	backtest *pb.Backtest
	err      error
}

func (f fakeBacktests) Get(_ context.Context, _ string) (*pb.Backtest, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.backtest, nil
}

func date(year, month, day int32) *commonPB.Date {
	return &commonPB.Date{Year: year, Month: month, Day: day}
}

// demoBacktest is a symbol-list backtest like examples/demo.yaml: one 2023
// period, SPY benchmark, and no universe_member rows.
func demoBacktest() *pb.Backtest {
	return &pb.Backtest{
		Name:      "demo",
		Benchmark: "SPY",
		Periods: []*pb.Backtest_Period{{
			StartDate: date(2023, 1, 1),
			EndDate:   date(2023, 12, 31),
			Assets: []*pb.Backtest_Period_Asset{
				{Symbol: "MSFT", Enabled: true},
				{Symbol: "XOM", Enabled: false},
				{Symbol: "AAPL", Enabled: true},
			},
		}},
	}
}

// drainTimeout bounds a run. A mock that refuses a call in the runner goroutine
// exits that goroutine without closing the channel, so an unbounded drain hangs.
const drainTimeout = 5 * time.Second

func drain(t *testing.T, ch <-chan *servicePb.RunExecutionResponse) {
	t.Helper()
	deadline := time.After(drainTimeout)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("run did not finish: the runner stopped without closing its channel")
		}
	}
}

func periodAt(t time.Time) *servicePb.GetCurrentPeriodResponse {
	return &servicePb.GetCurrentPeriodResponse{
		IsRunning: true,
		Portfolio: &finance.Portfolio{Timestamp: timestamppb.New(t)},
	}
}

func TestRunBacktestProcessReceivesMembersAtT(t *testing.T) {
	t.Parallel()

	fbDay := time.Date(2022, 12, 31, 15, 0, 0, 0, time.UTC)
	metaDay := time.Date(2023, 1, 31, 15, 0, 0, 0, time.UTC)

	client := servicePb.NewMockEngineSessionClient(t)
	pool := worker.NewMockPool(t)
	client.On("RunBacktest", mock.Anything, mock.Anything).
		Return(&servicePb.RunBacktestResponse{NumPeriods: 2}, nil).Once()
	client.On("GetCurrentPeriod", mock.Anything, mock.Anything).
		Return(periodAt(fbDay), nil).Once()
	client.On("GetCurrentPeriod", mock.Anything, mock.Anything).
		Return(periodAt(metaDay), nil).Once()
	client.On("GetCurrentPeriod", mock.Anything, mock.Anything).
		Return(&servicePb.GetCurrentPeriodResponse{IsRunning: false}, nil).Once()
	client.On("PlaceOrdersAndContinue", mock.Anything, mock.Anything).
		Return(&servicePb.PlaceOrdersAndContinueResponse{}, nil).Twice()

	pool.On("Process", mock.Anything, fbDay, []string{"AAPL", "FB"}, mock.Anything).
		Return(nil, nil).Once()
	pool.On("Process", mock.Anything, metaDay, []string{"AAPL", "META", "MSFT"}, mock.Anything).
		Return(nil, nil).Once()

	session := &ZipelineSession{
		client: client,
		symbolsAt: fakeMemberSymbols{byDate: map[string][]string{
			"2022-12-31": {"AAPL", "FB"},
			"2023-01-31": {"AAPL", "META", "MSFT"},
		}, intervals: etfMembership},
		backtests: fakeBacktests{backtest: &pb.Backtest{Name: "sp100", Benchmark: "OEF"}},
	}

	ch, err := session.RunBacktest(context.Background(), &pb.Execution{Backtest: "sp100"}, pool)
	require.NoError(t, err)
	drain(t, ch)
}

func TestRunBacktestEmptyUniversePassesEmptySymbols(t *testing.T) {
	t.Parallel()

	day := time.Date(2022, 12, 30, 15, 0, 0, 0, time.UTC)
	client := servicePb.NewMockEngineSessionClient(t)
	pool := worker.NewMockPool(t)
	client.On("RunBacktest", mock.Anything, mock.Anything).
		Return(&servicePb.RunBacktestResponse{NumPeriods: 1}, nil).Once()
	client.On("GetCurrentPeriod", mock.Anything, mock.Anything).
		Return(periodAt(day), nil).Once()
	client.On("GetCurrentPeriod", mock.Anything, mock.Anything).
		Return(&servicePb.GetCurrentPeriodResponse{IsRunning: false}, nil).Once()
	client.On("PlaceOrdersAndContinue", mock.Anything, mock.Anything).
		Return(&servicePb.PlaceOrdersAndContinueResponse{}, nil).Once()
	pool.On("Process", mock.Anything, day, []string{}, mock.Anything).
		Return(nil, nil).Once()

	session := &ZipelineSession{
		client:    client,
		symbolsAt: fakeMemberSymbols{byDate: map[string][]string{}, intervals: etfMembership},
		backtests: fakeBacktests{backtest: &pb.Backtest{Name: "sp100", Benchmark: "OEF"}},
	}

	ch, err := session.RunBacktest(context.Background(), &pb.Execution{Backtest: "sp100"}, pool)
	require.NoError(t, err)
	drain(t, ch)
}

func TestMemberSymbolsAtEmptyInputs(t *testing.T) {
	t.Parallel()

	got, err := memberSymbolsAt(context.Background(), nil, "sp100", time.Now())
	require.NoError(t, err)
	require.Empty(t, got)

	got, err = memberSymbolsAt(context.Background(), fakeMemberSymbols{}, "", time.Now())
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestRunBacktestSendsStoredBenchmark(t *testing.T) {
	t.Parallel()

	client := servicePb.NewMockEngineSessionClient(t)
	pool := worker.NewMockPool(t)
	client.On("RunBacktest", mock.Anything, mock.MatchedBy(func(req *servicePb.RunBacktestRequest) bool {
		return req.GetBenchmark() == "SPY" && req.GetExecution().GetBacktest() == "demo"
	})).Return(&servicePb.RunBacktestResponse{NumPeriods: 0}, nil).Once()
	client.On("GetCurrentPeriod", mock.Anything, mock.Anything).
		Return(&servicePb.GetCurrentPeriodResponse{IsRunning: false}, nil).Once()

	session := &ZipelineSession{
		client:    client,
		symbolsAt: fakeMemberSymbols{},
		backtests: fakeBacktests{backtest: demoBacktest()},
	}

	ch, err := session.RunBacktest(context.Background(), &pb.Execution{Backtest: "demo"}, pool)
	require.NoError(t, err)
	drain(t, ch)
}

func TestRunBacktestSymbolListPassesEnabledPeriodAssets(t *testing.T) {
	t.Parallel()

	// Zipline reports the session close; 21:00 UTC is still 2023-02-15.
	day := time.Date(2023, 2, 15, 21, 0, 0, 0, time.UTC)
	client := servicePb.NewMockEngineSessionClient(t)
	pool := worker.NewMockPool(t)
	client.On("RunBacktest", mock.Anything, mock.Anything).
		Return(&servicePb.RunBacktestResponse{NumPeriods: 1}, nil).Once()
	client.On("GetCurrentPeriod", mock.Anything, mock.Anything).
		Return(periodAt(day), nil).Once()
	client.On("GetCurrentPeriod", mock.Anything, mock.Anything).
		Return(&servicePb.GetCurrentPeriodResponse{IsRunning: false}, nil).Once()
	order := &finance.Order{Symbol: "AAPL", Amount: 10}
	client.On("PlaceOrdersAndContinue", mock.Anything, mock.MatchedBy(func(req *servicePb.PlaceOrdersAndContinueRequest) bool {
		return len(req.GetOrders()) == 1 && req.GetOrders()[0].GetSymbol() == "AAPL" && req.GetOrders()[0].GetAmount() == 10
	})).Return(&servicePb.PlaceOrdersAndContinueResponse{}, nil).Once()
	pool.On("Process", mock.Anything, day, []string{"AAPL", "MSFT"}, mock.Anything).
		Return([]*finance.Order{order}, nil).Once()

	session := &ZipelineSession{
		client:    client,
		symbolsAt: fakeMemberSymbols{},
		backtests: fakeBacktests{backtest: demoBacktest()},
	}

	ch, err := session.RunBacktest(context.Background(), &pb.Execution{Backtest: "demo"}, pool)
	require.NoError(t, err)
	drain(t, ch)
}

func TestRunBacktestRefusesUnreadableBacktest(t *testing.T) {
	t.Parallel()

	readErr := errors.New("backtest table unavailable")
	cases := []struct {
		name      string
		backtest  string
		symbolsAt fakeMemberSymbols
		backtests fakeBacktests
		wantErr   error
	}{
		{name: "missing backtest name", backtest: "", backtests: fakeBacktests{backtest: demoBacktest()}},
		{name: "backtest read fails", backtest: "demo", backtests: fakeBacktests{err: readErr}, wantErr: readErr},
		{name: "membership read fails", backtest: "demo", symbolsAt: fakeMemberSymbols{err: readErr}, backtests: fakeBacktests{backtest: demoBacktest()}, wantErr: readErr},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// No RunBacktest expectation: Zipline must not start a run the session cannot feed.
			client := servicePb.NewMockEngineSessionClient(t)
			pool := worker.NewMockPool(t)
			session := &ZipelineSession{client: client, symbolsAt: tc.symbolsAt, backtests: tc.backtests}

			ch, err := session.RunBacktest(context.Background(), &pb.Execution{Backtest: tc.backtest}, pool)
			require.Error(t, err)
			require.Nil(t, ch)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
			}
		})
	}
}

func TestPeriodSymbolsAt(t *testing.T) {
	t.Parallel()

	twoYears := []*pb.Backtest_Period{
		{
			StartDate: date(2022, 1, 1),
			EndDate:   date(2022, 12, 31),
			Assets: []*pb.Backtest_Period_Asset{
				{Symbol: "FB", Enabled: true},
				{Symbol: "AAPL", Enabled: true},
			},
		},
		{
			StartDate: date(2023, 1, 1),
			EndDate:   date(2023, 12, 31),
			Assets: []*pb.Backtest_Period_Asset{
				{Symbol: "META", Enabled: true},
				{Symbol: "AAPL", Enabled: true},
				{Symbol: "", Enabled: true},
				{Symbol: "XOM", Enabled: false},
			},
		},
	}
	overlapping := []*pb.Backtest_Period{
		{StartDate: date(2023, 1, 1), EndDate: date(2023, 6, 30), Assets: []*pb.Backtest_Period_Asset{{Symbol: "MSFT", Enabled: true}}},
		{StartDate: date(2023, 6, 1), EndDate: date(2023, 12, 31), Assets: []*pb.Backtest_Period_Asset{{Symbol: "MSFT", Enabled: true}, {Symbol: "KO", Enabled: true}}},
	}
	oslo := time.FixedZone("UTC+2", 2*60*60)

	cases := []struct {
		name    string
		periods []*pb.Backtest_Period
		at      time.Time
		want    []string
	}{
		{name: "first period", periods: twoYears, at: time.Date(2022, 6, 1, 21, 0, 0, 0, time.UTC), want: []string{"AAPL", "FB"}},
		{name: "end date is inclusive", periods: twoYears, at: time.Date(2022, 12, 31, 21, 0, 0, 0, time.UTC), want: []string{"AAPL", "FB"}},
		{name: "second period drops disabled and blank", periods: twoYears, at: time.Date(2023, 1, 3, 21, 0, 0, 0, time.UTC), want: []string{"AAPL", "META"}},
		{name: "date is taken in UTC", periods: twoYears, at: time.Date(2023, 1, 1, 1, 0, 0, 0, oslo), want: []string{"AAPL", "FB"}},
		{name: "before every period", periods: twoYears, at: time.Date(2021, 12, 31, 21, 0, 0, 0, time.UTC), want: []string{}},
		{name: "after every period", periods: twoYears, at: time.Date(2024, 1, 2, 21, 0, 0, 0, time.UTC), want: []string{}},
		{name: "overlapping periods list a symbol once", periods: overlapping, at: time.Date(2023, 6, 15, 21, 0, 0, 0, time.UTC), want: []string{"KO", "MSFT"}},
		{name: "no periods", periods: nil, at: time.Date(2023, 6, 15, 21, 0, 0, 0, time.UTC), want: []string{}},
		{name: "period without dates is skipped", periods: []*pb.Backtest_Period{{Assets: []*pb.Backtest_Period_Asset{{Symbol: "AAPL", Enabled: true}}}}, at: time.Date(2023, 6, 15, 21, 0, 0, 0, time.UTC), want: []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, periodSymbolsAt(tc.periods, tc.at))
		})
	}
}
