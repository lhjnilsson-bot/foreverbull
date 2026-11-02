package backtest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/internal/storage"
	"github.com/lhjnilsson/foreverbull/pkg/backtest/engine"
	"github.com/lhjnilsson/foreverbull/pkg/backtest/internal/repository"
	commonPB "github.com/lhjnilsson/foreverbull/pkg/pb"
	pb "github.com/lhjnilsson/foreverbull/pkg/pb/backtest"
	servicePb "github.com/lhjnilsson/foreverbull/pkg/pb/backtest/service"
	"github.com/lhjnilsson/foreverbull/pkg/service/worker"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// MemberSymbols maps a backtest's universe_member rows through asset_symbol at t.
// List tells an ETF-built backtest (membership rows) from a symbol-list one (none).
type MemberSymbols interface {
	SymbolsAt(ctx context.Context, backtestName string, t time.Time) ([]string, error)
	List(ctx context.Context, backtestName string) ([]repository.MemberInterval, error)
}

// BacktestReader returns the stored backtest: its benchmark and yearly period assets.
type BacktestReader interface {
	Get(ctx context.Context, name string) (*pb.Backtest, error)
}

func NewZiplineEngineSession(ctx context.Context, connStr string) (engine.EngineSession, error) {
	conn, err := grpc.NewClient(
		connStr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("error getting grpc client: %w", err)
	}

	client := servicePb.NewEngineSessionClient(conn)

	return &ZipelineSession{client: client}, nil
}

type ZipelineSession struct {
	client     servicePb.EngineSessionClient
	symbolsAt  MemberSymbols
	backtests  BacktestReader
	runFailure chan error
	leftOut    *leftOut
	events     []repository.SymbolEvent
}

// RunFailure is the error that stopped the run, or nil when the run finished.
// Call it after the period channel has closed. It returns one result.
func (z *ZipelineSession) RunFailure() error {
	if z.runFailure == nil {
		return nil
	}
	return <-z.runFailure
}

// RunNotices are the lines the run summary shows although the run finished,
// such as members left out after their last price. Call it after the period
// channel has closed.
func (z *ZipelineSession) RunNotices() []string {
	lines := z.leftOut.notices()
	return append(lines, coverageNotices(z.events, z.leftOut)...)
}

// ListingLine is the run-summary line for a confirmed listing.
func ListingLine(symbol string, on time.Time) string {
	return fmt.Sprintf("%s was listed on %s and has no prices before then.", symbol, on.UTC().Format(time.DateOnly))
}

// coverageNotices adds each confirmed listing, and a delisting the left-out
// record does not already name (a symbol-list run does not filter members).
func coverageNotices(events []repository.SymbolEvent, left *leftOut) []string {
	lines := make([]string, 0)
	for _, event := range events {
		if event.Kind != repository.EventDelisted {
			continue
		}
		if left != nil {
			if _, named := left.lastBar[event.Symbol]; named {
				continue
			}
		}
		lines = append(lines, leftOutLine(event.On, []string{event.Symbol}, false))
	}
	for _, event := range events {
		if event.Kind != repository.EventListed {
			continue
		}
		lines = append(lines, ListingLine(event.Symbol, event.On))
	}
	return lines
}

// symbolSource returns the tickers Process receives on date t.
type symbolSource func(ctx context.Context, t time.Time) ([]string, error)

func (z *ZipelineSession) RunBacktest(ctx context.Context, execution *pb.Execution, workers worker.Pool) (chan *servicePb.RunExecutionResponse, error) {
	members, backtests, pool, err := z.lookups(ctx)
	if err != nil {
		return nil, err
	}
	closePool := func() {
		if pool != nil {
			pool.Close()
		}
	}

	left := newLeftOut()
	stored, symbolsAt, events, err := runPlan(ctx, backtests, members, execution, left)
	if err != nil {
		closePool()
		return nil, err
	}
	z.events = events

	// An empty benchmark makes Zipline use a flat zero series, so the stored
	// benchmark_period_return would read 0 whatever the benchmark did.
	req := servicePb.RunBacktestRequest{
		Execution: execution,
		Benchmark: stored.GetBenchmark(),
	}

	rsp, err := z.client.RunBacktest(ctx, &req)
	if err != nil {
		closePool()
		if refused, ok := err.(interface{ GRPCStatus() *status.Status }); ok &&
			refused.GRPCStatus().Code() == codes.FailedPrecondition {
			// Zipline refused the run with the user's whole sentence (a
			// benchmark gap, a damaged bundle). It is returned unwrapped so
			// fbull shows it as written.
			return nil, err
		}
		return nil, fmt.Errorf("error running: %w", err)
	}
	totalPeriods := rsp.NumPeriods

	log.Debug().Any("response", rsp).Msg("run backtest sent")

	ch := make(chan *servicePb.RunExecutionResponse)
	z.runFailure = make(chan error, 1)
	z.leftOut = left

	runner := func() {
		defer closePool()
		defer close(ch)
		var runErr error
		defer func() { z.runFailure <- runErr }()
		var currentPeriod int64 = 0
		for {
			period, err := z.client.GetCurrentPeriod(ctx, &servicePb.GetCurrentPeriodRequest{})
			if err != nil {
				runErr = err
				return
			}
			if !period.IsRunning {
				return
			}
			currentPeriod++
			excRsp := servicePb.RunExecutionResponse{
				Execution:        execution,
				Portfolio:        period.Portfolio,
				NumTotalPeriods:  totalPeriods,
				NumCurrentPeriod: currentPeriod,
			}

			select {
			case ch <- &excRsp:
			default:
			}
			t := period.Portfolio.Timestamp.AsTime()
			// The filter names a left-out member held in this portfolio.
			left.sawPortfolio(period.Portfolio)
			symbols, err := symbolsAt(ctx, t)
			if err != nil {
				log.Error().Err(err).Msg("error listing members at timestamp")
				runErr = err
				return
			}
			orders, err := workers.Process(ctx, t, symbols, period.Portfolio)
			if err != nil {
				log.Error().Err(err).Msg("error processing orders")
				runErr = err
				return
			}

			_, err = z.client.PlaceOrdersAndContinue(ctx,
				&servicePb.PlaceOrdersAndContinueRequest{
					Orders: orders,
				},
			)
			if err != nil {
				log.Error().Err(err).Msg("error placing orders")
				runErr = err
				return
			}
		}
	}
	go runner()

	return ch, nil
}

