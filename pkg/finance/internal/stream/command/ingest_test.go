package command_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/internal/stream"
	"github.com/lhjnilsson/foreverbull/internal/test_helper"
	"github.com/lhjnilsson/foreverbull/pkg/finance/internal/repository"
	"github.com/lhjnilsson/foreverbull/pkg/finance/internal/stream/command"
	"github.com/lhjnilsson/foreverbull/pkg/finance/internal/stream/dependency"
	fs "github.com/lhjnilsson/foreverbull/pkg/finance/stream"
	"github.com/lhjnilsson/foreverbull/pkg/finance/supplier"
	internal_pb "github.com/lhjnilsson/foreverbull/pkg/pb"
	pb "github.com/lhjnilsson/foreverbull/pkg/pb/finance"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/suite"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type IngestCommandTest struct {
	suite.Suite

	db     *pgxpool.Pool
	assets *repository.Asset
	ohlc   *repository.OHLC

	storedAsset     *pb.Asset
	storedOHLCStart *internal_pb.Date
	storedOHLCEnd   *internal_pb.Date

	marketdata *supplier.MockMarketdata
}

func TestIngestCommand(t *testing.T) {
	suite.Run(t, new(IngestCommandTest))
}

func (test *IngestCommandTest) SetupTest() {
	test_helper.SetupEnvironment(test.T(), &test_helper.Containers{
		Postgres: true,
	})

	var err error
	test.db, err = pgxpool.New(context.Background(), environment.GetPostgresURL())
	test.Require().NoError(err)

	err = repository.Recreate(context.TODO(), test.db)
	test.Require().NoError(err)
	test.assets = &repository.Asset{Conn: test.db}
	test.ohlc = &repository.OHLC{Conn: test.db}
	test.storedAsset = &pb.Asset{
		Symbol: "Stored123",
		Name:   "Stored Asset",
	}
	test.Require().NoError(test.assets.Store(context.Background(), test.storedAsset.Symbol, test.storedAsset.Name))

	test.storedOHLCStart = &internal_pb.Date{Year: 2020, Month: 1, Day: 1}
	test.Require().NoError(test.ohlc.Store(context.Background(), "",
		"Stored123", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), 1.0, 4.0, 2.0, 3.0, 5))
	test.Require().NoError(test.ohlc.Store(context.Background(), "",
		"Stored123", time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC), 1.0, 4.0, 2.0, 3.0, 5))
	test.Require().NoError(test.ohlc.Store(context.Background(), "",
		"Stored123", time.Date(2020, 1, 3, 0, 0, 0, 0, time.UTC), 1.0, 4.0, 2.0, 3.0, 5))

	test.storedOHLCEnd = &internal_pb.Date{Year: 2020, Month: 1, Day: 3}

	test.marketdata = new(supplier.MockMarketdata)
}

func (test *IngestCommandTest) TearDownTest() {
}

func (test *IngestCommandTest) TestIngestCommandIngest() {
	message := new(stream.MockMessage)
	message.On("MustGet", stream.DBDep).Return(test.db)
	message.On("MustGet", dependency.MarketDataDep).Return(dividendFree{test.marketdata})

	newAsset := pb.Asset{
		Symbol: "NEW123",
		Name:   "New Asset",
	}
	test.marketdata.On("GetAsset", mock.Anything, newAsset.Symbol).Return(&newAsset, nil)
	test.marketdata.On("GetOHLC", mock.Anything, "NEW123", internal_pb.DateToTime(test.storedOHLCStart),
		mock.Anything).Return(
		[]*pb.OHLC{
			{
				Timestamp: timestamppb.New(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)),
				Open:      10, High: 11, Low: 9, Close: 10.5, Volume: 100,
			},
			{
				Timestamp: timestamppb.New(time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC)),
				Open:      10, High: 11, Low: 9, Close: 10.5, Volume: 100,
			},
			{
				Timestamp: timestamppb.New(time.Date(2020, 1, 3, 0, 0, 0, 0, time.UTC)),
				Open:      10, High: 11, Low: 9, Close: 10.5, Volume: 100,
			},
		},
		nil,
	)

	message.On("ParsePayload", &fs.IngestCommand{}).Return(nil).Run(func(args mock.Arguments) {
		command := args.Get(0).(*fs.IngestCommand)
		// yahoo routes GetOHLC to the mock supplier.
		command.Provider = "yahoo"
		command.Symbols = []string{"NEW123"}
		command.Start = internal_pb.DateToDateString(test.storedOHLCStart)
		end := internal_pb.DateToDateString(test.storedOHLCEnd)
		command.End = &end
	})

	err := command.Ingest(context.Background(), message)
	test.Require().NoError(err)

	asset, err := test.assets.Get(context.Background(), newAsset.Symbol)
	test.Require().NoError(err)
	test.Equal(newAsset.Name, asset.Name)

	exists, err := test.ohlc.Exists(context.Background(), "yahoo", []string{"NEW123"}, test.storedOHLCStart, test.storedOHLCEnd)
	test.Require().NoError(err)
	test.True(exists)

	test.marketdata.AssertExpectations(test.T())
}

