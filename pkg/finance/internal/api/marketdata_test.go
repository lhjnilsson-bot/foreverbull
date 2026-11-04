package api_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lhjnilsson/foreverbull/internal/environment"
	internalGrpc "github.com/lhjnilsson/foreverbull/internal/grpc"
	"github.com/lhjnilsson/foreverbull/internal/test_helper"
	financeapi "github.com/lhjnilsson/foreverbull/pkg/finance/api"
	"github.com/lhjnilsson/foreverbull/pkg/finance/internal/api"
	"github.com/lhjnilsson/foreverbull/pkg/finance/internal/repository"
	"github.com/lhjnilsson/foreverbull/pkg/finance/internal/suppliers/marketdata"
	"github.com/lhjnilsson/foreverbull/pkg/finance/supplier"
	internal_pb "github.com/lhjnilsson/foreverbull/pkg/pb"
	pb "github.com/lhjnilsson/foreverbull/pkg/pb/finance"
	servicePb "github.com/lhjnilsson/foreverbull/pkg/pb/finance/service"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/suite"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type MarketdataTest struct {
	suite.Suite

	pgx        *pgxpool.Pool
	marketdata *supplier.MockMarketdata

	listener *bufconn.Listener
	server   *grpc.Server
	client   servicePb.MarketdataClient
}

func TestMarketData(t *testing.T) {
	suite.Run(t, new(MarketdataTest))
}

func (suite *MarketdataTest) SetupSuite() {
	test_helper.SetupEnvironment(suite.T(), &test_helper.Containers{
		Postgres: true,
	})
}

func (suite *MarketdataTest) TearDownSuite() {
}

func (suite *MarketdataTest) SetupTest() {
	var err error
	suite.pgx, err = pgxpool.New(context.Background(), environment.GetPostgresURL())
	suite.Require().NoError(err)
	suite.Require().NoError(repository.Recreate(context.TODO(), suite.pgx))
	suite.marketdata = supplier.NewMockMarketdata(suite.T())

	suite.listener = bufconn.Listen(1024 * 1024)
	suite.server, err = internalGrpc.NewServer()
	suite.Require().NoError(err)
	server := api.NewMarketdataServer(suite.pgx, dividendFree{suite.marketdata})
	servicePb.RegisterMarketdataServer(suite.server, server)

	// Serve gets this test's server and listener, not the suite fields the
	// next SetupTest overwrites while this goroutine may still be starting.
	grpcServer, listener := suite.server, suite.listener
	go func() {
		_ = grpcServer.Serve(listener)
	}()

	resolver.SetDefaultScheme("passthrough")

	conn, err := grpc.NewClient(suite.listener.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(_ context.Context, _ string) (net.Conn, error) {
			return suite.listener.Dial()
		}),
	)
	if err != nil {
		log.Printf("error connecting to server: %v", err)
	}

	suite.client = servicePb.NewMarketdataClient(conn)
}

func (suite *MarketdataTest) TestGetAsset() {
	suite.marketdata.On("GetAsset", mock.Anything, "AAPL").Return(&pb.Asset{Symbol: "AAPL", Name: "Apple Inc."}, nil)

	req := &servicePb.GetAssetRequest{Identifier: &servicePb.GetAssetRequest_Symbol{Symbol: "AAPL"}}
	rsp, err := suite.client.GetAsset(context.Background(), req)
	suite.Require().NoError(err)
	suite.Require().NotNil(rsp)
	suite.Equal("AAPL", rsp.Asset.Symbol)
	suite.Equal("Apple Inc.", rsp.Asset.Name)
}

// The provider served the ISIN lookup and has no ISIN for the equity. The
// call goes through the server's interceptors, and the caller gets NotFound
// with the shared no-ISIN text naming only the requested symbol.
func (suite *MarketdataTest) TestGetAssetNoISINIsNotFound() {
	suite.marketdata.On("GetAsset", mock.Anything, "XYZ").Return(nil, fmt.Errorf("error getting isin for symbol XYZ: %w",
		fmt.Errorf("ISIN not found for symbol XYZ: %w", marketdata.ErrNoISIN)))

	req := &servicePb.GetAssetRequest{Identifier: &servicePb.GetAssetRequest_Symbol{Symbol: "XYZ"}}
	rsp, err := suite.client.GetAsset(context.Background(), req)
	suite.Require().Error(err)
	suite.Nil(rsp)
	st, ok := status.FromError(err)
	suite.Require().True(ok)
	suite.Equal(codes.NotFound, st.Code())
	suite.Equal("no ISIN for XYZ", st.Message())
	suite.True(financeapi.IsNoISIN(st.Message()))
}

// A failed ISIN request is not a no-ISIN answer: it keeps the generic
// failure code, and neither the URL nor its crumb reaches the caller.
func (suite *MarketdataTest) TestGetAssetFailedISINRequestIsNotNotFound() {
	suite.marketdata.On("GetAsset", mock.Anything, "XYZ").Return(nil, fmt.Errorf("error getting isin for symbol XYZ: %w",
		&url.Error{
			Op:  "Get",
			URL: "https://query2.finance.yahoo.com/v1/finance/search?q=XYZ&crumb=s3cr3tcrumb",
			Err: errors.New("503 Service Unavailable"),
		}))

	req := &servicePb.GetAssetRequest{Identifier: &servicePb.GetAssetRequest_Symbol{Symbol: "XYZ"}}
	_, err := suite.client.GetAsset(context.Background(), req)
	suite.Require().Error(err)
	st, ok := status.FromError(err)
	suite.Require().True(ok)
	suite.NotEqual(codes.NotFound, st.Code())
	suite.Equal(codes.Internal, st.Code())
	suite.False(financeapi.IsNoISIN(st.Message()))
	for _, raw := range []string{"crumb", "s3cr3tcrumb", "http", "yahoo.com", "search?q"} {
		suite.NotContains(st.Message(), raw)
	}
}

func (suite *MarketdataTest) TestGetIndex() {
	suite.marketdata.On("GetIndex", mock.Anything, "^GDAXI").Return([]*pb.Asset{
		{Symbol: "ADS.DE", Name: "Adidas AG"},
	}, nil)

	req := &servicePb.GetIndexRequest{Symbol: "^GDAXI"}
	rsp, err := suite.client.GetIndex(context.Background(), req)
	suite.Require().NoError(err)
	suite.Require().NotNil(rsp)
	suite.NotEmpty(rsp.Assets)
}

func (suite *MarketdataTest) TestDownloadHistoricalData() {
	suite.marketdata.On("GetAsset", mock.Anything, "AAPL").Return(&pb.Asset{Symbol: "AAPL", Name: "Apple Inc."}, nil)
	suite.marketdata.On("GetAsset", mock.Anything, "NOCORP").Return(nil, errors.New("not found"))
	suite.marketdata.On("GetOHLC", mock.Anything, "AAPL", mock.Anything, mock.Anything).Return([]*pb.OHLC{
		{
			Timestamp: timestamppb.Now(),
			Open:      1,
			High:      2,
			Low:       0.5,
			Close:     1.5,
			Volume:    100,
		},
	}, nil)

	req := &servicePb.DownloadHistoricalDataRequest{
		Symbols:   []string{"AAPL", "NOCORP"},
		StartDate: &internal_pb.Date{Year: 2020, Month: 1, Day: 1},
		EndDate:   &internal_pb.Date{Year: 2024, Month: 06, Day: 30},
		// yahoo routes GetOHLC to the injected mock.
		Provider: "yahoo",
	}
	rsp, err := suite.client.DownloadHistoricalData(context.Background(), req)
	suite.Require().NoError(err)

	result := make(map[string]servicePb.DownloadHistoricalDataResponse_Status)
	for {
		msg, err := rsp.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			suite.Require().NoError(err)
			break
		}
		suite.Require().NoError(err)
		result[msg.GetSymbol()] = msg.GetStatus()
	}
	suite.Equal(servicePb.DownloadHistoricalDataResponse_STATUS_OK, result["AAPL"])
	suite.Equal(servicePb.DownloadHistoricalDataResponse_STATUS_ERROR, result["NOCORP"])
}

// A refused bar fails the symbol once and stores none of its bars, so the
// shared price cache never keeps a gap.
func (suite *MarketdataTest) TestDownloadHistoricalDataStoresNothingWhenABarIsRefused() {
	suite.marketdata.On("GetAsset", mock.Anything, "XYZ").Return(&pb.Asset{Symbol: "XYZ", Name: "XYZ Corp"}, nil)
	suite.marketdata.On("GetOHLC", mock.Anything, "XYZ", mock.Anything, mock.Anything).Return([]*pb.OHLC{
		{Timestamp: timestamppb.New(mustTime("2026-05-01")), Open: 10, High: 11, Low: 9, Close: 10.5, Volume: 100},
		{Timestamp: timestamppb.New(mustTime("2026-05-02")), Open: 0, High: 11, Low: 9, Close: 10.5, Volume: 100},
		{Timestamp: timestamppb.New(mustTime("2026-05-03")), Open: 10, High: 11, Low: 9, Close: 10.5, Volume: 100},
	}, nil)

	rsp, err := suite.client.DownloadHistoricalData(context.Background(), &servicePb.DownloadHistoricalDataRequest{
		Symbols:   []string{"XYZ"},
		StartDate: &internal_pb.Date{Year: 2026, Month: 5, Day: 1},
		EndDate:   &internal_pb.Date{Year: 2026, Month: 5, Day: 3},
		Provider:  "yahoo",
	})
	suite.Require().NoError(err)

	var msgs []*servicePb.DownloadHistoricalDataResponse
	for {
		msg, err := rsp.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		suite.Require().NoError(err)
		msgs = append(msgs, msg)
	}
	suite.Require().Len(msgs, 1, "the symbol fails once")
	suite.Equal(servicePb.DownloadHistoricalDataResponse_STATUS_ERROR, msgs[0].GetStatus())
	suite.Equal("yahoo sent an invalid price for XYZ on 2026-05-02 (0, negative or not a number)", msgs[0].GetError())

	var rows int
	suite.Require().NoError(suite.pgx.QueryRow(context.Background(),
		`SELECT count(*) FROM ohlc WHERE symbol = 'XYZ'`).Scan(&rows))
	suite.Zero(rows, "no bar of a refused batch may be stored")
}

