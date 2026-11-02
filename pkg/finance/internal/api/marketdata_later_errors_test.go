package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"syscall"
	"testing"
	"time"

	"github.com/alpacahq/alpaca-trade-api-go/v3/alpaca"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/internal/test_helper"
	"github.com/lhjnilsson/foreverbull/pkg/finance/internal/repository"
	"github.com/lhjnilsson/foreverbull/pkg/finance/internal/suppliers/marketdata"
	"github.com/lhjnilsson/foreverbull/pkg/finance/supplier"
	pb "github.com/lhjnilsson/foreverbull/pkg/pb/finance"
	servicePb "github.com/lhjnilsson/foreverbull/pkg/pb/finance/service"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// renamedISIN is the instrument FB was renamed from; META is its current
// ticker, found by the search.
const renamedISIN = "US30303M1027"

// ohlcAnswer is one GetOHLC result for FB.
type ohlcAnswer struct {
	bars []*pb.OHLC
	err  error
}

// errCancelDownload marks an answer that cancels the download before returning
// its error, as a client that goes away mid-request does.
var errCancelDownload = errors.New("cancel the download")

func yahooNoChart() error {
	return fmt.Errorf("fail to get OHLC data for symbol FB: No data found, symbol may be delisted: %w", marketdata.ErrNoBars)
}

func stooqNoData() error {
	return fmt.Errorf("stooq GetOHLC FB.US: %w", fmt.Errorf("missing file: no data: %w", marketdata.ErrNoBars))
}

func connectionRefused() error {
	return fmt.Errorf("fail to get OHLC data for symbol FB: error making request: %w", &url.Error{
		Op: "Get", URL: "https://query2.finance.yahoo.com/v8/finance/chart/FB",
		Err: syscall.ECONNREFUSED,
	})
}

func alpacaStatus(status int) error {
	return fmt.Errorf("error getting bars: %w", &alpaca.APIError{StatusCode: status, Message: http.StatusText(status)})
}