// A refused bar anywhere in the batch stores none of it, so the shared price
// cache never keeps a gap.
func (test *IngestCommandTest) TestIngestStoresNothingWhenABarIsRefused() {
	message := new(stream.MockMessage)
	message.On("MustGet", stream.DBDep).Return(test.db)
	message.On("MustGet", dependency.MarketDataDep).Return(dividendFree{test.marketdata})

	test.marketdata.On("GetAsset", mock.Anything, "XYZ").Return(&pb.Asset{Symbol: "XYZ", Name: "XYZ Corp"}, nil)
	test.marketdata.On("GetOHLC", mock.Anything, "XYZ", mock.Anything, mock.Anything).Return(
		[]*pb.OHLC{
			{Timestamp: timestamppb.New(time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)), Open: 10, High: 11, Low: 9, Close: 10.5, Volume: 100},
			{Timestamp: timestamppb.New(time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC)), Open: 10, High: 11, Low: 9, Close: 0, Volume: 100},
			{Timestamp: timestamppb.New(time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)), Open: 10, High: 11, Low: 9, Close: 10.5, Volume: 100},
		},
		nil,
	)
	message.On("ParsePayload", &fs.IngestCommand{}).Return(nil).Run(func(args mock.Arguments) {
		command := args.Get(0).(*fs.IngestCommand)
		command.Provider = "yahoo"
		command.Symbols = []string{"XYZ"}
		command.Start = "2026-05-01"
		end := "2026-05-03"
		command.End = &end
	})

	err := command.Ingest(context.Background(), message)
	test.Require().EqualError(err, "yahoo sent an invalid price for XYZ on 2026-05-02 (0, negative or not a number)")

	var rows int
	test.Require().NoError(test.db.QueryRow(context.Background(),
		`SELECT count(*) FROM ohlc WHERE symbol = 'XYZ'`).Scan(&rows))
	test.Zero(rows, "no bar of a refused batch may be stored")
}

func (test *IngestCommandTest) TestAlpacaFailsBeforeDownload() {
	test.T().Setenv(environment.TradingProfilesFile, filepath.Join(test.T().TempDir(), "profiles.yaml"))

	message := new(stream.MockMessage)
	message.On("MustGet", stream.DBDep).Return(test.db)
	message.On("MustGet", dependency.MarketDataDep).Return(dividendFree{test.marketdata})
	message.On("ParsePayload", &fs.IngestCommand{}).Return(nil).Run(func(args mock.Arguments) {
		command := args.Get(0).(*fs.IngestCommand)
		// yahoo routes GetOHLC to the mock supplier.
		command.Provider = "yahoo"
		command.Symbols = []string{"NEW123"}
		command.Start = internal_pb.DateToDateString(test.storedOHLCStart)
		end := internal_pb.DateToDateString(test.storedOHLCEnd)
		command.End = &end
		command.Provider = "alpaca"
	})

	err := command.Ingest(context.Background(), message)
	test.Require().Error(err)
	test.marketdata.AssertNotCalled(test.T(), "GetOHLC", mock.Anything, mock.Anything, mock.Anything)
	test.marketdata.AssertNotCalled(test.T(), "GetAsset", mock.Anything)
}