// A provider bar with no price inside the range fails the
// symbol with its days, under the asked symbol, after one retry. Nothing is
// stored and no later ticker is tried: the ticker has bars, with a gap.
func (suite *MarketdataTest) TestDownloadHistoricalDataFailsOnAMissingPriceInside() {
	suite.marketdata.On("GetAsset", mock.Anything, "BRKB").Return(&pb.Asset{Symbol: "BRK-B", Name: "Berkshire Hathaway"}, nil)
	suite.marketdata.On("GetOHLC", mock.Anything, "BRK-B", mock.Anything, mock.Anything).Return(nil, &marketdata.MissingPriceError{
		Provider: "yahoo", Symbol: "BRK-B", Days: []time.Time{mustTime("2024-02-14"), mustTime("2024-02-20")},
	}).Twice()

	rsp, err := suite.client.DownloadHistoricalData(context.Background(), &servicePb.DownloadHistoricalDataRequest{
		Symbols:   []string{"BRKB"},
		StartDate: &internal_pb.Date{Year: 2024, Month: 1, Day: 1},
		EndDate:   &internal_pb.Date{Year: 2024, Month: 3, Day: 31},
		Provider:  "yahoo",
	})
	suite.Require().NoError(err)

	msgs := suite.recvAll(rsp)
	suite.Require().Len(msgs, 1, "the symbol fails once")
	suite.Equal("BRKB", msgs[0].GetSymbol())
	suite.Equal(servicePb.DownloadHistoricalDataResponse_STATUS_ERROR, msgs[0].GetStatus())
	suite.Equal("yahoo sent no price for BRKB on 2024-02-14, 2024-02-20", msgs[0].GetError())
	suite.marketdata.AssertNotCalled(suite.T(), "SearchAsset", mock.Anything)

	var rows int
	suite.Require().NoError(suite.pgx.QueryRow(context.Background(),
		`SELECT count(*) FROM ohlc WHERE symbol IN ('BRKB', 'BRK-B')`).Scan(&rows))
	suite.Zero(rows)
}

func (suite *MarketdataTest) recvAll(stream servicePb.Marketdata_DownloadHistoricalDataClient) []*servicePb.DownloadHistoricalDataResponse {
	var msgs []*servicePb.DownloadHistoricalDataResponse
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return msgs
		}
		suite.Require().NoError(err)
		msgs = append(msgs, msg)
	}
}

// A DB error on one row rolls the whole symbol back, and its text (which may
// carry a DSN or SQL) stays in the log, never in the response.
func (suite *MarketdataTest) TestDownloadHistoricalDataScrubsStoreErrorAndRollsBack() {
	const leak = "dial postgres://admin:hunter2@10.0.0.5:5432/prod: INSERT INTO ohlc"
	_, err := suite.pgx.Exec(context.Background(), `
CREATE OR REPLACE FUNCTION fail_on_bad_row() RETURNS trigger AS $$
BEGIN
	IF NEW.close = 13.37 THEN
		RAISE EXCEPTION '`+leak+`';
	END IF;
	RETURN NEW;
END $$ LANGUAGE plpgsql;
CREATE TRIGGER fail_on_bad_row BEFORE INSERT ON ohlc FOR EACH ROW EXECUTE FUNCTION fail_on_bad_row();`)
	suite.Require().NoError(err)

	suite.marketdata.On("GetAsset", mock.Anything, "XYZ").Return(&pb.Asset{Symbol: "XYZ", Name: "XYZ Corp"}, nil)
	suite.marketdata.On("GetAsset", mock.Anything, "LEAK").Return(nil, errors.New(leak))
	suite.marketdata.On("GetOHLC", mock.Anything, "XYZ", mock.Anything, mock.Anything).Return([]*pb.OHLC{
		{Timestamp: timestamppb.New(mustTime("2026-05-01")), Open: 10, High: 14, Low: 9, Close: 10.5, Volume: 100},
		{Timestamp: timestamppb.New(mustTime("2026-05-02")), Open: 10, High: 14, Low: 9, Close: 13.37, Volume: 100},
		{Timestamp: timestamppb.New(mustTime("2026-05-03")), Open: 10, High: 14, Low: 9, Close: 10.5, Volume: 100},
	}, nil)

	rsp, err := suite.client.DownloadHistoricalData(context.Background(), &servicePb.DownloadHistoricalDataRequest{
		Symbols:   []string{"XYZ", "LEAK"},
		StartDate: &internal_pb.Date{Year: 2026, Month: 5, Day: 1},
		EndDate:   &internal_pb.Date{Year: 2026, Month: 5, Day: 3},
		Provider:  "yahoo",
	})
	suite.Require().NoError(err)

	errs := make(map[string]string)
	for _, msg := range suite.recvAll(rsp) {
		suite.Equal(servicePb.DownloadHistoricalDataResponse_STATUS_ERROR, msg.GetStatus(), msg.GetSymbol())
		errs[msg.GetSymbol()] = msg.GetError()
	}
	suite.Equal("couldn't download prices for XYZ. Try again in a few minutes, or use another provider with --provider.", errs["XYZ"])
	suite.Equal("couldn't download prices for LEAK. Try again in a few minutes, or use another provider with --provider.", errs["LEAK"])
	for symbol, text := range errs {
		suite.NotContains(text, "hunter2", symbol)
		suite.NotContains(text, "INSERT", symbol)
	}

	var rows int
	suite.Require().NoError(suite.pgx.QueryRow(context.Background(),
		`SELECT count(*) FROM ohlc WHERE symbol = 'XYZ'`).Scan(&rows))
	suite.Zero(rows, "a DB error on one row must leave none of the symbol's bars")
}

// Many workers report on one stream; run with -race to catch concurrent Send.
func (suite *MarketdataTest) TestDownloadHistoricalDataManySymbols() {
	const n = 60
	symbols := make([]string, 0, n)
	for i := range n {
		symbols = append(symbols, fmt.Sprintf("SYM%02d", i))
	}
	suite.marketdata.On("GetAsset", mock.Anything, mock.Anything).Return(func(_ context.Context, symbol string) (*pb.Asset, error) {
		return &pb.Asset{Symbol: symbol, Name: symbol}, nil
	})
	suite.marketdata.On("GetOHLC", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return([]*pb.OHLC{
		{Timestamp: timestamppb.New(mustTime("2026-05-01")), Open: 10, High: 11, Low: 9, Close: 10.5, Volume: 100},
	}, nil)

	rsp, err := suite.client.DownloadHistoricalData(context.Background(), &servicePb.DownloadHistoricalDataRequest{
		Symbols:   symbols,
		StartDate: &internal_pb.Date{Year: 2026, Month: 5, Day: 1},
		EndDate:   &internal_pb.Date{Year: 2026, Month: 5, Day: 1},
		Provider:  "yahoo",
	})
	suite.Require().NoError(err)

	seen := make(map[string]bool)
	for _, msg := range suite.recvAll(rsp) {
		suite.Equal(servicePb.DownloadHistoricalDataResponse_STATUS_OK, msg.GetStatus(), msg.GetSymbol())
		seen[msg.GetSymbol()] = true
	}
	suite.Len(seen, n)
}

func (suite *MarketdataTest) TestDownloadAssetsNoEndDate() {
	suite.marketdata.On("GetAsset", mock.Anything, "AAPL").Return(&pb.Asset{Symbol: "AAPL", Name: "Apple Inc."}, nil)
	suite.marketdata.On("GetAsset", mock.Anything, "NOCORP").Return(nil, errors.New("not found"))
	suite.marketdata.On("GetOHLC", mock.Anything, "AAPL", mock.Anything, mock.Anything).Return([]*pb.OHLC{
		{
			Timestamp: timestamppb.Now(),
			Open:      1,
			High:      2,
			Low:       0.5,
			Close:     1.5,
			Volume:    100,
		},
	}, nil)

	req := &servicePb.DownloadHistoricalDataRequest{
		Symbols:   []string{"AAPL", "NOCORP"},
		StartDate: &internal_pb.Date{Year: 2020, Month: 1, Day: 1},
		// yahoo routes GetOHLC to the injected mock.
		Provider: "yahoo",
	}
	rsp, err := suite.client.DownloadHistoricalData(context.Background(), req)
	suite.Require().NoError(err)

	result := make(map[string]servicePb.DownloadHistoricalDataResponse_Status)
	for {
		msg, err := rsp.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			suite.Require().NoError(err)
			break
		}
		suite.Require().NoError(err)
		result[msg.GetSymbol()] = msg.GetStatus()
	}
	suite.Equal(servicePb.DownloadHistoricalDataResponse_STATUS_OK, result["AAPL"])
	suite.Equal(servicePb.DownloadHistoricalDataResponse_STATUS_ERROR, result["NOCORP"])
}

func (suite *MarketdataTest) TestDownloadHistoricalDataStopsOnBlockedProvider() {
	symbols := []string{"AAPL", "MSFT", "NVDA", "AMZN", "GOOG", "META", "TSLA", "AVGO", "COST", "NFLX", "AMD", "PEP"}
	for _, symbol := range symbols {
		suite.marketdata.On("GetAsset", mock.Anything, symbol).Return(&pb.Asset{Symbol: symbol, Name: symbol}, nil).Maybe()
		suite.marketdata.On("GetOHLC", mock.Anything, symbol, mock.Anything, mock.Anything).
			Return(nil, &marketdata.BlockedError{Provider: "yahoo", Symbol: symbol}).Maybe()
	}

	req := &servicePb.DownloadHistoricalDataRequest{
		Symbols:   symbols,
		StartDate: &internal_pb.Date{Year: 2020, Month: 1, Day: 1},
		// yahoo routes GetOHLC to the injected mock.
		Provider: "yahoo",
	}
	rsp, err := suite.client.DownloadHistoricalData(context.Background(), req)
	suite.Require().NoError(err)

	var streamErr error
	responses := 0
	for {
		msg, err := rsp.Recv()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				streamErr = err
			}
			break
		}
		responses++
		suite.NotEqual(servicePb.DownloadHistoricalDataResponse_STATUS_ERROR, msg.GetStatus(),
			"%s was reported as failed, which disables it", msg.GetSymbol())
	}
	suite.Zero(responses, "a blocked symbol must not be reported per symbol")

	st, ok := status.FromError(streamErr)
	suite.Require().True(ok, "want a gRPC status, got %v", streamErr)
	suite.Equal(codes.FailedPrecondition, st.Code())
	suite.Regexp(`^Yahoo blocked the download for [A-Z]+ \(it returned a bot-check page instead of data\)\. Retry with --provider stooq\.$`, st.Message())
}

const metaISIN = "US30303M1027"

