package api_test

import (
	"context"
	"io"

	internal_pb "github.com/lhjnilsson/foreverbull/pkg/pb"
	pb "github.com/lhjnilsson/foreverbull/pkg/pb/finance"
	servicePb "github.com/lhjnilsson/foreverbull/pkg/pb/finance/service"
	"github.com/stretchr/testify/mock"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (suite *MarketdataTest) downloadOne(ctx context.Context, symbol string) *servicePb.DownloadHistoricalDataResponse {
	rsp, err := suite.client.DownloadHistoricalData(ctx, &servicePb.DownloadHistoricalDataRequest{
		Symbols:   []string{symbol},
		StartDate: &internal_pb.Date{Year: 2019, Month: 1, Day: 1},
		EndDate:   &internal_pb.Date{Year: 2020, Month: 12, Day: 31},
		Provider:  "yahoo",
	})
	suite.Require().NoError(err)
	msg, err := rsp.Recv()
	suite.Require().NoError(err)
	_, err = rsp.Recv()
	suite.Require().ErrorIs(err, io.EOF)
	return msg
}

func (suite *MarketdataTest) barSymbols(ctx context.Context) []string {
	rows, err := suite.pgx.Query(ctx, `SELECT DISTINCT symbol FROM ohlc ORDER BY symbol`)
	suite.Require().NoError(err)
	defer rows.Close()
	symbols := make([]string, 0)
	for rows.Next() {
		var symbol string
		suite.Require().NoError(rows.Scan(&symbol))
		symbols = append(symbols, symbol)
	}
	suite.Require().NoError(rows.Err())
	return symbols
}

func oneBar() []*pb.OHLC {
	return []*pb.OHLC{{Timestamp: timestamppb.New(mustTime("2019-06-03")), Open: 10, High: 11, Low: 9, Close: 10.5, Volume: 1000}}
}

// The provider answers FB's lookup as META, so the bars are fetched and
// stored under META. The OK response for FB names META as the later ticker,
// the same as when FB has no bars and META is found by the ISIN, so create
// records and shows it instead of reading META's prices without a word.
func (suite *MarketdataTest) TestDownloadHistoricalDataNamesTheTickerTheProviderAnsweredAs() {
	ctx := context.Background()
	suite.marketdata.On("GetAsset", mock.Anything, "FB").Return(&pb.Asset{Symbol: "META", Name: "Meta Platforms, Inc."}, nil)
	suite.marketdata.On("GetOHLC", mock.Anything, "META", mock.Anything, mock.Anything).Return(oneBar(), nil)

	msg := suite.downloadOne(ctx, "FB")
	suite.Equal("FB", msg.GetSymbol())
	suite.Equal(servicePb.DownloadHistoricalDataResponse_STATUS_OK, msg.GetStatus(), msg.GetError())
	suite.Require().NotNil(msg.GetLaterTicker())
	suite.Equal("META", msg.GetLaterTicker().GetSymbol())
	suite.Equal([]string{"META"}, suite.barSymbols(ctx))
	suite.marketdata.AssertNotCalled(suite.T(), "GetOHLC", mock.Anything, "FB", mock.Anything, mock.Anything)
}

// Another spelling of the asked ticker is the asked ticker: Yahoo answers
// aapl as AAPL and BRKB as BRK-B. No later ticker is named.
func (suite *MarketdataTest) TestDownloadHistoricalDataNamesNoLaterTickerForAnotherSpelling() {
	ctx := context.Background()
	for _, c := range []struct{ asked, answered string }{{"aapl", "AAPL"}, {"BRKB", "BRK-B"}} {
		suite.marketdata.On("GetAsset", mock.Anything, c.asked).Return(&pb.Asset{Symbol: c.answered, Name: c.answered}, nil)
		suite.marketdata.On("GetOHLC", mock.Anything, c.answered, mock.Anything, mock.Anything).Return(oneBar(), nil)

		msg := suite.downloadOne(ctx, c.asked)
		suite.Equal(servicePb.DownloadHistoricalDataResponse_STATUS_OK, msg.GetStatus(), c.asked)
		suite.Nil(msg.GetLaterTicker(), c.asked)
	}
}

// The provider answers FB as META and has no bars under META. A later
// ticker found another way would contradict the one it named, so FB fails
// with no price data, no other ticker is looked up, and nothing is stored.
func (suite *MarketdataTest) TestDownloadHistoricalDataFailsWhenTheTickerTheProviderAnsweredAsHasNoBars() {
	ctx := context.Background()
	suite.marketdata.On("GetAsset", mock.Anything, "FB").Return(&pb.Asset{Symbol: "META", Name: "Meta Platforms, Inc."}, nil)
	suite.marketdata.On("GetOHLC", mock.Anything, "META", mock.Anything, mock.Anything).Return(nil, yahooNotFound("META"))

	msg := suite.downloadOne(ctx, "FB")
	suite.requireOnlyText(msg, "FB", "no price data for FB")
	suite.Nil(msg.GetLaterTicker())
	suite.Empty(suite.barSymbols(ctx))
	suite.marketdata.AssertNotCalled(suite.T(), "SearchAsset", mock.Anything, mock.Anything)
}