// lookups returns the membership and backtest readers. The pool is non-nil
// only when this call opened it, and the caller closes it when the run ends.
func (z *ZipelineSession) lookups(ctx context.Context) (MemberSymbols, BacktestReader, *pgxpool.Pool, error) {
	if z.symbolsAt != nil && z.backtests != nil {
		return z.symbolsAt, z.backtests, nil, nil
	}
	pool, err := pgxpool.New(ctx, environment.GetPostgresURL())
	if err != nil {
		return nil, nil, nil, fmt.Errorf("error connecting to postgres for membership: %w", err)
	}
	var members MemberSymbols = &repository.UniverseMember{Conn: pool}
	if z.symbolsAt != nil {
		members = z.symbolsAt
	}
	var backtests BacktestReader = &repository.Backtest{Conn: pool}
	if z.backtests != nil {
		backtests = z.backtests
	}
	return members, backtests, pool, nil
}

// runPlan reads the backtest once before the first bar and picks where Process
// gets its tickers. An ETF-built backtest has universe_member rows and stays
// point-in-time, so an empty universe at t is still an empty slice. A backtest
// created from a symbol list has no membership rows; its enabled period assets
// are what DownloadFinancialData and SQLIngester put in the bundle, so those
// are its universe. Without that fallback every bar got no symbols and the
// algorithm could not place an order.
//
// An ETF member whose prices ended before t (a delisting the next month-end
// list has not dropped yet) is left out of Process and recorded in left, so
// the run summary names it instead of the algorithm reading a stale price.
func runPlan(ctx context.Context, backtests BacktestReader, members MemberSymbols, execution *pb.Execution, left *leftOut) (*pb.Backtest, symbolSource, []repository.SymbolEvent, error) {
	if execution == nil {
		return nil, nil, nil, errors.New("execution has no backtest")
	}
	if environment.GetAuthEnabled() {
		return runPlanOwned(ctx, backtests, members, execution, left)
	}
	return runPlanNamed(ctx, backtests, members, execution.GetBacktest(), left)
}

func runPlanNamed(ctx context.Context, backtests BacktestReader, members MemberSymbols, backtestName string, left *leftOut) (*pb.Backtest, symbolSource, []repository.SymbolEvent, error) {
	if backtestName == "" {
		return nil, nil, nil, errors.New("execution has no backtest")
	}
	stored, err := backtests.Get(ctx, backtestName)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("error getting backtest %s: %w", backtestName, err)
	}
	events, err := readSymbolEvents(ctx, backtests, backtestName)
	if err != nil {
		return nil, nil, nil, err
	}
	intervals, err := members.List(ctx, backtestName)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("error listing universe members of %s: %w", backtestName, err)
	}
	if len(intervals) > 0 {
		source := symbolSource(func(ctx context.Context, t time.Time) ([]string, error) {
			return memberSymbolsAt(ctx, members, backtestName, t)
		})
		reader, ok := members.(lastBarsByName)
		if !ok {
			return stored, source, events, nil
		}
		aliases, err := reader.LastBars(ctx, backtestName, barsProvider(stored), barsEnd(stored))
		if err != nil {
			return nil, nil, nil, fmt.Errorf("error reading last prices of %s: %w", backtestName, err)
		}
		return stored, withoutEndedPrices(source, aliases, left, confirmedDelistings(events)), events, nil
	}
	return stored, func(_ context.Context, t time.Time) ([]string, error) {
		return periodSymbolsAt(stored.GetPeriods(), t), nil
	}, events, nil
}