// Yahoo has no bars under FB. The ISIN's current ticker (META) still has the
// history, including dates before the rename. Download must fetch under META,
// store those bars, record META as a later alias, and report OK for FB.
func (suite *MarketdataTest) TestDownloadHistoricalDataUsesLaterTickerWhenAskedHasNoBars() {
	ctx := context.Background()
	instruments := repository.Instruments{Conn: suite.pgx}
	_, err := instruments.Store(ctx, metaISIN, "Facebook Inc")
	suite.Require().NoError(err)
	_, err = suite.pgx.Exec(ctx, `
		INSERT INTO asset_symbol (isin, symbol, from_date, to_date)
		VALUES ($1, 'FB', '2019-01-01'::date, NULL)`, metaISIN)
	suite.Require().NoError(err)

	suite.marketdata.On("GetAsset", mock.Anything, "FB").Return(&pb.Asset{Symbol: "FB", Name: "ProShares Buffer ETF"}, nil)
	suite.marketdata.On("GetAsset", mock.Anything, "META").Return(&pb.Asset{Symbol: "META", Name: "Meta Platforms, Inc.", ISIN: metaISIN}, nil)
	suite.marketdata.On("GetOHLC", mock.Anything, "FB", mock.Anything, mock.Anything).
		Return(nil, yahooNotFound("FB"))
	suite.marketdata.On("GetOHLC", mock.Anything, "META", mock.Anything, mock.Anything).Return([]*pb.OHLC{
		{
			Timestamp: timestamppb.New(mustTime("2019-06-03")),
			Open:      10, High: 11, Low: 9, Close: 10.5, Volume: 1000,
		},
	}, nil)
	suite.marketdata.On("SearchAsset", mock.Anything, metaISIN).Return([]*pb.Asset{
		{Symbol: "META", Name: "Meta Platforms, Inc.", ISIN: metaISIN},
	}, nil)

	req := &servicePb.DownloadHistoricalDataRequest{
		Symbols:   []string{"FB"},
		StartDate: &internal_pb.Date{Year: 2019, Month: 1, Day: 1},
		EndDate:   &internal_pb.Date{Year: 2020, Month: 12, Day: 31},
		Provider:  "yahoo",
	}
	rsp, err := suite.client.DownloadHistoricalData(context.Background(), req)
	suite.Require().NoError(err)

	var statuses []servicePb.DownloadHistoricalDataResponse_Status
	for {
		msg, err := rsp.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		suite.Require().NoError(err)
		suite.Equal("FB", msg.GetSymbol())
		statuses = append(statuses, msg.GetStatus())
	}
	suite.Equal([]servicePb.DownloadHistoricalDataResponse_Status{
		servicePb.DownloadHistoricalDataResponse_STATUS_OK,
	}, statuses)

	var metaBars, fbBars int
	suite.Require().NoError(suite.pgx.QueryRow(ctx,
		`SELECT count(*) FROM ohlc WHERE provider='yahoo' AND symbol='META'`).Scan(&metaBars))
	suite.Require().NoError(suite.pgx.QueryRow(ctx,
		`SELECT count(*) FROM ohlc WHERE provider='yahoo' AND symbol='FB'`).Scan(&fbBars))
	suite.Equal(1, metaBars, "bars must be stored under META")
	suite.Zero(fbBars, "Yahoo serves no FB bars")

	aliases, err := instruments.ListAliases(ctx, metaISIN)
	suite.Require().NoError(err)
	symbols := make([]string, 0, len(aliases))
	for _, a := range aliases {
		symbols = append(symbols, a.Symbol)
	}
	suite.Contains(symbols, "META")
	suite.Contains(symbols, "FB")
}

// The OK response for FB names the later ticker its bars were read under,
// with no day: the provider doesn't say when FB became META.
func (suite *MarketdataTest) TestDownloadHistoricalDataNamesTheLaterTicker() {
	ctx := context.Background()
	suite.fbRenamedToMeta(ctx)
	suite.metaHasBars()

	rsp := suite.downloadFB(ctx)
	msg, err := rsp.Recv()
	suite.Require().NoError(err)
	suite.Equal("FB", msg.GetSymbol())
	suite.Equal(servicePb.DownloadHistoricalDataResponse_STATUS_OK, msg.GetStatus())
	suite.Require().NotNil(msg.GetLaterTicker())
	suite.Equal("META", msg.GetLaterTicker().GetSymbol())
	_, err = rsp.Recv()
	suite.Require().ErrorIs(err, io.EOF)
}

// META is already stored, so nothing is written, and the response still
// names it.
func (suite *MarketdataTest) TestDownloadHistoricalDataNamesAStoredLaterTicker() {
	ctx := context.Background()
	instruments := repository.Instruments{Conn: suite.pgx}
	_, err := instruments.Store(ctx, metaISIN, "Meta Platforms, Inc.")
	suite.Require().NoError(err)
	_, err = suite.pgx.Exec(ctx, `
		INSERT INTO asset_symbol (isin, symbol, from_date, to_date)
		VALUES ($1, 'FB', '2019-01-01'::date, '2022-06-09'::date),
		       ($1, 'META', '2022-06-09'::date, NULL)`, metaISIN)
	suite.Require().NoError(err)
	suite.metaHasBars()

	rsp := suite.downloadFB(ctx)
	msg, err := rsp.Recv()
	suite.Require().NoError(err)
	suite.Equal(servicePb.DownloadHistoricalDataResponse_STATUS_OK, msg.GetStatus())
	suite.Require().NotNil(msg.GetLaterTicker())
	suite.Equal("META", msg.GetLaterTicker().GetSymbol())
	aliases, err := instruments.ListAliases(ctx, metaISIN)
	suite.Require().NoError(err)
	suite.Len(aliases, 2)
}

// Bars read under the asked ticker carry no later ticker.
func (suite *MarketdataTest) TestDownloadHistoricalDataSendsNoLaterTickerForTheAskedOne() {
	ctx := context.Background()
	suite.marketdata.On("GetAsset", mock.Anything, "META").Return(&pb.Asset{Symbol: "META", Name: "Meta Platforms, Inc.", ISIN: metaISIN}, nil)
	suite.marketdata.On("GetOHLC", mock.Anything, "META", mock.Anything, mock.Anything).Return([]*pb.OHLC{
		{Timestamp: timestamppb.New(mustTime("2019-06-03")), Open: 10, High: 11, Low: 9, Close: 10.5, Volume: 1000},
	}, nil)

	rsp, err := suite.client.DownloadHistoricalData(ctx, &servicePb.DownloadHistoricalDataRequest{
		Symbols:   []string{"META"},
		StartDate: &internal_pb.Date{Year: 2019, Month: 1, Day: 1},
		EndDate:   &internal_pb.Date{Year: 2020, Month: 12, Day: 31},
		Provider:  "yahoo",
	})
	suite.Require().NoError(err)
	msg, err := rsp.Recv()
	suite.Require().NoError(err)
	suite.Equal(servicePb.DownloadHistoricalDataResponse_STATUS_OK, msg.GetStatus())
	suite.Nil(msg.GetLaterTicker())
}

// Another ticker of the ISIN already starts on the day META would be written
// from, so the insert stores nothing. The read-back finds no META, so FB
// fails instead of reporting a later ticker that isn't recorded, and no META
// bars are stored.
func (suite *MarketdataTest) TestDownloadHistoricalDataFailsWhenTheLaterTickerIsNotStored() {
	ctx := context.Background()
	instruments := repository.Instruments{Conn: suite.pgx}
	_, err := instruments.Store(ctx, metaISIN, "Facebook Inc")
	suite.Require().NoError(err)
	_, err = suite.pgx.Exec(ctx, `
		INSERT INTO asset_symbol (isin, symbol, from_date, to_date)
		VALUES ($1, 'FB', '2019-01-01'::date, '2021-01-01'::date),
		       ($1, 'FBB', '2021-01-01'::date, NULL)`, metaISIN)
	suite.Require().NoError(err)
	suite.metaHasBars()
	suite.marketdata.On("GetOHLC", mock.Anything, "FBB", mock.Anything, mock.Anything).Return(nil, yahooNotFound("FBB"))

	rsp := suite.downloadFB(ctx)
	msg, err := rsp.Recv()
	suite.Require().NoError(err)
	suite.requireOnlyText(msg, "FB", downloadFailedText("FB"))
	suite.Nil(msg.GetLaterTicker())

	var metaBars int
	suite.Require().NoError(suite.pgx.QueryRow(ctx,
		`SELECT count(*) FROM ohlc WHERE symbol='META'`).Scan(&metaBars))
	suite.Zero(metaBars, "no META bars may be stored")
	aliases, err := instruments.ListAliases(ctx, metaISIN)
	suite.Require().NoError(err)
	for _, a := range aliases {
		suite.NotEqual("META", a.Symbol)
	}
}

// metaHasBars mocks Yahoo with no bars under FB and one bar under META,
// which a search by the ISIN finds.
func (suite *MarketdataTest) metaHasBars() {
	suite.marketdata.On("GetAsset", mock.Anything, "FB").Return(&pb.Asset{Symbol: "FB", Name: "Facebook"}, nil)
	suite.marketdata.On("GetAsset", mock.Anything, "META").Return(&pb.Asset{Symbol: "META", Name: "Meta Platforms, Inc.", ISIN: metaISIN}, nil)
	suite.marketdata.On("GetOHLC", mock.Anything, "FB", mock.Anything, mock.Anything).Return(nil, yahooNotFound("FB"))
	suite.marketdata.On("GetOHLC", mock.Anything, "META", mock.Anything, mock.Anything).Return([]*pb.OHLC{
		{Timestamp: timestamppb.New(mustTime("2019-06-03")), Open: 10, High: 11, Low: 9, Close: 10.5, Volume: 1000},
	}, nil)
	suite.marketdata.On("SearchAsset", mock.Anything, metaISIN).Return([]*pb.Asset{
		{Symbol: "META", Name: "Meta Platforms, Inc.", ISIN: metaISIN},
	}, nil)
}