// Only the provider's answer that FB has no bars (ErrNoBars or an empty
// answer, on the first ask and the one retry) looks up the later ticker
// META. Every other failure, and a missing price on either ask that the
// retry doesn't replace with bars, fails FB with its cause: SearchAsset is
// never called, and no bars, asset or alias are stored for META. A retry
// that has bars stores them under FB without a lookup.
func TestDownloadSymbolLooksUpLaterTickerOnlyOnNoBars(t *testing.T) {
	test_helper.SetupEnvironment(t, &test_helper.Containers{Postgres: true})
	pool, err := pgxpool.New(context.Background(), environment.GetPostgresURL())
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	failed := downloadFailedError("FB").Error()
	missing := &marketdata.MissingPriceError{Provider: "yahoo", Symbol: "FB", Days: []time.Time{
		time.Date(2019, 6, 4, 0, 0, 0, 0, time.UTC),
	}}

	cases := []struct {
		name     string
		provider string
		// answers are FB's GetOHLC results in order; a second one is the retry.
		answers []ohlcAnswer
		// wantLookup is true when the later-ticker lookup must run.
		wantLookup bool
		// wantStatus is the one response sent for FB; STATUS_UNSPECIFIED
		// means none is.
		wantStatus servicePb.DownloadHistoricalDataResponse_Status
		wantError  string
		wantBlock  bool
		// wantFBBars is true when FB's own bars from the retry are stored.
		wantFBBars bool
	}{
		{name: "yahoo not found", provider: "yahoo",
			answers:    []ohlcAnswer{{err: yahooNoChart()}, {err: yahooNoChart()}},
			wantLookup: true, wantStatus: servicePb.DownloadHistoricalDataResponse_STATUS_OK},
		{name: "yahoo empty answer", provider: "yahoo",
			answers:    []ohlcAnswer{{bars: []*pb.OHLC{}}, {bars: []*pb.OHLC{}}},
			wantLookup: true, wantStatus: servicePb.DownloadHistoricalDataResponse_STATUS_OK},
		{name: "yahoo not found then empty", provider: "yahoo",
			answers:    []ohlcAnswer{{err: yahooNoChart()}, {bars: []*pb.OHLC{}}},
			wantLookup: true, wantStatus: servicePb.DownloadHistoricalDataResponse_STATUS_OK},
		{name: "yahoo 500", provider: "yahoo",
			answers:    []ohlcAnswer{{err: errors.New("fail to get OHLC data for symbol FB: HTTP 500 Internal Server Error")}},
			wantStatus: servicePb.DownloadHistoricalDataResponse_STATUS_ERROR, wantError: failed},
		{name: "yahoo 401", provider: "yahoo",
			answers:    []ohlcAnswer{{err: errors.New("fail to get OHLC data for symbol FB: HTTP 401 Unauthorized")}},
			wantStatus: servicePb.DownloadHistoricalDataResponse_STATUS_ERROR, wantError: failed},
		{name: "yahoo 429 after its retries", provider: "yahoo",
			answers:    []ohlcAnswer{{err: errors.New("fail to get OHLC data for symbol FB: HTTP 429 Too Many Requests")}},
			wantStatus: servicePb.DownloadHistoricalDataResponse_STATUS_ERROR, wantError: failed},
		{name: "yahoo connection refused", provider: "yahoo",
			answers:    []ohlcAnswer{{err: connectionRefused()}},
			wantStatus: servicePb.DownloadHistoricalDataResponse_STATUS_ERROR, wantError: failed},
		{name: "yahoo timeout", provider: "yahoo",
			answers:    []ohlcAnswer{{err: fmt.Errorf("fail to get OHLC data for symbol FB: %w", context.DeadlineExceeded)}},
			wantStatus: servicePb.DownloadHistoricalDataResponse_STATUS_ERROR,
			wantError:  (&marketdata.TimeoutError{Provider: "yahoo", Symbol: "FB"}).Error()},
		{name: "yahoo bot check", provider: "yahoo",
			answers:   []ohlcAnswer{{err: &marketdata.BlockedError{Provider: "yahoo", Symbol: "FB"}}},
			wantBlock: true},
		{name: "yahoo not found then bot check", provider: "yahoo",
			answers:   []ohlcAnswer{{err: yahooNoChart()}, {err: &marketdata.BlockedError{Provider: "yahoo", Symbol: "FB"}}},
			wantBlock: true},
		{name: "yahoo request cancelled while the download runs", provider: "yahoo",
			answers:    []ohlcAnswer{{err: fmt.Errorf("fail to get OHLC data for symbol FB: %w", context.Canceled)}},
			wantStatus: servicePb.DownloadHistoricalDataResponse_STATUS_ERROR, wantError: failed},
		{name: "download cancelled", provider: "yahoo",
			answers: []ohlcAnswer{{err: errCancelDownload}}},
		{name: "yahoo not found then 500", provider: "yahoo",
			answers: []ohlcAnswer{{err: yahooNoChart()},
				{err: errors.New("fail to get OHLC data for symbol FB: HTTP 500 Internal Server Error")}},
			wantStatus: servicePb.DownloadHistoricalDataResponse_STATUS_ERROR, wantError: failed},
		{name: "yahoo not found then timeout", provider: "yahoo",
			answers: []ohlcAnswer{{err: yahooNoChart()},
				{err: fmt.Errorf("fail to get OHLC data for symbol FB: %w", context.DeadlineExceeded)}},
			wantStatus: servicePb.DownloadHistoricalDataResponse_STATUS_ERROR, wantError: failed},
		{name: "yahoo missing price", provider: "yahoo",
			answers:    []ohlcAnswer{{err: missing}, {err: missing}},
			wantStatus: servicePb.DownloadHistoricalDataResponse_STATUS_ERROR, wantError: "yahoo sent no price for FB on 2019-06-04"},
		{name: "yahoo missing price then not found", provider: "yahoo",
			answers:    []ohlcAnswer{{err: missing}, {err: yahooNoChart()}},
			wantStatus: servicePb.DownloadHistoricalDataResponse_STATUS_ERROR, wantError: "yahoo sent no price for FB on 2019-06-04"},
		{name: "yahoo missing price then empty", provider: "yahoo",
			answers:    []ohlcAnswer{{err: missing}, {bars: []*pb.OHLC{}}},
			wantStatus: servicePb.DownloadHistoricalDataResponse_STATUS_ERROR, wantError: "yahoo sent no price for FB on 2019-06-04"},
		{name: "yahoo not found then missing price", provider: "yahoo",
			answers:    []ohlcAnswer{{err: yahooNoChart()}, {err: missing}},
			wantStatus: servicePb.DownloadHistoricalDataResponse_STATUS_ERROR, wantError: "yahoo sent no price for FB on 2019-06-04"},
		{name: "yahoo empty then bars", provider: "yahoo",
			answers:    []ohlcAnswer{{bars: []*pb.OHLC{}}, {bars: fbBars()}},
			wantStatus: servicePb.DownloadHistoricalDataResponse_STATUS_OK, wantFBBars: true},
		{name: "yahoo missing price then bars", provider: "yahoo",
			answers:    []ohlcAnswer{{err: missing}, {bars: fbBars()}},
			wantStatus: servicePb.DownloadHistoricalDataResponse_STATUS_OK, wantFBBars: true},
		{name: "stooq no data", provider: "stooq",
			answers:    []ohlcAnswer{{err: stooqNoData()}, {err: stooqNoData()}},
			wantLookup: true, wantStatus: servicePb.DownloadHistoricalDataResponse_STATUS_OK},
		{name: "stooq http 404", provider: "stooq",
			answers:    []ohlcAnswer{{err: errors.New("stooq GetOHLC FB.US: missing file: HTTP 404 Not Found")}},
			wantStatus: servicePb.DownloadHistoricalDataResponse_STATUS_ERROR, wantError: failed},
		{name: "stooq 500", provider: "stooq",
			answers:    []ohlcAnswer{{err: errors.New("stooq GetOHLC FB.US: missing file: HTTP 500 Internal Server Error")}},
			wantStatus: servicePb.DownloadHistoricalDataResponse_STATUS_ERROR, wantError: failed},
		{name: "stooq daily limit", provider: "stooq",
			answers:    []ohlcAnswer{{err: errors.New("stooq GetOHLC FB.US: daily hits limit exceeded")}},
			wantStatus: servicePb.DownloadHistoricalDataResponse_STATUS_ERROR, wantError: failed},
		{name: "alpaca empty answer", provider: "alpaca",
			answers:    []ohlcAnswer{{bars: []*pb.OHLC{}}, {bars: []*pb.OHLC{}}},
			wantLookup: true, wantStatus: servicePb.DownloadHistoricalDataResponse_STATUS_OK},
		{name: "alpaca 401", provider: "alpaca",
			answers:    []ohlcAnswer{{err: alpacaStatus(http.StatusUnauthorized)}},
			wantStatus: servicePb.DownloadHistoricalDataResponse_STATUS_ERROR, wantError: failed},
		{name: "alpaca 429 after its retries", provider: "alpaca",
			answers:    []ohlcAnswer{{err: alpacaStatus(http.StatusTooManyRequests)}},
			wantStatus: servicePb.DownloadHistoricalDataResponse_STATUS_ERROR, wantError: failed},
		{name: "alpaca 500", provider: "alpaca",
			answers:    []ohlcAnswer{{err: alpacaStatus(http.StatusInternalServerError)}},
			wantStatus: servicePb.DownloadHistoricalDataResponse_STATUS_ERROR, wantError: failed},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, repository.Recreate(context.Background(), pool))
			storeRenamedFB(t, pool)

			meta := supplier.NewMockMarketdata(t)
			meta.On("GetAsset", mock.Anything, "FB").Return(&pb.Asset{Symbol: "FB", Name: "Facebook"}, nil)
			meta.On("SearchAsset", mock.Anything, renamedISIN).
				Return([]*pb.Asset{{Symbol: "META", Name: "Meta Platforms, Inc.", ISIN: renamedISIN}}, nil).Maybe()
			meta.On("GetAsset", mock.Anything, "META").
				Return(&pb.Asset{Symbol: "META", Name: "Meta Platforms, Inc.", ISIN: renamedISIN}, nil).Maybe()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			bars := supplier.NewMockMarketdata(t)
			for _, answer := range tc.answers {
				call := bars.On("GetOHLC", mock.Anything, "FB", mock.Anything, mock.Anything).Once()
				if errors.Is(answer.err, errCancelDownload) {
					call.Run(func(mock.Arguments) { cancel() })
					call.Return(nil, context.Canceled)
					continue
				}
				call.Return(answer.bars, answer.err)
			}
			bars.On("GetOHLC", mock.Anything, "META", mock.Anything, mock.Anything).Return([]*pb.OHLC{
				{Timestamp: timestamppb.New(time.Date(2019, 6, 3, 0, 0, 0, 0, time.UTC)), Open: 10, High: 11, Low: 9, Close: 10.5, Volume: 1000},
			}, nil).Maybe()

			stream := &countingStream{}
			fs := NewMarketdataServer(pool, meta)
			start := time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC)
			end := time.Date(2020, 12, 31, 0, 0, 0, 0, time.UTC)
			blocked := fs.downloadSymbol(ctx, newDownloadSender(stream, func() {}), "FB", start, &end, tc.provider,
				repository.Asset{Conn: pool}, repository.OHLC{Conn: pool}, bars)

			if tc.wantBlock {
				require.Equal(t, &marketdata.BlockedError{Provider: "yahoo", Symbol: "FB"}, blocked)
				require.Equal(t, "Yahoo blocked the download for FB (it returned a bot-check page instead of data). "+
					"Retry with --provider stooq.", blocked.Error())
			} else {
				require.Nil(t, blocked)
			}
			bars.AssertNumberOfCalls(t, "GetOHLC", len(tc.answers)+lookupCalls(tc.wantLookup))
			meta.AssertNumberOfCalls(t, "SearchAsset", lookupCalls(tc.wantLookup))

			if tc.wantStatus == servicePb.DownloadHistoricalDataResponse_STATUS_UNSPECIFIED {
				require.Empty(t, stream.sent, "nothing is reported for a symbol that stopped with the download")
			} else {
				require.Len(t, stream.sent, 1)
				require.Equal(t, "FB", stream.sent[0].GetSymbol())
				require.Equal(t, tc.wantStatus, stream.sent[0].GetStatus())
				require.Equal(t, tc.wantError, stream.sent[0].GetError())
			}

			var metaBars, metaAssets, metaAliases int
			require.NoError(t, pool.QueryRow(context.Background(),
				`SELECT count(*) FROM ohlc WHERE symbol = 'META'`).Scan(&metaBars))
			require.NoError(t, pool.QueryRow(context.Background(),
				`SELECT count(*) FROM asset WHERE symbol = 'META'`).Scan(&metaAssets))
			require.NoError(t, pool.QueryRow(context.Background(),
				`SELECT count(*) FROM asset_symbol WHERE symbol = 'META'`).Scan(&metaAliases))
			if tc.wantLookup {
				require.Equal(t, 1, metaBars, "META's bars are stored")
				require.Equal(t, 1, metaAliases, "META is recorded as FB's later ticker")
				return
			}
			require.Zero(t, metaBars, "no later-ticker bars are stored")
			require.Zero(t, metaAssets, "no later-ticker asset is stored")
			require.Zero(t, metaAliases, "no later-ticker alias is stored")
			var fbBars int
			require.NoError(t, pool.QueryRow(context.Background(),
				`SELECT count(*) FROM ohlc WHERE provider = $1 AND symbol = 'FB'`, tc.provider).Scan(&fbBars))
			if tc.wantFBBars {
				require.Equal(t, 1, fbBars, "the retry's bars are stored under FB")
				return
			}
			require.Zero(t, fbBars, "a failed symbol stores no bars")
		})
	}
}

// fbBars is one complete bar for FB, as a retry that has them returns it.
func fbBars() []*pb.OHLC {
	return []*pb.OHLC{
		{Timestamp: timestamppb.New(time.Date(2019, 6, 4, 0, 0, 0, 0, time.UTC)), Open: 20, High: 21, Low: 19, Close: 20.5, Volume: 500},
	}
}

// lookupCalls is the number of SearchAsset and later-ticker GetOHLC calls a
// case makes: one each when the lookup runs.
func lookupCalls(lookup bool) int {
	if lookup {
		return 1
	}
	return 0
}

// storeRenamedFB stores the instrument with FB as its only alias, so META
// can only come from the search.
func storeRenamedFB(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	instruments := repository.Instruments{Conn: pool}
	_, err := instruments.Store(context.Background(), renamedISIN, "Facebook Inc")
	require.NoError(t, err)
	_, err = pool.Exec(context.Background(), `
		INSERT INTO asset_symbol (isin, symbol, from_date, to_date)
		VALUES ($1, 'FB', '2019-01-01'::date, NULL)`, renamedISIN)
	require.NoError(t, err)
}