// aaplAdjusted is Yahoo's split-adjusted AAPL around the 4:1 split on
// 2020-08-31 (recorded chart response, as in yahoo_split_test.go).
func aaplAdjusted() []*pb.OHLC {
	bar := func(day int, month time.Month, o, h, l, c float64, v uint64) *pb.OHLC {
		return &pb.OHLC{
			Timestamp: timestamppb.New(time.Date(2020, month, day, 0, 0, 0, 0, time.UTC)),
			Open:      o, High: h, Low: l, Close: c, Volume: v,
		}
	}
	return []*pb.OHLC{
		bar(27, time.August, 127.1425, 127.485, 123.8325, 125.01, 155552400),
		bar(28, time.August, 126.0125, 126.4425, 124.5775, 124.8075, 187630000),
		bar(31, time.August, 127.58, 131.0, 126.0, 129.04, 225702700),
		bar(1, time.September, 132.76, 134.8, 130.53, 134.18, 151948100),
	}
}

func sameDay(want time.Time) interface{} {
	return mock.MatchedBy(func(got time.Time) bool {
		return got.UTC().Format("2006-01-02") == want.Format("2006-01-02")
	})
}

func (test *IngestCommandTest) ingestAAPL(start, end string) error {
	message := new(stream.MockMessage)
	message.On("MustGet", stream.DBDep).Return(test.db)
	message.On("MustGet", dependency.MarketDataDep).Return(dividendFree{test.marketdata})
	message.On("ParsePayload", &fs.IngestCommand{}).Return(nil).Run(func(args mock.Arguments) {
		command := args.Get(0).(*fs.IngestCommand)
		command.Provider = "yahoo"
		command.Symbols = []string{"AAPL"}
		command.Start = start
		command.End = &end
	})
	return command.Ingest(context.Background(), message)
}

// An incremental fetch after a split sees the overlap day re-scaled and
// replaces the whole stored range, so no day jumps by the split ratio.
func (test *IngestCommandTest) TestIncrementalFetchRescalesAfterSplit() {
	ctx := context.Background()
	test.Require().NoError(test.assets.Store(ctx, "AAPL", "Apple Inc."))
	adjusted := aaplAdjusted()
	// Stored before the split, on the old scale.
	for _, b := range adjusted[:2] {
		test.Require().NoError(test.ohlc.Store(ctx, "yahoo", "AAPL", b.Timestamp.AsTime(),
			b.Open*4, b.High*4, b.Low*4, b.Close*4, b.Volume/4))
	}

	aug27 := time.Date(2020, 8, 27, 0, 0, 0, 0, time.UTC)
	aug28 := time.Date(2020, 8, 28, 0, 0, 0, 0, time.UTC)
	// The incremental fetch starts on the last stored day.
	test.marketdata.On("GetOHLC", mock.Anything, "AAPL", sameDay(aug28), mock.Anything).Return(adjusted[1:], nil).Once()
	test.marketdata.On("GetOHLC", mock.Anything, "AAPL", sameDay(aug27), mock.Anything).Return(adjusted, nil).Once()

	test.Require().NoError(test.ingestAAPL("2020-08-27", "2020-09-01"))
	test.marketdata.AssertExpectations(test.T())

	bars, err := test.ohlc.List(ctx, "yahoo", []string{"AAPL"}, time.Date(2020, 9, 1, 0, 0, 0, 0, time.UTC), 0)
	test.Require().NoError(err)
	test.Require().Len(bars, len(adjusted))
	for i := 1; i < len(bars); i++ {
		move := math.Abs(bars[i].Close/bars[i-1].Close - 1)
		test.LessOrEqual(move, 0.20, "%s", bars[i].Timestamp.AsTime().Format("2006-01-02"))
	}
	closeAug28, err := test.ohlc.Close(ctx, "yahoo", "AAPL", aug28)
	test.Require().NoError(err)
	test.InDelta(124.8075, closeAug28, 0.0001)
}