func (suite *MarketdataTest) TestDownloadHistoricalDataFailsWhenNoTickerHasBars() {
	ctx := context.Background()
	instruments := repository.Instruments{Conn: suite.pgx}
	_, err := instruments.Store(ctx, metaISIN, "Facebook Inc")
	suite.Require().NoError(err)
	_, err = suite.pgx.Exec(ctx, `
		INSERT INTO asset_symbol (isin, symbol, from_date, to_date)
		VALUES ($1, 'FB', '2019-01-01'::date, NULL)`, metaISIN)
	suite.Require().NoError(err)

	suite.marketdata.On("GetAsset", mock.Anything, "FB").Return(&pb.Asset{Symbol: "FB", Name: "Facebook"}, nil)
	suite.marketdata.On("GetOHLC", mock.Anything, "FB", mock.Anything, mock.Anything).
		Return(nil, yahooNotFound("FB"))
	suite.marketdata.On("GetOHLC", mock.Anything, "META", mock.Anything, mock.Anything).
		Return(nil, yahooNotFound("META")).Maybe()
	suite.marketdata.On("SearchAsset", mock.Anything, metaISIN).Return([]*pb.Asset{
		{Symbol: "META", Name: "Meta Platforms, Inc.", ISIN: metaISIN},
	}, nil)

	req := &servicePb.DownloadHistoricalDataRequest{
		Symbols:   []string{"FB"},
		StartDate: &internal_pb.Date{Year: 2019, Month: 1, Day: 1},
		EndDate:   &internal_pb.Date{Year: 2020, Month: 12, Day: 31},
		Provider:  "yahoo",
	}
	rsp, err := suite.client.DownloadHistoricalData(context.Background(), req)
	suite.Require().NoError(err)

	msg, err := rsp.Recv()
	suite.Require().NoError(err)
	suite.Equal("FB", msg.GetSymbol())
	suite.Equal(servicePb.DownloadHistoricalDataResponse_STATUS_ERROR, msg.GetStatus())
	suite.Equal("no price data for FB", msg.GetError())
	_, err = rsp.Recv()
	suite.Require().ErrorIs(err, io.EOF)
}

// downloadFailedText is the STATUS_ERROR text for a failed price request
// or ticker search. The raw cause is logged, not sent.
func downloadFailedText(symbol string) string {
	return "couldn't download prices for " + symbol + ". Try again in a few minutes, or use another provider with --provider."
}

// yahooNotFound is Yahoo's answer for a ticker it has no chart for. It is
// an answer, not a failure, so the download may try another ticker or
// report no price data.
func yahooNotFound(symbol string) error {
	return fmt.Errorf("fail to get OHLC data for symbol %s: No data found, symbol may be delisted: %w",
		symbol, marketdata.ErrNoBars)
}

// yahooURLError is a failed Yahoo request whose text quotes the URL with
// its crumb, as the HTTP client reports it.
func yahooURLError(symbol string) error {
	return fmt.Errorf("fail to get OHLC data for symbol %s: error making request: %w", symbol, &url.Error{
		Op:  "Get",
		URL: "https://query2.finance.yahoo.com/v8/finance/chart/" + symbol + "?crumb=s3cr3tcrumb&interval=1d",
		Err: errors.New("503 Service Unavailable"),
	})
}

// rawLeaks are fragments of provider, URL or database errors that must
// never reach a STATUS_ERROR text.
var rawLeaks = []string{"503", "Service Unavailable", "crumb", "s3cr3t", "yahoo.com", "http", "postgres", "asset_symbol", "search for"}

func (suite *MarketdataTest) requireOnlyText(msg *servicePb.DownloadHistoricalDataResponse, symbol, want string) {
	suite.Equal(symbol, msg.GetSymbol())
	suite.Equal(servicePb.DownloadHistoricalDataResponse_STATUS_ERROR, msg.GetStatus())
	suite.Equal(want, msg.GetError())
	for _, raw := range rawLeaks {
		suite.NotContains(msg.GetError(), raw)
	}
}

// fbRenamedToMeta stores the instrument with FB as its only alias, so META
// can only come from the search.
func (suite *MarketdataTest) fbRenamedToMeta(ctx context.Context) {
	instruments := repository.Instruments{Conn: suite.pgx}
	_, err := instruments.Store(ctx, metaISIN, "Facebook Inc")
	suite.Require().NoError(err)
	_, err = suite.pgx.Exec(ctx, `
		INSERT INTO asset_symbol (isin, symbol, from_date, to_date)
		VALUES ($1, 'FB', '2019-01-01'::date, NULL)`, metaISIN)
	suite.Require().NoError(err)
}

func (suite *MarketdataTest) downloadFB(ctx context.Context) servicePb.Marketdata_DownloadHistoricalDataClient {
	rsp, err := suite.client.DownloadHistoricalData(ctx, &servicePb.DownloadHistoricalDataRequest{
		Symbols:   []string{"FB"},
		StartDate: &internal_pb.Date{Year: 2019, Month: 1, Day: 1},
		EndDate:   &internal_pb.Date{Year: 2020, Month: 12, Day: 31},
		Provider:  "yahoo",
	})
	suite.Require().NoError(err)
	return rsp
}

// requireNothingStoredForMeta checks that no META bars were stored and FB is
// still the instrument's only, open alias.
func (suite *MarketdataTest) requireNothingStoredForMeta(ctx context.Context) {
	var metaBars int
	suite.Require().NoError(suite.pgx.QueryRow(ctx,
		`SELECT count(*) FROM ohlc WHERE symbol='META'`).Scan(&metaBars))
	suite.Zero(metaBars, "no META bars may be stored")

	var aliases []string
	rows, err := suite.pgx.Query(ctx,
		`SELECT symbol || ':' || coalesce(to_date::text, 'open') FROM asset_symbol WHERE isin=$1 ORDER BY from_date`, metaISIN)
	suite.Require().NoError(err)
	for rows.Next() {
		var a string
		suite.Require().NoError(rows.Scan(&a))
		aliases = append(aliases, a)
	}
	suite.Require().NoError(rows.Err())
	suite.Equal([]string{"FB:open"}, aliases, "the aliases must be unchanged")
}

// FB has no bars, and fetching under its later ticker META fails. We don't
// know whether META has prices, so the failure is reported, not "no price
// data".
func (suite *MarketdataTest) TestDownloadHistoricalDataReportsLaterTickerGetOHLCFailure() {
	ctx := context.Background()
	suite.fbRenamedToMeta(ctx)

	suite.marketdata.On("GetAsset", mock.Anything, "FB").Return(&pb.Asset{Symbol: "FB", Name: "Facebook"}, nil)
	suite.marketdata.On("GetOHLC", mock.Anything, "FB", mock.Anything, mock.Anything).Return(nil, yahooNotFound("FB"))
	suite.marketdata.On("GetOHLC", mock.Anything, "META", mock.Anything, mock.Anything).Return(nil, yahooURLError("META"))
	suite.marketdata.On("SearchAsset", mock.Anything, metaISIN).Return([]*pb.Asset{
		{Symbol: "META", Name: "Meta Platforms, Inc.", ISIN: metaISIN},
	}, nil)

	rsp := suite.downloadFB(ctx)
	msg, err := rsp.Recv()
	suite.Require().NoError(err)
	suite.requireOnlyText(msg, "FB", downloadFailedText("FB"))
	_, err = rsp.Recv()
	suite.Require().ErrorIs(err, io.EOF)
	suite.marketdata.AssertNotCalled(suite.T(), "GetAsset", "META")
	suite.requireNothingStoredForMeta(ctx)
}

// The search returns only a quote without an ISIN (an ETF or fund) that has
// bars. It may be another company, so it is not a candidate: FB has no
// price data and nothing is stored under that ticker.
func (suite *MarketdataTest) TestDownloadHistoricalDataIgnoresSearchHitWithoutISIN() {
	ctx := context.Background()
	suite.fbRenamedToMeta(ctx)

	suite.marketdata.On("GetAsset", mock.Anything, "FB").Return(&pb.Asset{Symbol: "FB", Name: "Facebook"}, nil)
	suite.marketdata.On("GetOHLC", mock.Anything, "FB", mock.Anything, mock.Anything).Return(nil, yahooNotFound("FB"))
	suite.marketdata.On("GetOHLC", mock.Anything, "FBETF", mock.Anything, mock.Anything).Return([]*pb.OHLC{
		{Timestamp: timestamppb.New(mustTime("2019-06-03")), Open: 10, High: 11, Low: 9, Close: 10.5, Volume: 1000},
	}, nil).Maybe()
	suite.marketdata.On("SearchAsset", mock.Anything, metaISIN).Return([]*pb.Asset{
		{Symbol: "FBETF", Name: "Some Facebook ETF", ISIN: ""},
	}, nil)

	rsp := suite.downloadFB(ctx)
	msg, err := rsp.Recv()
	suite.Require().NoError(err)
	suite.requireOnlyText(msg, "FB", "no price data for FB")
	_, err = rsp.Recv()
	suite.Require().ErrorIs(err, io.EOF)
	suite.marketdata.AssertNotCalled(suite.T(), "GetOHLC", "FBETF", mock.Anything, mock.Anything)

	var bars int
	suite.Require().NoError(suite.pgx.QueryRow(ctx,
		`SELECT count(*) FROM ohlc WHERE symbol='FBETF'`).Scan(&bars))
	suite.Zero(bars, "a search hit without the ISIN must not store bars")
}

// The asked symbol is an equity the provider has no ISIN for. A retry
// can't help, so the symbol fails with the no-ISIN text, not the
// download-failed sentence, and no bars are fetched.
func (suite *MarketdataTest) TestDownloadHistoricalDataReportsAskedSymbolWithoutISIN() {
	suite.marketdata.On("GetAsset", mock.Anything, "XYZ").Return(nil, fmt.Errorf("error getting isin for symbol XYZ: %w",
		fmt.Errorf("ISIN not found for symbol XYZ: %w", marketdata.ErrNoISIN)))

	rsp, err := suite.client.DownloadHistoricalData(context.Background(), &servicePb.DownloadHistoricalDataRequest{
		Symbols:   []string{"XYZ"},
		StartDate: &internal_pb.Date{Year: 2024, Month: 1, Day: 2},
		EndDate:   &internal_pb.Date{Year: 2024, Month: 3, Day: 28},
		Provider:  "yahoo",
	})
	suite.Require().NoError(err)

	msg, err := rsp.Recv()
	suite.Require().NoError(err)
	suite.requireOnlyText(msg, "XYZ", "no ISIN for XYZ")
	suite.NotContains(msg.GetError(), "not found")
	_, err = rsp.Recv()
	suite.Require().ErrorIs(err, io.EOF)
	suite.marketdata.AssertNotCalled(suite.T(), "GetOHLC", mock.Anything, mock.Anything, mock.Anything)
}

// A provider call that returns context.Canceled while the download itself
// is still running is a failed request, not "no price data".
func (suite *MarketdataTest) TestDownloadHistoricalDataReportsLaterTickerCanceledRequest() {
	ctx := context.Background()
	suite.fbRenamedToMeta(ctx)

	suite.marketdata.On("GetAsset", mock.Anything, "FB").Return(&pb.Asset{Symbol: "FB", Name: "Facebook"}, nil)
	suite.marketdata.On("GetOHLC", mock.Anything, "FB", mock.Anything, mock.Anything).Return(nil, yahooNotFound("FB"))
	suite.marketdata.On("GetOHLC", mock.Anything, "META", mock.Anything, mock.Anything).
		Return(nil, fmt.Errorf("fail to get OHLC data for symbol META: %w", context.Canceled))
	suite.marketdata.On("SearchAsset", mock.Anything, metaISIN).Return([]*pb.Asset{
		{Symbol: "META", Name: "Meta Platforms, Inc.", ISIN: metaISIN},
	}, nil)

	msg, err := suite.downloadFB(ctx).Recv()
	suite.Require().NoError(err)
	suite.requireOnlyText(msg, "FB", downloadFailedText("FB"))
	suite.NotContains(msg.GetError(), "canceled")
	suite.requireNothingStoredForMeta(ctx)
}