type symbolEventLister interface {
	SymbolEvents(ctx context.Context, name string) ([]repository.SymbolEvent, error)
}

func readSymbolEvents(ctx context.Context, backtests BacktestReader, name string) ([]repository.SymbolEvent, error) {
	lister, ok := backtests.(symbolEventLister)
	if !ok {
		return nil, errors.New("backtest symbol events are not readable")
	}
	events, err := lister.SymbolEvents(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("error reading symbol events of %s: %w", name, err)
	}
	return events, nil
}

func confirmedDelistings(events []repository.SymbolEvent) map[string]bool {
	confirmed := make(map[string]bool)
	for _, event := range events {
		if event.Kind == repository.EventDelisted {
			confirmed[event.Symbol] = true
		}
	}
	return confirmed
}

type backtestByID interface {
	IDForSession(ctx context.Context, sessionID string) (string, error)
	GetByID(ctx context.Context, id string) (*pb.Backtest, error)
}

type membersByID interface {
	ListByID(ctx context.Context, backtestID string) ([]repository.MemberInterval, error)
	SymbolsAtID(ctx context.Context, backtestID string, t time.Time) ([]string, error)
}

func runPlanOwned(ctx context.Context, backtests BacktestReader, members MemberSymbols, execution *pb.Execution, left *leftOut) (*pb.Backtest, symbolSource, []repository.SymbolEvent, error) {
	ids, ok := backtests.(backtestByID)
	listed, membersOK := members.(membersByID)
	if !ok || !membersOK {
		return nil, nil, nil, errors.New("backtest id is empty")
	}
	backtestID, err := ids.IDForSession(ctx, execution.GetSession())
	if err != nil {
		return nil, nil, nil, fmt.Errorf("error getting backtest for session: %w", err)
	}
	stored, err := ids.GetByID(ctx, backtestID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("error getting backtest %s: %w", backtestID, err)
	}
	events, err := readSymbolEvents(ctx, backtests, stored.GetName())
	if err != nil {
		return nil, nil, nil, err
	}
	intervals, err := listed.ListByID(ctx, backtestID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("error listing universe members of %s: %w", backtestID, err)
	}
	if len(intervals) > 0 {
		source := symbolSource(func(ctx context.Context, t time.Time) ([]string, error) {
			symbols, err := listed.SymbolsAtID(ctx, backtestID, t)
			if err != nil {
				return nil, err
			}
			if symbols == nil {
				return []string{}, nil
			}
			return symbols, nil
		})
		reader, ok := members.(lastBarsByID)
		if !ok {
			return stored, source, events, nil
		}
		aliases, err := reader.LastBarsByID(ctx, backtestID, barsProvider(stored), barsEnd(stored))
		if err != nil {
			return nil, nil, nil, fmt.Errorf("error reading last prices of %s: %w", backtestID, err)
		}
		return stored, withoutEndedPrices(source, aliases, left, confirmedDelistings(events)), events, nil
	}
	return stored, func(_ context.Context, t time.Time) ([]string, error) {
		return periodSymbolsAt(stored.GetPeriods(), t), nil
	}, events, nil
}

// periodSymbolsAt returns the enabled assets of every period whose
// [start_date, end_date] holds the UTC date of t, sorted and without repeats.
// A disabled asset has no bars in the bundle, so it is not offered to Process.
func periodSymbolsAt(periods []*pb.Backtest_Period, t time.Time) []string {
	year, month, day := t.UTC().Date()
	date := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	symbols := make([]string, 0)
	for _, period := range periods {
		if period.GetStartDate() == nil || period.GetEndDate() == nil {
			continue
		}
		if date.Before(commonPB.DateToTime(period.GetStartDate())) || date.After(commonPB.DateToTime(period.GetEndDate())) {
			continue
		}
		for _, asset := range period.GetAssets() {
			if !asset.GetEnabled() || asset.GetSymbol() == "" {
				continue
			}
			if slices.Contains(symbols, asset.GetSymbol()) {
				continue
			}
			symbols = append(symbols, asset.GetSymbol())
		}
	}
	slices.Sort(symbols)
	return symbols
}

func memberSymbolsAt(ctx context.Context, lookup MemberSymbols, backtestName string, t time.Time) ([]string, error) {
	if lookup == nil || backtestName == "" {
		return []string{}, nil
	}
	symbols, err := lookup.SymbolsAt(ctx, backtestName, t)
	if err != nil {
		return nil, err
	}
	if symbols == nil {
		return []string{}, nil
	}
	return symbols, nil
}

func (z *ZipelineSession) GetResult(ctx context.Context, executionId string) (*servicePb.GetResultResponse, error) {
	req := servicePb.GetResultRequest{
		Execution: executionId,
		Upload: &servicePb.GetResultRequest_Upload{
			Bucket: string(storage.ResultsBucket),
			Object: executionId,
		},
	}
	rsp, err := z.client.GetResult(ctx, &req)
	if err != nil {
		return nil, fmt.Errorf("error getting result: %w", err)
	}
	return rsp, nil
}