// When the overlap day matches, only the new days are fetched and stored.
func (test *IngestCommandTest) TestIncrementalFetchAppendsWhenOverlapMatches() {
	ctx := context.Background()
	test.Require().NoError(test.assets.Store(ctx, "AAPL", "Apple Inc."))
	adjusted := aaplAdjusted()
	for _, b := range adjusted[:2] {
		test.Require().NoError(test.ohlc.Store(ctx, "yahoo", "AAPL", b.Timestamp.AsTime(),
			b.Open, b.High, b.Low, b.Close, b.Volume))
	}

	aug28 := time.Date(2020, 8, 28, 0, 0, 0, 0, time.UTC)
	test.marketdata.On("GetOHLC", mock.Anything, "AAPL", sameDay(aug28), mock.Anything).Return(adjusted[1:], nil).Once()

	test.Require().NoError(test.ingestAAPL("2020-08-27", "2020-09-01"))
	test.marketdata.AssertExpectations(test.T())
	test.marketdata.AssertNumberOfCalls(test.T(), "GetOHLC", 1)

	first, last, ok, err := test.ohlc.Range(ctx, "yahoo", "AAPL")
	test.Require().NoError(err)
	test.True(ok)
	test.Equal("2020-08-27", first.Format("2006-01-02"))
	test.Equal("2020-09-01", last.Format("2006-01-02"))
}

// A requested range that is already stored is not fetched again.
func (test *IngestCommandTest) TestIngestSkipsStoredRange() {
	ctx := context.Background()
	test.Require().NoError(test.assets.Store(ctx, "AAPL", "Apple Inc."))
	for _, b := range aaplAdjusted() {
		test.Require().NoError(test.ohlc.Store(ctx, "yahoo", "AAPL", b.Timestamp.AsTime(),
			b.Open, b.High, b.Low, b.Close, b.Volume))
	}

	test.Require().NoError(test.ingestAAPL("2020-08-27", "2020-08-31"))
	test.marketdata.AssertNotCalled(test.T(), "GetOHLC", mock.Anything, mock.Anything, mock.Anything)
}

// storeAAPLPreSplit stores the first n recorded bars on the pre-split scale.
func (test *IngestCommandTest) storeAAPLPreSplit(n int) {
	ctx := context.Background()
	test.Require().NoError(test.assets.Store(ctx, "AAPL", "Apple Inc."))
	for _, b := range aaplAdjusted()[:n] {
		test.Require().NoError(test.ohlc.Store(ctx, "yahoo", "AAPL", b.Timestamp.AsTime(),
			b.Open*4, b.High*4, b.Low*4, b.Close*4, b.Volume/4))
	}
}

// requireNoSplitSeam checks that no stored day moves more than 20% and that
// 2020-08-28 is on the split-adjusted scale.
func (test *IngestCommandTest) requireNoSplitSeam() {
	ctx := context.Background()
	bars, err := test.ohlc.List(ctx, "yahoo", []string{"AAPL"}, time.Date(2020, 9, 1, 0, 0, 0, 0, time.UTC), 0)
	test.Require().NoError(err)
	test.Require().Len(bars, len(aaplAdjusted()))
	for i := 1; i < len(bars); i++ {
		move := math.Abs(bars[i].Close/bars[i-1].Close - 1)
		test.LessOrEqual(move, 0.20, "%s", bars[i].Timestamp.AsTime().Format("2006-01-02"))
	}
	closeAug28, err := test.ohlc.Close(ctx, "yahoo", "AAPL", time.Date(2020, 8, 28, 0, 0, 0, 0, time.UTC))
	test.Require().NoError(err)
	test.InDelta(124.8075, closeAug28, 0.0001)
}

// When the incremental fetch leaves out the overlap day, the scale of the
// stored history can not be checked, so the whole range is fetched again and
// replaced instead of appended next to old-scale rows.
func (test *IngestCommandTest) TestIncrementalFetchWithoutOverlapReplaces() {
	test.storeAAPLPreSplit(2)
	adjusted := aaplAdjusted()

	aug27 := time.Date(2020, 8, 27, 0, 0, 0, 0, time.UTC)
	aug28 := time.Date(2020, 8, 28, 0, 0, 0, 0, time.UTC)
	// The provider returns only 08-31 and 09-01 for the incremental fetch.
	test.marketdata.On("GetOHLC", mock.Anything, "AAPL", sameDay(aug28), mock.Anything).Return(adjusted[2:], nil).Once()
	test.marketdata.On("GetOHLC", mock.Anything, "AAPL", sameDay(aug27), mock.Anything).Return(adjusted, nil).Once()

	test.Require().NoError(test.ingestAAPL("2020-08-27", "2020-09-01"))
	test.marketdata.AssertExpectations(test.T())
	test.requireNoSplitSeam()
}