// Yahoo answers the first request with no bars and the retry fails. We don't
// know whether the symbol has prices, so the failure is reported and no
// other ticker is tried.
func (suite *MarketdataTest) TestDownloadHistoricalDataReportsRetryFailureAfterEmptyAnswer() {
	suite.marketdata.On("GetAsset", mock.Anything, "BK").Return(&pb.Asset{Symbol: "BK", Name: "The Bank of New York Mellon Corporation"}, nil)
	suite.marketdata.On("GetOHLC", mock.Anything, "BK", mock.Anything, mock.Anything).Return([]*pb.OHLC{}, nil).Once()
	suite.marketdata.On("GetOHLC", mock.Anything, "BK", mock.Anything, mock.Anything).Return(nil, yahooURLError("BK")).Once()

	rsp, err := suite.client.DownloadHistoricalData(context.Background(), &servicePb.DownloadHistoricalDataRequest{
		Symbols:   []string{"BK"},
		StartDate: &internal_pb.Date{Year: 2024, Month: 1, Day: 2},
		EndDate:   &internal_pb.Date{Year: 2024, Month: 3, Day: 28},
		Provider:  "yahoo",
	})
	suite.Require().NoError(err)

	msg, err := rsp.Recv()
	suite.Require().NoError(err)
	suite.requireOnlyText(msg, "BK", downloadFailedText("BK"))
	_, err = rsp.Recv()
	suite.Require().ErrorIs(err, io.EOF)
	suite.marketdata.AssertNumberOfCalls(suite.T(), "GetOHLC", 2)
	suite.marketdata.AssertNotCalled(suite.T(), "SearchAsset", mock.Anything)

	var bars int
	suite.Require().NoError(suite.pgx.QueryRow(context.Background(),
		`SELECT count(*) FROM ohlc WHERE symbol='BK'`).Scan(&bars))
	suite.Zero(bars)
}

// FB gets Yahoo's Not Found and the search for the instrument's current
// ticker fails. The search may have missed a ticker with bars, so the
// download reports that failure, not "no price data".
func (suite *MarketdataTest) TestDownloadHistoricalDataReportsSearchFailureWhenNoTickerHasBars() {
	ctx := context.Background()
	suite.fbRenamedToMeta(ctx)

	suite.marketdata.On("GetAsset", mock.Anything, "FB").Return(&pb.Asset{Symbol: "FB", Name: "Facebook"}, nil)
	suite.marketdata.On("GetOHLC", mock.Anything, "FB", mock.Anything, mock.Anything).Return(nil, yahooNotFound("FB"))
	suite.marketdata.On("SearchAsset", mock.Anything, metaISIN).Return(nil, errors.New("fail to search asset: 503 Service Unavailable"))

	rsp := suite.downloadFB(ctx)
	msg, err := rsp.Recv()
	suite.Require().NoError(err)
	suite.requireOnlyText(msg, "FB", downloadFailedText("FB"))
	_, err = rsp.Recv()
	suite.Require().ErrorIs(err, io.EOF)
}

// A failed search fails the symbol even when an asset_symbol alias of the
// instrument has bars: the search may have found the current ticker, so no
// bars are fetched or stored on the aliases alone.
func (suite *MarketdataTest) TestDownloadHistoricalDataFailsOnSearchErrorEvenWhenAnAliasHasBars() {
	ctx := context.Background()
	instruments := repository.Instruments{Conn: suite.pgx}
	_, err := instruments.Store(ctx, metaISIN, "Facebook Inc")
	suite.Require().NoError(err)
	_, err = suite.pgx.Exec(ctx, `
		INSERT INTO asset_symbol (isin, symbol, from_date, to_date)
		VALUES ($1, 'FB', '2019-01-01'::date, '2022-06-08'::date),
		       ($1, 'META', '2022-06-09'::date, NULL)`, metaISIN)
	suite.Require().NoError(err)

	suite.marketdata.On("GetAsset", mock.Anything, "FB").Return(&pb.Asset{Symbol: "FB", Name: "Facebook"}, nil)
	suite.marketdata.On("GetOHLC", mock.Anything, "FB", mock.Anything, mock.Anything).Return(nil, yahooNotFound("FB"))
	suite.marketdata.On("SearchAsset", mock.Anything, metaISIN).Return(nil, errors.New("fail to search asset: 503 Service Unavailable"))

	msg, err := suite.downloadFB(ctx).Recv()
	suite.Require().NoError(err)
	suite.requireOnlyText(msg, "FB", downloadFailedText("FB"))
	suite.marketdata.AssertNotCalled(suite.T(), "GetOHLC", "META", mock.Anything, mock.Anything)

	var metaBars int
	suite.Require().NoError(suite.pgx.QueryRow(ctx,
		`SELECT count(*) FROM ohlc WHERE symbol='META'`).Scan(&metaBars))
	suite.Zero(metaBars)
}

// META has bars, but its asset lookup fails. The symbol fails; no bars are
// stored and no alias is recorded.
func (suite *MarketdataTest) TestDownloadHistoricalDataReportsLaterTickerGetAssetFailure() {
	ctx := context.Background()
	suite.fbRenamedToMeta(ctx)

	suite.marketdata.On("GetAsset", mock.Anything, "FB").Return(&pb.Asset{Symbol: "FB", Name: "Facebook"}, nil)
	suite.marketdata.On("GetAsset", mock.Anything, "META").Return(nil, yahooURLError("META"))
	suite.marketdata.On("GetOHLC", mock.Anything, "FB", mock.Anything, mock.Anything).Return(nil, yahooNotFound("FB"))
	suite.marketdata.On("GetOHLC", mock.Anything, "META", mock.Anything, mock.Anything).Return([]*pb.OHLC{
		{Timestamp: timestamppb.New(mustTime("2019-06-03")), Open: 10, High: 11, Low: 9, Close: 10.5, Volume: 1000},
	}, nil)
	suite.marketdata.On("SearchAsset", mock.Anything, metaISIN).Return([]*pb.Asset{
		{Symbol: "META", Name: "Meta Platforms, Inc.", ISIN: metaISIN},
	}, nil)

	msg, err := suite.downloadFB(ctx).Recv()
	suite.Require().NoError(err)
	suite.requireOnlyText(msg, "FB", downloadFailedText("FB"))
	suite.requireNothingStoredForMeta(ctx)
}

// META has bars, but storing its asset row fails. The symbol fails with the
// fixed sentence; the database text stays in the log.
func (suite *MarketdataTest) TestDownloadHistoricalDataReportsLaterTickerAssetStoreFailure() {
	ctx := context.Background()
	suite.fbRenamedToMeta(ctx)
	_, err := suite.pgx.Exec(ctx, `
CREATE OR REPLACE FUNCTION fail_meta_asset() RETURNS trigger AS $$
BEGIN
	IF NEW.symbol = 'META' THEN
		RAISE EXCEPTION 'postgres://admin:s3cr3t@db/prod: INSERT INTO asset refused';
	END IF;
	RETURN NEW;
END $$ LANGUAGE plpgsql;
CREATE TRIGGER fail_meta_asset BEFORE INSERT ON asset FOR EACH ROW EXECUTE FUNCTION fail_meta_asset();`)
	suite.Require().NoError(err)

	suite.marketdata.On("GetAsset", mock.Anything, "FB").Return(&pb.Asset{Symbol: "FB", Name: "Facebook"}, nil)
	suite.marketdata.On("GetAsset", mock.Anything, "META").Return(&pb.Asset{Symbol: "META", Name: "Meta Platforms, Inc.", ISIN: metaISIN}, nil)
	suite.marketdata.On("GetOHLC", mock.Anything, "FB", mock.Anything, mock.Anything).Return(nil, yahooNotFound("FB"))
	suite.marketdata.On("GetOHLC", mock.Anything, "META", mock.Anything, mock.Anything).Return([]*pb.OHLC{
		{Timestamp: timestamppb.New(mustTime("2019-06-03")), Open: 10, High: 11, Low: 9, Close: 10.5, Volume: 1000},
	}, nil)
	suite.marketdata.On("SearchAsset", mock.Anything, metaISIN).Return([]*pb.Asset{
		{Symbol: "META", Name: "Meta Platforms, Inc.", ISIN: metaISIN},
	}, nil)

	msg, err := suite.downloadFB(ctx).Recv()
	suite.Require().NoError(err)
	suite.requireOnlyText(msg, "FB", downloadFailedText("FB"))
	suite.requireNothingStoredForMeta(ctx)
}

// META has bars, but recording it as FB's later ticker fails. The symbol
// fails, no bars are stored, and FB's alias is left open (the close and the
// insert are one transaction).
func (suite *MarketdataTest) TestDownloadHistoricalDataReportsLaterAliasFailureAndStoresNothing() {
	ctx := context.Background()
	suite.fbRenamedToMeta(ctx)
	_, err := suite.pgx.Exec(ctx, `
CREATE OR REPLACE FUNCTION fail_meta_alias() RETURNS trigger AS $$
BEGIN
	IF NEW.symbol = 'META' THEN
		RAISE EXCEPTION 'postgres://admin:s3cr3t@db/prod: INSERT INTO asset_symbol refused';
	END IF;
	RETURN NEW;
END $$ LANGUAGE plpgsql;
CREATE TRIGGER fail_meta_alias BEFORE INSERT ON asset_symbol FOR EACH ROW EXECUTE FUNCTION fail_meta_alias();`)
	suite.Require().NoError(err)

	suite.marketdata.On("GetAsset", mock.Anything, "FB").Return(&pb.Asset{Symbol: "FB", Name: "Facebook"}, nil)
	suite.marketdata.On("GetAsset", mock.Anything, "META").Return(&pb.Asset{Symbol: "META", Name: "Meta Platforms, Inc.", ISIN: metaISIN}, nil)
	suite.marketdata.On("GetOHLC", mock.Anything, "FB", mock.Anything, mock.Anything).Return(nil, yahooNotFound("FB"))
	suite.marketdata.On("GetOHLC", mock.Anything, "META", mock.Anything, mock.Anything).Return([]*pb.OHLC{
		{Timestamp: timestamppb.New(mustTime("2019-06-03")), Open: 10, High: 11, Low: 9, Close: 10.5, Volume: 1000},
	}, nil)
	suite.marketdata.On("SearchAsset", mock.Anything, metaISIN).Return([]*pb.Asset{
		{Symbol: "META", Name: "Meta Platforms, Inc.", ISIN: metaISIN},
	}, nil)

	rsp := suite.downloadFB(ctx)
	msg, err := rsp.Recv()
	suite.Require().NoError(err)
	suite.requireOnlyText(msg, "FB", downloadFailedText("FB"))
	_, err = rsp.Recv()
	suite.Require().ErrorIs(err, io.EOF)
	suite.requireNothingStoredForMeta(ctx)
}