// A bad fetched close on the overlap day is not a match either.
func (test *IngestCommandTest) TestIncrementalFetchWithBadOverlapCloseReplaces() {
	test.storeAAPLPreSplit(2)
	adjusted := aaplAdjusted()
	badOverlap := proto.Clone(adjusted[1]).(*pb.OHLC)
	badOverlap.Close = 0

	aug27 := time.Date(2020, 8, 27, 0, 0, 0, 0, time.UTC)
	aug28 := time.Date(2020, 8, 28, 0, 0, 0, 0, time.UTC)
	test.marketdata.On("GetOHLC", mock.Anything, "AAPL", sameDay(aug28), mock.Anything).
		Return([]*pb.OHLC{badOverlap, adjusted[2], adjusted[3]}, nil).Once()
	test.marketdata.On("GetOHLC", mock.Anything, "AAPL", sameDay(aug27), mock.Anything).Return(adjusted, nil).Once()

	test.Require().NoError(test.ingestAAPL("2020-08-27", "2020-09-01"))
	test.marketdata.AssertExpectations(test.T())
	test.requireNoSplitSeam()
}

// A split refetch with one bad bar is refused before ReplaceRange runs: the
// stored old-scale rows stay exactly as they were, no day goes missing, and
// the InvalidPriceError names the provider, symbol and day.
func (test *IngestCommandTest) TestRescaleWithBadRefetchedBarKeepsStoredRows() {
	ctx := context.Background()
	test.storeAAPLPreSplit(2)
	adjusted := aaplAdjusted()

	type row struct{ o, h, l, c float64 }
	readRows := func() map[string]row {
		rows, err := test.db.Query(ctx,
			`SELECT time, open::float8, high::float8, low::float8, close::float8 FROM ohlc
			 WHERE provider = 'yahoo' AND symbol = 'AAPL' ORDER BY time`)
		test.Require().NoError(err)
		defer rows.Close()
		got := map[string]row{}
		for rows.Next() {
			var t time.Time
			var r row
			test.Require().NoError(rows.Scan(&t, &r.o, &r.h, &r.l, &r.c))
			got[t.UTC().Format("2006-01-02")] = r
		}
		test.Require().NoError(rows.Err())
		return got
	}
	before := readRows()
	test.Require().Len(before, 2)

	// The full refetch is split-adjusted but 2020-08-28 comes back with close 0.
	refetch := make([]*pb.OHLC, len(adjusted))
	for i, b := range adjusted {
		refetch[i] = proto.Clone(b).(*pb.OHLC)
	}
	refetch[1].Close = 0

	aug27 := time.Date(2020, 8, 27, 0, 0, 0, 0, time.UTC)
	aug28 := time.Date(2020, 8, 28, 0, 0, 0, 0, time.UTC)
	// The overlap close moved by the split ratio, so ingest refetches the range.
	test.marketdata.On("GetOHLC", mock.Anything, "AAPL", sameDay(aug28), mock.Anything).Return(adjusted[1:], nil).Once()
	test.marketdata.On("GetOHLC", mock.Anything, "AAPL", sameDay(aug27), mock.Anything).Return(refetch, nil).Once()

	err := test.ingestAAPL("2020-08-27", "2020-09-01")
	test.Require().ErrorIs(err, repository.ErrInvalidPrice)
	var refused *repository.InvalidPriceError
	test.Require().ErrorAs(err, &refused)
	test.Equal("close", refused.Field)
	test.Require().EqualError(err, "yahoo sent an invalid price for AAPL on 2020-08-28 (0, negative or not a number)")
	test.marketdata.AssertExpectations(test.T())

	after := readRows()
	test.Equal(before, after, "stored rows must keep their old values")
	for _, day := range []string{"2020-08-27", "2020-08-28"} {
		test.Contains(after, day, "no stored day may go missing")
	}
	test.InDelta(adjusted[1].Close*4, after["2020-08-28"].c, 0.0001)
}

// A first ingest has nothing to re-scale: one fetch from start, inserted.
func (test *IngestCommandTest) TestFirstIngestInserts() {
	test.Require().NoError(test.assets.Store(context.Background(), "AAPL", "Apple Inc."))
	aug27 := time.Date(2020, 8, 27, 0, 0, 0, 0, time.UTC)
	test.marketdata.On("GetOHLC", mock.Anything, "AAPL", sameDay(aug27), mock.Anything).Return(aaplAdjusted(), nil).Once()

	test.Require().NoError(test.ingestAAPL("2020-08-27", "2020-09-01"))
	test.marketdata.AssertExpectations(test.T())
	test.marketdata.AssertNumberOfCalls(test.T(), "GetOHLC", 1)
	test.requireNoSplitSeam()
}