// A GetAsset error can quote Yahoo's request URL with its crumb. Only the
// fixed sentence reaches the status; the raw error stays in the log.
func (suite *MarketdataTest) TestDownloadHistoricalDataKeepsGetAssetErrorOutOfStatus() {
	suite.marketdata.On("GetAsset", mock.Anything, "AAPL").Return(nil, fmt.Errorf("error: %w", &url.Error{
		Op:  "Get",
		URL: "https://query2.finance.yahoo.com/v10/finance/quoteSummary/AAPL?crumb=s3cr3tcrumb&modules=quoteType",
		Err: errors.New("connection reset by peer"),
	}))

	req := &servicePb.DownloadHistoricalDataRequest{
		Symbols:   []string{"AAPL"},
		StartDate: &internal_pb.Date{Year: 2019, Month: 1, Day: 1},
		EndDate:   &internal_pb.Date{Year: 2020, Month: 12, Day: 31},
		Provider:  "yahoo",
	}
	rsp, err := suite.client.DownloadHistoricalData(context.Background(), req)
	suite.Require().NoError(err)

	msg, err := rsp.Recv()
	suite.Require().NoError(err)
	suite.Equal(servicePb.DownloadHistoricalDataResponse_STATUS_ERROR, msg.GetStatus())
	suite.Equal(downloadFailedText("AAPL"), msg.GetError())
	for _, raw := range []string{"crumb", "s3cr3tcrumb", "http", "yahoo.com", "connection reset"} {
		suite.NotContains(msg.GetError(), raw)
	}
}

// Yahoo sometimes answers a listed symbol (BK) with no data. Download must
// ask once more and store the bars from the second answer.
func (suite *MarketdataTest) TestDownloadHistoricalDataRetriesAnEmptyAnswerOnce() {
	ctx := context.Background()
	suite.marketdata.On("GetAsset", mock.Anything, "BK").Return(&pb.Asset{Symbol: "BK", Name: "The Bank of New York Mellon Corporation"}, nil)
	suite.marketdata.On("GetOHLC", mock.Anything, "BK", mock.Anything, mock.Anything).Return([]*pb.OHLC{}, nil).Once()
	suite.marketdata.On("GetOHLC", mock.Anything, "BK", mock.Anything, mock.Anything).Return([]*pb.OHLC{
		{
			Timestamp: timestamppb.New(mustTime("2024-01-02")),
			Open:      51.5, High: 52.4, Low: 51.2, Close: 52.1, Volume: 4200000,
		},
	}, nil).Once()

	req := &servicePb.DownloadHistoricalDataRequest{
		Symbols:   []string{"BK"},
		StartDate: &internal_pb.Date{Year: 2024, Month: 1, Day: 2},
		EndDate:   &internal_pb.Date{Year: 2024, Month: 3, Day: 28},
		Provider:  "yahoo",
	}
	rsp, err := suite.client.DownloadHistoricalData(ctx, req)
	suite.Require().NoError(err)

	msg, err := rsp.Recv()
	suite.Require().NoError(err)
	suite.Equal("BK", msg.GetSymbol())
	suite.Equal(servicePb.DownloadHistoricalDataResponse_STATUS_OK, msg.GetStatus(), msg.GetError())

	var bars int
	suite.Require().NoError(suite.pgx.QueryRow(ctx,
		`SELECT count(*) FROM ohlc WHERE provider='yahoo' AND symbol='BK'`).Scan(&bars))
	suite.Equal(1, bars)
	suite.marketdata.AssertNumberOfCalls(suite.T(), "GetOHLC", 2)
}

// A London listing is stored under the symbol the user asked for. Stooq
// fetches VOD.L as vod.uk, and a bar labelled with the provider's ticker must
// not end up stored under that ticker.
func (suite *MarketdataTest) TestDownloadHistoricalDataStoresAskedSymbolNotProviderTicker() {
	ctx := context.Background()
	suite.marketdata.On("GetAsset", mock.Anything, "VOD.L").Return(&pb.Asset{Symbol: "VOD.L", Name: "Vodafone Group Plc"}, nil)
	suite.marketdata.On("GetOHLC", mock.Anything, "VOD.L", mock.Anything, mock.Anything).Return([]*pb.OHLC{
		{
			Symbol:    "vod.uk",
			Timestamp: timestamppb.New(mustTime("2024-01-02")),
			Open:      69.1, High: 70.2, Low: 68.9, Close: 69.8, Volume: 51000000,
		},
	}, nil)

	req := &servicePb.DownloadHistoricalDataRequest{
		Symbols:   []string{"VOD.L"},
		StartDate: &internal_pb.Date{Year: 2024, Month: 1, Day: 2},
		EndDate:   &internal_pb.Date{Year: 2024, Month: 1, Day: 3},
		Provider:  "yahoo",
	}
	rsp, err := suite.client.DownloadHistoricalData(ctx, req)
	suite.Require().NoError(err)
	msg, err := rsp.Recv()
	suite.Require().NoError(err)
	suite.Equal(servicePb.DownloadHistoricalDataResponse_STATUS_OK, msg.GetStatus(), msg.GetError())

	var symbols []string
	rows, err := suite.pgx.Query(ctx, `SELECT DISTINCT symbol FROM ohlc WHERE provider='yahoo'`)
	suite.Require().NoError(err)
	for rows.Next() {
		var symbol string
		suite.Require().NoError(rows.Scan(&symbol))
		symbols = append(symbols, symbol)
	}
	suite.Require().NoError(rows.Err())
	suite.Equal([]string{"VOD.L"}, symbols)
}

// A request symbol outside the ticker pattern refuses the whole download
// before any provider call. The refusal names the first invalid symbol's
// 1-based position and the symbol count, and does not repeat the symbol.
func (suite *MarketdataTest) TestDownloadHistoricalDataRefusesInvalidRequestSymbol() {
	for _, bad := range []string{"", "AA PL", "AAPL\n", "<b>x</b>", strings.Repeat("A", 33)} {
		rsp, err := suite.client.DownloadHistoricalData(context.Background(), &servicePb.DownloadHistoricalDataRequest{
			Symbols:   []string{"AAPL", bad, "MSFT", "also bad"},
			StartDate: &internal_pb.Date{Year: 2026, Month: 5, Day: 1},
			EndDate:   &internal_pb.Date{Year: 2026, Month: 5, Day: 3},
			Provider:  "yahoo",
		})
		suite.Require().NoError(err)
		_, err = rsp.Recv()
		st, ok := status.FromError(err)
		suite.Require().True(ok, "%q: %v", bad, err)
		suite.Equal(codes.InvalidArgument, st.Code(), "%q", bad)
		suite.Equal("symbol 2 of 4 isn't a valid ticker (use 1 to 32 letters, digits, or . ^ = _ -)", st.Message(), "%q", bad)
	}
	// The strict mock fails the test on any GetAsset or GetOHLC call.
	suite.marketdata.AssertNotCalled(suite.T(), "GetAsset", mock.Anything)
}

// The provider's ticker is stored and named in a refused-bar sentence, so one
// outside the pattern fails the symbol with the generic sentence, stores no
// asset, and fetches no bars.
func (suite *MarketdataTest) TestDownloadHistoricalDataRefusesInvalidProviderSymbol() {
	const providerSymbol = "XYZ\x1b[31m <b>"
	suite.marketdata.On("GetAsset", mock.Anything, "XYZ").Return(&pb.Asset{Symbol: providerSymbol, Name: "XYZ Corp"}, nil)
	suite.marketdata.On("GetOHLC", mock.Anything, providerSymbol, mock.Anything, mock.Anything).Return([]*pb.OHLC{
		{Timestamp: timestamppb.New(mustTime("2026-05-01")), Open: 0, High: 11, Low: 9, Close: 10.5, Volume: 100},
	}, nil).Maybe()

	rsp, err := suite.client.DownloadHistoricalData(context.Background(), &servicePb.DownloadHistoricalDataRequest{
		Symbols:   []string{"XYZ"},
		StartDate: &internal_pb.Date{Year: 2026, Month: 5, Day: 1},
		EndDate:   &internal_pb.Date{Year: 2026, Month: 5, Day: 3},
		Provider:  "yahoo",
	})
	suite.Require().NoError(err)

	msgs := suite.recvAll(rsp)
	suite.Require().Len(msgs, 1)
	suite.Equal("XYZ", msgs[0].GetSymbol())
	suite.Equal(servicePb.DownloadHistoricalDataResponse_STATUS_ERROR, msgs[0].GetStatus())
	suite.Equal("couldn't download prices for XYZ. Try again in a few minutes, or use another provider with --provider.", msgs[0].GetError())
	suite.marketdata.AssertNotCalled(suite.T(), "GetOHLC", providerSymbol, mock.Anything, mock.Anything)

	var assets int
	suite.Require().NoError(suite.pgx.QueryRow(context.Background(),
		`SELECT count(*) FROM asset WHERE symbol = $1`, providerSymbol).Scan(&assets))
	suite.Zero(assets, "a refused provider ticker must not be stored")
}