// A requested start before the stored history fetches through the last stored
// day and replaces it, so stored days are not left on the old scale.
func (test *IngestCommandTest) TestIngestBeforeStoredHistoryReplaces() {
	ctx := context.Background()
	test.Require().NoError(test.assets.Store(ctx, "AAPL", "Apple Inc."))
	adjusted := aaplAdjusted()
	for _, b := range adjusted[1:] {
		test.Require().NoError(test.ohlc.Store(ctx, "yahoo", "AAPL", b.Timestamp.AsTime(),
			b.Open*4, b.High*4, b.Low*4, b.Close*4, b.Volume/4))
	}

	aug27 := time.Date(2020, 8, 27, 0, 0, 0, 0, time.UTC)
	sep1 := time.Date(2020, 9, 1, 0, 0, 0, 0, time.UTC)
	test.marketdata.On("GetOHLC", mock.Anything, "AAPL", sameDay(aug27), mock.MatchedBy(func(end *time.Time) bool {
		return end != nil && end.Format("2006-01-02") == sep1.Format("2006-01-02")
	})).Return(adjusted, nil).Once()

	test.Require().NoError(test.ingestAAPL("2020-08-27", "2020-08-28"))
	test.marketdata.AssertExpectations(test.T())
	test.marketdata.AssertNumberOfCalls(test.T(), "GetOHLC", 1)
	test.requireNoSplitSeam()
}

// A re-scale refetch that returns no bars fails and names the symbol, instead
// of reporting success with the old-scale rows still stored.
func (test *IngestCommandTest) TestRescaleRefetchWithNoBarsFails() {
	test.storeAAPLPreSplit(2)
	adjusted := aaplAdjusted()

	aug27 := time.Date(2020, 8, 27, 0, 0, 0, 0, time.UTC)
	aug28 := time.Date(2020, 8, 28, 0, 0, 0, 0, time.UTC)
	test.marketdata.On("GetOHLC", mock.Anything, "AAPL", sameDay(aug28), mock.Anything).Return(adjusted[1:], nil).Once()
	test.marketdata.On("GetOHLC", mock.Anything, "AAPL", sameDay(aug27), mock.Anything).Return([]*pb.OHLC{}, nil).Once()

	err := test.ingestAAPL("2020-08-27", "2020-09-01")
	test.Require().ErrorIs(err, repository.ErrNoBars)
	test.Require().EqualError(err, "yahoo returned no prices for AAPL between 2020-08-27 and 2020-09-01. "+
		"Check that AAPL traded between those dates, try again later, or use another provider with --provider.")
	test.marketdata.AssertExpectations(test.T())

	closeAug28, err := test.ohlc.Close(context.Background(), "yahoo", "AAPL", aug28)
	test.Require().NoError(err)
	test.InDelta(adjusted[1].Close*4, closeAug28, 0.0001, "stored rows stay as they were")
}

// A database error reaches the task state as a fixed sentence; the SQL and
// driver text stay in the server log.
func (test *IngestCommandTest) TestIngestStoreErrorIsAFixedSentence() {
	ctx := context.Background()
	const leak = "dial postgres://admin:hunter2@10.0.0.5:5432/prod: INSERT INTO ohlc"
	_, err := test.db.Exec(ctx, `
CREATE OR REPLACE FUNCTION fail_ingest() RETURNS trigger AS $$
BEGIN RAISE EXCEPTION '`+leak+`'; END $$ LANGUAGE plpgsql;
CREATE TRIGGER fail_ingest BEFORE INSERT ON ohlc FOR EACH ROW EXECUTE FUNCTION fail_ingest();`)
	test.Require().NoError(err)
	test.Require().NoError(test.assets.Store(ctx, "AAPL", "Apple Inc."))
	test.marketdata.On("GetOHLC", mock.Anything, "AAPL", mock.Anything, mock.Anything).Return(aaplAdjusted(), nil).Once()

	err = test.ingestAAPL("2020-08-27", "2020-09-01")
	test.Require().EqualError(err, "Couldn't store prices for AAPL. Try again in a few minutes. "+
		"If it keeps failing, report it at https://github.com/lhjnilsson/foreverbull/issues.")
	var stored *repository.StoreError
	test.Require().ErrorAs(err, &stored)
	test.Contains(stored.Err.Error(), "hunter2", "the cause is kept for the log")
}