// A later-ticker candidate from the search outside the pattern is never
// fetched or stored; with no other candidate the symbol has no price data.
func (suite *MarketdataTest) TestDownloadHistoricalDataSkipsInvalidLaterTicker() {
	ctx := context.Background()
	const badTicker = "META\nX"
	instruments := repository.Instruments{Conn: suite.pgx}
	_, err := instruments.Store(ctx, metaISIN, "Facebook Inc")
	suite.Require().NoError(err)
	_, err = suite.pgx.Exec(ctx, `
		INSERT INTO asset_symbol (isin, symbol, from_date, to_date)
		VALUES ($1, 'FB', '2019-01-01'::date, NULL)`, metaISIN)
	suite.Require().NoError(err)

	suite.marketdata.On("GetAsset", mock.Anything, "FB").Return(&pb.Asset{Symbol: "FB", Name: "Facebook"}, nil)
	suite.marketdata.On("GetAsset", mock.Anything, badTicker).Return(&pb.Asset{Symbol: badTicker, Name: "Meta"}, nil).Maybe()
	suite.marketdata.On("GetOHLC", mock.Anything, "FB", mock.Anything, mock.Anything).Return([]*pb.OHLC{}, nil)
	suite.marketdata.On("GetOHLC", mock.Anything, badTicker, mock.Anything, mock.Anything).Return([]*pb.OHLC{
		{Timestamp: timestamppb.New(mustTime("2019-06-03")), Open: 10, High: 11, Low: 9, Close: 10.5, Volume: 1000},
	}, nil).Maybe()
	suite.marketdata.On("SearchAsset", mock.Anything, metaISIN).Return([]*pb.Asset{
		{Symbol: badTicker, Name: "Meta Platforms, Inc.", ISIN: metaISIN},
	}, nil)

	rsp, err := suite.client.DownloadHistoricalData(ctx, &servicePb.DownloadHistoricalDataRequest{
		Symbols:   []string{"FB"},
		StartDate: &internal_pb.Date{Year: 2019, Month: 1, Day: 1},
		EndDate:   &internal_pb.Date{Year: 2020, Month: 12, Day: 31},
		Provider:  "yahoo",
	})
	suite.Require().NoError(err)

	msgs := suite.recvAll(rsp)
	suite.Require().Len(msgs, 1)
	suite.Equal(servicePb.DownloadHistoricalDataResponse_STATUS_ERROR, msgs[0].GetStatus())
	suite.Equal("no price data for FB", msgs[0].GetError())
	suite.marketdata.AssertNotCalled(suite.T(), "GetOHLC", badTicker, mock.Anything, mock.Anything)

	var bars int
	suite.Require().NoError(suite.pgx.QueryRow(ctx,
		`SELECT count(*) FROM ohlc WHERE symbol = $1`, badTicker).Scan(&bars))
	suite.Zero(bars)
}

// A DB error while looking for a later ticker is not "no price data": the
// user gets the generic download sentence and the DB text stays in the log.
func (suite *MarketdataTest) TestDownloadHistoricalDataReportsLaterTickerDBErrorAsDownloadFailure() {
	ctx := context.Background()
	// Resolve reads asset_symbol; without it every lookup is a DB error.
	// SetupTest's Recreate creates the table again for the next test.
	_, err := suite.pgx.Exec(ctx, `DROP TABLE asset_symbol`)
	suite.Require().NoError(err)

	suite.marketdata.On("GetAsset", mock.Anything, "FB").Return(&pb.Asset{Symbol: "FB", Name: "Facebook"}, nil)
	suite.marketdata.On("GetOHLC", mock.Anything, "FB", mock.Anything, mock.Anything).Return([]*pb.OHLC{}, nil)

	rsp, err := suite.client.DownloadHistoricalData(ctx, &servicePb.DownloadHistoricalDataRequest{
		Symbols:   []string{"FB"},
		StartDate: &internal_pb.Date{Year: 2019, Month: 1, Day: 1},
		EndDate:   &internal_pb.Date{Year: 2020, Month: 12, Day: 31},
		Provider:  "yahoo",
	})
	suite.Require().NoError(err)

	msgs := suite.recvAll(rsp)
	suite.Require().Len(msgs, 1)
	suite.Equal(servicePb.DownloadHistoricalDataResponse_STATUS_ERROR, msgs[0].GetStatus())
	suite.Equal("couldn't download prices for FB. Try again in a few minutes, or use another provider with --provider.", msgs[0].GetError())
	suite.NotContains(msgs[0].GetError(), "asset_symbol")
}

// A ticker that maps to two instruments is refused with the provider and
// the ticker, not guessed and not reported as missing prices.
func (suite *MarketdataTest) TestDownloadHistoricalDataRefusesAmbiguousSymbol() {
	ctx := context.Background()
	instruments := repository.Instruments{Conn: suite.pgx}
	for _, isin := range []string{"GB00BH4HKS39", "US92857W3088"} {
		_, err := instruments.Store(ctx, isin, "Vodafone")
		suite.Require().NoError(err)
		_, err = suite.pgx.Exec(ctx, `
			INSERT INTO asset_symbol (isin, symbol, from_date, to_date)
			VALUES ($1, 'VOD', '2019-01-01'::date, NULL)`, isin)
		suite.Require().NoError(err)
	}
	suite.marketdata.On("GetAsset", mock.Anything, "VOD").Return(&pb.Asset{Symbol: "VOD", Name: "Vodafone"}, nil)
	suite.marketdata.On("GetOHLC", mock.Anything, "VOD", mock.Anything, mock.Anything).Return([]*pb.OHLC{}, nil)

	rsp, err := suite.client.DownloadHistoricalData(ctx, &servicePb.DownloadHistoricalDataRequest{
		Symbols:   []string{"VOD"},
		StartDate: &internal_pb.Date{Year: 2024, Month: 1, Day: 1},
		EndDate:   &internal_pb.Date{Year: 2024, Month: 3, Day: 31},
		Provider:  "YAHOO",
	})
	suite.Require().NoError(err)

	msgs := suite.recvAll(rsp)
	suite.Require().Len(msgs, 1)
	suite.Equal("VOD", msgs[0].GetSymbol())
	suite.Equal(servicePb.DownloadHistoricalDataResponse_STATUS_ERROR, msgs[0].GetStatus())
	suite.Equal("yahoo has more than one instrument for VOD", msgs[0].GetError())
}

func mustTime(day string) time.Time {
	t, err := time.Parse("2006-01-02", day)
	if err != nil {
		panic(err)
	}
	return t
}

func (suite *MarketdataTest) TestGetOHLCReadsStoredBars() {
	ctx := context.Background()
	assetRepo := repository.Asset{Conn: suite.pgx}
	suite.Require().NoError(assetRepo.Store(ctx, "AAPL", "Apple"))
	ohlcRepo := repository.OHLC{Conn: suite.pgx}
	day := time.Date(2021, 3, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		suite.Require().NoError(ohlcRepo.Store(ctx, "yahoo", "AAPL", day.AddDate(0, 0, i), 1, 2, 0.5, float64(100+i), 10))
	}

	rsp, err := suite.client.GetOHLC(ctx, &servicePb.GetOHLCRequest{
		Symbols: []string{"AAPL"},
		AsOf:    timestamppb.New(day.AddDate(0, 0, 2)),
		Limit:   2,
	})
	suite.Require().NoError(err)
	suite.Require().Len(rsp.Bars, 2)
	suite.InDelta(101, rsp.Bars[0].Close, 0.0001)
	suite.InDelta(102, rsp.Bars[1].Close, 0.0001)
}

// GetOHLC for BRK.B reads the bars stored as BRKB.
func (suite *MarketdataTest) TestGetOHLCReadsShareClassUnderStoredName() {
	ctx := context.Background()
	assetRepo := repository.Asset{Conn: suite.pgx}
	suite.Require().NoError(assetRepo.Store(ctx, "BRKB", "Berkshire Hathaway B"))
	ohlcRepo := repository.OHLC{Conn: suite.pgx}
	day := time.Date(2021, 3, 1, 0, 0, 0, 0, time.UTC)
	suite.Require().NoError(ohlcRepo.Store(ctx, "yahoo", "BRKB", day, 1, 2, 0.5, 250, 10))

	rsp, err := suite.client.GetOHLC(ctx, &servicePb.GetOHLCRequest{
		Symbols: []string{"BRK.B"},
		AsOf:    timestamppb.New(day),
	})
	suite.Require().NoError(err)
	suite.Require().Len(rsp.Bars, 1)
	suite.Equal("BRKB", rsp.Bars[0].GetSymbol())
	suite.InDelta(250, rsp.Bars[0].GetClose(), 0.0001)
}

func (suite *MarketdataTest) TestGetOHLCRejectsEmptySymbols() {
	_, err := suite.client.GetOHLC(context.Background(), &servicePb.GetOHLCRequest{
		AsOf: timestamppb.Now(),
	})
	suite.Require().Error(err)
	st, ok := status.FromError(err)
	suite.Require().True(ok)
	suite.Equal(codes.InvalidArgument, st.Code())
}

// aaplAdjusted is Yahoo's split-adjusted AAPL around the 4:1 split on
// 2020-08-31 (recorded chart response, as in yahoo_split_test.go).
func aaplAdjusted() []*pb.OHLC {
	bar := func(day string, o, h, l, c float64, v uint64) *pb.OHLC {
		return &pb.OHLC{Timestamp: timestamppb.New(mustTime(day)), Open: o, High: h, Low: l, Close: c, Volume: v}
	}
	return []*pb.OHLC{
		bar("2020-08-27", 127.1425, 127.485, 123.8325, 125.01, 155552400),
		bar("2020-08-28", 126.0125, 126.4425, 124.5775, 124.8075, 187630000),
		bar("2020-08-31", 127.58, 131.0, 126.0, 129.04, 225702700),
		bar("2020-09-01", 132.76, 134.8, 130.53, 134.18, 151948100),
	}
}

// The download path re-scales after a split the same way ingest does. With
// 08-27 and 08-28 stored on the pre-split scale, a download from 08-28 sees
// the overlap close moved, refetches from the first stored day, and replaces
// it, so 08-27 is not left on the old scale and 08-28 reads 124.8075.
func (suite *MarketdataTest) TestDownloadHistoricalDataRescalesAfterSplit() {
	ctx := context.Background()
	adjusted := aaplAdjusted()
	assets := repository.Asset{Conn: suite.pgx}
	ohlc := repository.OHLC{Conn: suite.pgx}
	suite.Require().NoError(assets.Store(ctx, "AAPL", "Apple Inc."))
	for _, b := range adjusted[:2] {
		suite.Require().NoError(ohlc.Store(ctx, "yahoo", "AAPL", b.Timestamp.AsTime(),
			b.Open*4, b.High*4, b.Low*4, b.Close*4, b.Volume/4))
	}

	onDay := func(day string) interface{} {
		return mock.MatchedBy(func(t time.Time) bool { return t.UTC().Format(time.DateOnly) == day })
	}
	suite.marketdata.On("GetAsset", mock.Anything, "AAPL").Return(&pb.Asset{Symbol: "AAPL", Name: "Apple Inc."}, nil)
	suite.marketdata.On("GetOHLC", mock.Anything, "AAPL", onDay("2020-08-28"), mock.Anything).Return(adjusted[1:], nil).Once()
	suite.marketdata.On("GetOHLC", mock.Anything, "AAPL", onDay("2020-08-27"), mock.Anything).Return(adjusted, nil).Once()

	rsp, err := suite.client.DownloadHistoricalData(ctx, &servicePb.DownloadHistoricalDataRequest{
		Symbols:   []string{"AAPL"},
		StartDate: &internal_pb.Date{Year: 2020, Month: 8, Day: 28},
		EndDate:   &internal_pb.Date{Year: 2020, Month: 9, Day: 1},
		Provider:  "yahoo",
	})
	suite.Require().NoError(err)
	msgs := suite.recvAll(rsp)
	suite.Require().Len(msgs, 1)
	suite.Equal(servicePb.DownloadHistoricalDataResponse_STATUS_OK, msgs[0].GetStatus(), msgs[0].GetError())

	bars, err := ohlc.List(ctx, "yahoo", []string{"AAPL"}, mustTime("2020-09-01"), 0)
	suite.Require().NoError(err)
	suite.Require().Len(bars, len(adjusted))
	for i, b := range bars {
		suite.InDelta(adjusted[i].Close, b.Close, 0.0001, b.Timestamp.AsTime().Format(time.DateOnly))
	}
	closeAug28, err := ohlc.Close(ctx, "yahoo", "AAPL", mustTime("2020-08-28"))
	suite.Require().NoError(err)
	suite.InDelta(124.8075, closeAug28, 0.0001)
}

// The first ask is Yahoo's Not Found, and the retry runs out of time. A
// timeout says nothing about whether the provider has bars for the symbol,
// so the symbol fails with the download error and no later ticker is looked
// up for it.
func (suite *MarketdataTest) TestDownloadHistoricalDataFailsWhenTheRetryTimesOut() {
	ctx := context.Background()
	instruments := repository.Instruments{Conn: suite.pgx}
	_, err := instruments.Store(ctx, metaISIN, "Facebook Inc")
	suite.Require().NoError(err)
	_, err = suite.pgx.Exec(ctx, `
		INSERT INTO asset_symbol (isin, symbol, from_date, to_date)
		VALUES ($1, 'FB', '2019-01-01'::date, NULL)`, metaISIN)
	suite.Require().NoError(err)

	suite.marketdata.On("GetAsset", mock.Anything, "FB").Return(&pb.Asset{Symbol: "FB", Name: "Meta Platforms, Inc."}, nil)
	suite.marketdata.On("GetOHLC", mock.Anything, "FB", mock.Anything, mock.Anything).
		Return(nil, yahooNotFound("FB")).Once()
	suite.marketdata.On("GetOHLC", mock.Anything, "FB", mock.Anything, mock.Anything).
		Return(nil, fmt.Errorf("fail to get OHLC data for symbol FB: %w", context.DeadlineExceeded)).Once()
	// The later-ticker lookup would ask for these; it must not run.
	suite.marketdata.On("SearchAsset", mock.Anything, metaISIN).Return([]*pb.Asset{
		{Symbol: "META", Name: "Meta Platforms, Inc.", ISIN: metaISIN},
	}, nil).Maybe()
	suite.marketdata.On("GetAsset", mock.Anything, "META").
		Return(&pb.Asset{Symbol: "META", Name: "Meta Platforms, Inc.", ISIN: metaISIN}, nil).Maybe()
	suite.marketdata.On("GetOHLC", mock.Anything, "META", mock.Anything, mock.Anything).Return([]*pb.OHLC{
		{
			Timestamp: timestamppb.New(mustTime("2019-06-03")),
			Open:      10, High: 11, Low: 9, Close: 10.5, Volume: 1000,
		},
	}, nil).Maybe()

	req := &servicePb.DownloadHistoricalDataRequest{
		Symbols:   []string{"FB"},
		StartDate: &internal_pb.Date{Year: 2019, Month: 1, Day: 1},
		EndDate:   &internal_pb.Date{Year: 2020, Month: 12, Day: 31},
		Provider:  "yahoo",
	}
	rsp, err := suite.client.DownloadHistoricalData(ctx, req)
	suite.Require().NoError(err)
	msgs := suite.recvAll(rsp)
	suite.Require().Len(msgs, 1)
	suite.Equal("FB", msgs[0].GetSymbol())
	suite.Equal(servicePb.DownloadHistoricalDataResponse_STATUS_ERROR, msgs[0].GetStatus())
	suite.Equal("couldn't download prices for FB. Try again in a few minutes, "+
		"or use another provider with --provider.", msgs[0].GetError())

	suite.marketdata.AssertNotCalled(suite.T(), "SearchAsset", mock.Anything)
	suite.marketdata.AssertNotCalled(suite.T(), "GetAsset", "META")
	suite.marketdata.AssertNotCalled(suite.T(), "GetOHLC", "META", mock.Anything, mock.Anything)
	suite.marketdata.AssertNumberOfCalls(suite.T(), "GetOHLC", 2)
	aliases, err := instruments.ListAliases(ctx, metaISIN)
	suite.Require().NoError(err)
	for _, alias := range aliases {
		suite.NotEqual("META", alias.Symbol, "no later ticker is recorded")
	}
}

// The first ask is Yahoo's Not Found, and the client cancels the download
// before the retry. The download stops with the
// cancelled status: no later ticker is looked up for the symbol, and no
// alias, asset or bars are recorded for one.
func (suite *MarketdataTest) TestDownloadHistoricalDataStopsWhenCancelledBeforeTheRetry() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	suite.downloadEndsBeforeTheRetry(ctx, cancel, codes.Canceled, "context canceled")
}

// As above, with the client's deadline passing instead of a cancel.
func (suite *MarketdataTest) TestDownloadHistoricalDataStopsWhenTheDeadlinePassesBeforeTheRetry() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// Only the code: the stream can end on the deadline or on the RST_STREAM
	// that follows it, so the message isn't stable.
	suite.downloadEndsBeforeTheRetry(ctx, func() {}, codes.DeadlineExceeded, "")
}

// ohlcRetryWait is longer than the download's pause before its retry, so a
// retry or later-ticker lookup that wrongly went on has run by the time the
// test checks.
const ohlcRetryWait = 400 * time.Millisecond

// downloadEndsBeforeTheRetry downloads FB under downloadCtx. The first ask
// is Yahoo's Not Found and ends downloadCtx (end cancels it; a deadline ends it
// by itself), then checks the stream ends with code (and text, unless it is
// empty) and that nothing of the later ticker META was asked for or stored.
func (suite *MarketdataTest) downloadEndsBeforeTheRetry(downloadCtx context.Context, end func(),
	code codes.Code, text string,
) {
	ctx := context.Background()
	instruments := repository.Instruments{Conn: suite.pgx}
	_, err := instruments.Store(ctx, metaISIN, "Facebook Inc")
	suite.Require().NoError(err)
	_, err = suite.pgx.Exec(ctx, `
		INSERT INTO asset_symbol (isin, symbol, from_date, to_date)
		VALUES ($1, 'FB', '2019-01-01'::date, NULL)`, metaISIN)
	suite.Require().NoError(err)

	suite.marketdata.On("GetAsset", mock.Anything, "FB").Return(&pb.Asset{Symbol: "FB", Name: "Meta Platforms, Inc."}, nil)
	suite.marketdata.On("GetOHLC", mock.Anything, "FB", mock.Anything, mock.Anything).
		Run(func(mock.Arguments) {
			end()
			<-downloadCtx.Done()
			// Let the end reach the server's stream ctx before the retry.
			time.Sleep(50 * time.Millisecond)
		}).
		Return(nil, yahooNotFound("FB")).Once()
	// The later-ticker lookup would ask for these; it must not run.
	suite.marketdata.On("SearchAsset", mock.Anything, mock.Anything).Return([]*pb.Asset{
		{Symbol: "META", Name: "Meta Platforms, Inc.", ISIN: metaISIN},
	}, nil).Maybe()
	suite.marketdata.On("GetAsset", mock.Anything, "META").
		Return(&pb.Asset{Symbol: "META", Name: "Meta Platforms, Inc.", ISIN: metaISIN}, nil).Maybe()
	suite.marketdata.On("GetOHLC", mock.Anything, "META", mock.Anything, mock.Anything).Return([]*pb.OHLC{
		{
			Timestamp: timestamppb.New(mustTime("2019-06-03")),
			Open:      10, High: 11, Low: 9, Close: 10.5, Volume: 1000,
		},
	}, nil).Maybe()

	req := &servicePb.DownloadHistoricalDataRequest{
		Symbols:   []string{"FB"},
		StartDate: &internal_pb.Date{Year: 2019, Month: 1, Day: 1},
		EndDate:   &internal_pb.Date{Year: 2020, Month: 12, Day: 31},
		Provider:  "yahoo",
	}
	rsp, err := suite.client.DownloadHistoricalData(downloadCtx, req)
	suite.Require().NoError(err)
	var streamErr error
	for {
		msg, err := rsp.Recv()
		if err != nil {
			streamErr = err
			break
		}
		suite.Failf("no symbol is reported once the download ended", "%s: %s %s",
			msg.GetSymbol(), msg.GetStatus(), msg.GetError())
	}
	st, ok := status.FromError(streamErr)
	suite.Require().True(ok, "want a gRPC status, got %v", streamErr)
	suite.Equal(code, st.Code())
	if text != "" {
		suite.Equal(text, st.Message())
	}

	// The client sees the status as soon as its ctx ends; the server's
	// worker may still be on the symbol. Give it past the retry pause.
	time.Sleep(ohlcRetryWait)

	suite.marketdata.AssertNotCalled(suite.T(), "SearchAsset", mock.Anything)
	suite.marketdata.AssertNotCalled(suite.T(), "GetAsset", "META")
	suite.marketdata.AssertNotCalled(suite.T(), "GetOHLC", "META", mock.Anything, mock.Anything)
	suite.marketdata.AssertNumberOfCalls(suite.T(), "GetOHLC", 1)

	aliases, err := instruments.ListAliases(ctx, metaISIN)
	suite.Require().NoError(err)
	suite.Require().Len(aliases, 1, "no alias is stored")
	suite.Equal("FB", aliases[0].Symbol)
	var metaAssets, bars int
	suite.Require().NoError(suite.pgx.QueryRow(ctx,
		`SELECT count(*) FROM asset WHERE symbol='META'`).Scan(&metaAssets))
	suite.Require().NoError(suite.pgx.QueryRow(ctx,
		`SELECT count(*) FROM ohlc WHERE symbol IN ('FB', 'META')`).Scan(&bars))
	suite.Zero(metaAssets, "no later-ticker asset is recorded")
	suite.Zero(bars, "no later-ticker bars are recorded")
}