// A DB error on one row rolls the symbol's whole batch back, as it does on
// the download RPC: no gap is left in the shared price cache. The asset row
// is written before the bars transaction and stays.
func (test *IngestCommandTest) TestIngestRollsBackSymbolOnDBError() {
	ctx := context.Background()
	_, err := test.db.Exec(ctx, `
CREATE OR REPLACE FUNCTION fail_on_bad_row() RETURNS trigger AS $$
BEGIN
	IF NEW.close = 13.37 THEN
		RAISE EXCEPTION 'forced failure on the second row';
	END IF;
	RETURN NEW;
END $$ LANGUAGE plpgsql;
CREATE TRIGGER fail_on_bad_row BEFORE INSERT ON ohlc FOR EACH ROW EXECUTE FUNCTION fail_on_bad_row();`)
	test.Require().NoError(err)

	message := new(stream.MockMessage)
	message.On("MustGet", stream.DBDep).Return(test.db)
	message.On("MustGet", dependency.MarketDataDep).Return(dividendFree{test.marketdata})
	test.marketdata.On("GetAsset", mock.Anything, "XYZ").Return(&pb.Asset{Symbol: "XYZ", Name: "XYZ Corp"}, nil)
	test.marketdata.On("GetOHLC", mock.Anything, "XYZ", mock.Anything, mock.Anything).Return(
		[]*pb.OHLC{
			{Timestamp: timestamppb.New(time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)), Open: 10, High: 14, Low: 9, Close: 10.5, Volume: 100},
			{Timestamp: timestamppb.New(time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC)), Open: 10, High: 14, Low: 9, Close: 13.37, Volume: 100},
			{Timestamp: timestamppb.New(time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)), Open: 10, High: 14, Low: 9, Close: 10.5, Volume: 100},
		},
		nil,
	)
	message.On("ParsePayload", &fs.IngestCommand{}).Return(nil).Run(func(args mock.Arguments) {
		command := args.Get(0).(*fs.IngestCommand)
		command.Provider = "yahoo"
		command.Symbols = []string{"XYZ"}
		command.Start = "2026-05-01"
		end := "2026-05-03"
		command.End = &end
	})

	err = command.Ingest(ctx, message)
	test.Require().EqualError(err, "Couldn't store prices for XYZ. Try again in a few minutes. "+
		"If it keeps failing, report it at https://github.com/lhjnilsson/foreverbull/issues.")
	test.NotContains(err.Error(), "forced failure", "the DB text stays in the log")

	var rows int
	test.Require().NoError(test.db.QueryRow(ctx,
		`SELECT count(*) FROM ohlc WHERE symbol = 'XYZ'`).Scan(&rows))
	test.Zero(rows, "a DB error on one row must leave none of the symbol's bars")

	_, err = test.assets.Get(ctx, "XYZ")
	test.Require().NoError(err, "the asset row is outside the bars transaction")
}

const crumbURL = "https://query2.finance.yahoo.com/v1/finance/chart/XYZ?crumb=s3cr3tCrumb#frag"

func yahooURLError() error {
	return fmt.Errorf("yahoo: %w", &url.Error{Op: "Get", URL: crumbURL, Err: errors.New("dial tcp: i/o timeout")})
}

// Ingest's error is published verbatim as the NATS task state and logged by
// the orchestration, so a Yahoo URL in it must not carry the session crumb.
func (test *IngestCommandTest) TestIngestKeepsCrumbOutOfTaskState() {
	for _, tc := range []struct {
		name   string
		symbol string
		setup  func(md *supplier.MockMarketdata)
		prefix string
	}{
		{
			name:   "get asset",
			symbol: "XYZ",
			setup: func(md *supplier.MockMarketdata) {
				md.On("GetAsset", mock.Anything, "XYZ").Return(nil, yahooURLError())
			},
			prefix: "error getting asset: ",
		},
		{
			name:   "get ohlc",
			symbol: test.storedAsset.Symbol,
			setup: func(md *supplier.MockMarketdata) {
				md.On("GetOHLC", mock.Anything, test.storedAsset.Symbol, mock.Anything, mock.Anything).Return(nil, yahooURLError())
			},
			prefix: "error getting OHLC: ",
		},
	} {
		test.Run(tc.name, func() {
			md := supplier.NewMockMarketdata(test.T())
			tc.setup(md)
			message := new(stream.MockMessage)
			message.On("MustGet", stream.DBDep).Return(test.db)
			message.On("MustGet", dependency.MarketDataDep).Return(dividendFree{md})
			message.On("ParsePayload", &fs.IngestCommand{}).Return(nil).Run(func(args mock.Arguments) {
				command := args.Get(0).(*fs.IngestCommand)
				command.Provider = "yahoo"
				command.Symbols = []string{tc.symbol}
				command.Start = "2026-05-01"
			})

			err := command.Ingest(context.Background(), message)

			test.Require().EqualError(err, tc.prefix+
				`yahoo: Get "https://query2.finance.yahoo.com/v1/finance/chart/XYZ": dial tcp: i/o timeout`)
			test.NotContains(err.Error(), "s3cr3tCrumb")
		})
	}
}

// ingestSymbols runs Ingest for symbols with the mock supplier.
func (test *IngestCommandTest) ingestSymbols(md *supplier.MockMarketdata, symbols ...string) error {
	return test.ingestSymbolsVia("yahoo", md, symbols...)
}

func (test *IngestCommandTest) ingestSymbolsVia(provider string, md *supplier.MockMarketdata, symbols ...string) error {
	message := new(stream.MockMessage)
	message.On("MustGet", stream.DBDep).Return(test.db)
	message.On("MustGet", dependency.MarketDataDep).Return(dividendFree{md})
	message.On("ParsePayload", &fs.IngestCommand{}).Return(nil).Run(func(args mock.Arguments) {
		command := args.Get(0).(*fs.IngestCommand)
		command.Provider = provider
		command.Symbols = symbols
		command.Start = "2026-05-01"
	})
	return command.Ingest(context.Background(), message)
}

// A requested symbol outside the ticker pattern refuses the whole ingest
// before any provider call or write; the refusal names its position only.
func (test *IngestCommandTest) TestIngestRefusesInvalidRequestSymbol() {
	for _, bad := range []string{"", "AA PL", "XYZ\n", "<b>x</b>", strings.Repeat("A", 33)} {
		md := supplier.NewMockMarketdata(test.T())

		err := test.ingestSymbols(md, "XYZ", bad, "ABC")

		test.Require().EqualError(err, "symbol 2 of 3 isn't a valid ticker (use 1 to 32 letters, digits, or . ^ = _ -)", "%q", bad)
		md.AssertNotCalled(test.T(), "GetAsset", mock.Anything)
		_, err = test.assets.Get(context.Background(), "XYZ")
		test.Require().ErrorIs(err, pgx.ErrNoRows, "nothing is stored before the check")
	}
}

// A provider ticker outside the pattern is refused before the asset row or
// any bar is stored, with a sentence that names only the requested symbol.
//
// The asset lookup is always Yahoo, so the text names yahoo even when the
// bars come from stooq, and it is the whole error with no prefix.
func (test *IngestCommandTest) TestIngestRefusesInvalidProviderSymbol() {
	const providerSymbol = "XYZ\x1b[31m <b>"
	for _, provider := range []string{"yahoo", "stooq"} {
		md := supplier.NewMockMarketdata(test.T())
		md.On("GetAsset", mock.Anything, "XYZ").Return(&pb.Asset{Symbol: providerSymbol, Name: "XYZ Corp"}, nil)

		err := test.ingestSymbolsVia(provider, md, "XYZ")

		test.Require().EqualError(err, "yahoo returned an invalid ticker for XYZ, so its prices weren't imported. "+
			"Check the symbol or leave it out, and report it at https://github.com/lhjnilsson/foreverbull/issues.", provider)
		md.AssertNotCalled(test.T(), "GetOHLC", mock.Anything, mock.Anything, mock.Anything)
		var assets int
		test.Require().NoError(test.db.QueryRow(context.Background(),
			`SELECT count(*) FROM asset WHERE symbol = $1`, providerSymbol).Scan(&assets))
		test.Zero(assets, "a refused provider ticker must not be stored")
	}
}
