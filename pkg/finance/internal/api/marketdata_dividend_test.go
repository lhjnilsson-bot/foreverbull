package api

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/internal/test_helper"
	financeapi "github.com/lhjnilsson/foreverbull/pkg/finance/api"
	"github.com/lhjnilsson/foreverbull/pkg/finance/internal/repository"
	"github.com/lhjnilsson/foreverbull/pkg/finance/internal/suppliers/marketdata"
	"github.com/lhjnilsson/foreverbull/pkg/finance/supplier"
	pb "github.com/lhjnilsson/foreverbull/pkg/pb/finance"
	servicePb "github.com/lhjnilsson/foreverbull/pkg/pb/finance/service"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// failingDividends is a price source whose dividend request fails.
type failingDividends struct {
	supplier.Marketdata
	err error
}

func (f failingDividends) GetDividends(context.Context, string, time.Time, *time.Time) ([]supplier.Dividend, error) {
	return nil, f.err
}

func koDownload(t *testing.T, provider string, bars supplier.Marketdata) (*countingStream, *pgxpool.Pool) {
	t.Helper()
	return symbolDownload(t, "KO", provider, bars)
}

func symbolDownload(t *testing.T, symbol, provider string, bars supplier.Marketdata) (*countingStream, *pgxpool.Pool) {
	t.Helper()
	test_helper.SetupEnvironment(t, &test_helper.Containers{Postgres: true})
	pool, err := pgxpool.New(context.Background(), environment.GetPostgresURL())
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, repository.Recreate(context.Background(), pool))

	meta := supplier.NewMockMarketdata(t)
	meta.On("GetAsset", mock.Anything, symbol).Return(&pb.Asset{Symbol: symbol, Name: symbol}, nil)
	stream := &countingStream{}
	start := time.Date(2024, 3, 13, 0, 0, 0, 0, time.UTC)
	end := time.Date(2024, 3, 14, 0, 0, 0, 0, time.UTC)
	blocked := NewMarketdataServer(pool, meta).downloadSymbol(context.Background(), newDownloadSender(stream, func() {}),
		symbol, start, &end, provider, repository.Asset{Conn: pool}, repository.OHLC{Conn: pool}, bars)
	require.Nil(t, blocked)
	return stream, pool
}

func koBarsMock(t *testing.T) *supplier.MockMarketdata {
	t.Helper()
	return barsMock(t, "KO")
}

func barsMock(t *testing.T, symbol string) *supplier.MockMarketdata {
	t.Helper()
	bars := supplier.NewMockMarketdata(t)
	bars.On("GetOHLC", mock.Anything, symbol, mock.Anything, mock.Anything).Return([]*pb.OHLC{
		{Timestamp: timestamppb.New(time.Date(2024, 3, 13, 0, 0, 0, 0, time.UTC)), Open: 61, High: 61.5, Low: 60.5, Close: 61.12, Volume: 1},
		{Timestamp: timestamppb.New(time.Date(2024, 3, 14, 0, 0, 0, 0, time.UTC)), Open: 60.6, High: 61, Low: 60, Close: 60.50, Volume: 1},
	}, nil)
	return bars
}

func count(t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var rows int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM `+table).Scan(&rows))
	return rows
}

// A failed dividend fetch fails the symbol with a sentence naming the
// provider, the symbol and the cause in foreverbull's words, sends the cause
// as dividend_failure, and stores none of its bars.
func TestDownloadSymbolFailsWhenDividendsCantBeLoaded(t *testing.T) {
	stream, pool := koDownload(t, "yahoo", failingDividends{Marketdata: koBarsMock(t), err: &marketdata.DividendStatusError{
		Provider: "yahoo", Request: "yahoo dividends", Symbol: "KO", HTTPCode: 503, HTTPStatus: "503 Service Unavailable",
	}})

	require.Len(t, stream.sent, 1)
	require.Equal(t, servicePb.DownloadHistoricalDataResponse_STATUS_ERROR, stream.sent[0].GetStatus())
	require.Equal(t, "foreverbull couldn't download dividends for KO from yahoo: yahoo had a server error (HTTP 503). "+
		"Its prices weren't saved either. Run the download again in a few minutes, or try another provider.",
		stream.sent[0].GetError())
	require.Equal(t, &financeapi.DividendFetchError{Provider: "yahoo", Symbol: "KO", Kind: financeapi.DividendHTTPStatus, Status: 503},
		financeapi.DividendFailure(stream.sent[0]))
	require.NoError(t, financeapi.DownloadFailure(stream.sent[0]))
	require.Zero(t, count(t, pool, "ohlc"))
	require.Zero(t, count(t, pool, "dividend"))
	require.Zero(t, count(t, pool, "dividend_fetch"))
}

// Stooq's bars already include dividends: its bars are stored and no
// dividend is asked for or stored.
func TestDownloadSymbolStoresNoDividendsForStooq(t *testing.T) {
	stream, pool := koDownload(t, "stooq", koBarsMock(t))

	require.Len(t, stream.sent, 1)
	require.Equal(t, servicePb.DownloadHistoricalDataResponse_STATUS_OK, stream.sent[0].GetStatus())
	require.Equal(t, 2, count(t, pool, "ohlc"))
	require.Zero(t, count(t, pool, "dividend"))
	require.Zero(t, count(t, pool, "dividend_fetch"))
}

// A yahoo source that can't fetch dividends is refused, never run without them.
func TestDownloadSymbolRefusesAYahooSourceWithoutDividends(t *testing.T) {
	stream, pool := koDownload(t, "yahoo", koBarsMock(t))

	require.Len(t, stream.sent, 1)
	require.Equal(t, servicePb.DownloadHistoricalDataResponse_STATUS_ERROR, stream.sent[0].GetStatus())
	require.Zero(t, count(t, pool, "ohlc"))
}

// fixedDividends is a price source whose dividend request answers with the
// given rows.
type fixedDividends struct {
	supplier.Marketdata
	dividends []supplier.Dividend
}

func (f fixedDividends) GetDividends(context.Context, string, time.Time, *time.Time) ([]supplier.Dividend, error) {
	return f.dividends, nil
}

// The benchmark's dividends count like any symbol's: a failed SPY fetch
// fails SPY with the cause, and nothing of SPY is stored.
func TestDownloadSymbolFailsTheBenchmarkWhenItsDividendsCantBeLoaded(t *testing.T) {
	stream, pool := symbolDownload(t, "SPY", "yahoo", failingDividends{Marketdata: barsMock(t, "SPY"), err: &marketdata.DividendStatusError{
		Provider: "yahoo", Request: "yahoo dividends", Symbol: "SPY", HTTPCode: 429, HTTPStatus: "429 Too Many Requests",
	}})

	require.Len(t, stream.sent, 1)
	require.Equal(t, servicePb.DownloadHistoricalDataResponse_STATUS_ERROR, stream.sent[0].GetStatus())
	require.Equal(t, "foreverbull couldn't download dividends for SPY from yahoo: yahoo limited how many requests "+
		"foreverbull can make. Its prices weren't saved either. Wait a few minutes, then run the download again, "+
		"or try another provider.", stream.sent[0].GetError())
	require.Zero(t, count(t, pool, "ohlc"))
	require.Zero(t, count(t, pool, "dividend"))
	require.Zero(t, count(t, pool, "dividend_fetch"))
	require.Zero(t, count(t, pool, "corporate_action"))
}

// A dividend that can't be used fails the symbol with every reason, sends
// dividend_failure, and stores nothing of the symbol.
func TestDownloadSymbolFailsOnADividendThatCantBeUsed(t *testing.T) {
	stream, pool := koDownload(t, "alpaca", fixedDividends{Marketdata: koBarsMock(t), dividends: []supplier.Dividend{{
		ExDate: time.Date(2024, 3, 14, 0, 0, 0, 0, time.UTC), Amount: 0.485, RawAmount: 0.485, SplitFactor: 1, Currency: "CAD",
	}}})

	require.Len(t, stream.sent, 1)
	require.Equal(t, servicePb.DownloadHistoricalDataResponse_STATUS_ERROR, stream.sent[0].GetStatus())
	require.Equal(t, "alpaca sent dividends for KO that can't be used: ex-date 2024-03-14: currency CAD is not USD. "+
		"Its prices weren't saved either. Try another provider, or report it at "+
		"https://github.com/lhjnilsson/foreverbull/issues with this message.", stream.sent[0].GetError())
	require.Equal(t, &financeapi.DividendFetchError{Provider: "alpaca", Symbol: "KO", Kind: financeapi.DividendInvalid},
		financeapi.DividendFailure(stream.sent[0]))
	require.Zero(t, count(t, pool, "ohlc"))
	require.Zero(t, count(t, pool, "dividend"))
	require.Zero(t, count(t, pool, "dividend_fetch"))
}

// A stock dividend is stored next to the bars, not refused at download:
// whether it fails a backtest depends on the backtest's window, which the
// bundle checks when the data is loaded.
func TestDownloadSymbolStoresAStockDividendForTheBundleToCheck(t *testing.T) {
	stream, pool := koDownload(t, "alpaca", fixedDividends{Marketdata: koBarsMock(t), dividends: []supplier.Dividend{{
		ExDate: time.Date(2024, 3, 14, 0, 0, 0, 0, time.UTC), Unsupported: "stock_dividend", SourceRef: "alpaca:d1",
	}}})

	require.Len(t, stream.sent, 1)
	require.Equal(t, servicePb.DownloadHistoricalDataResponse_STATUS_OK, stream.sent[0].GetStatus())
	require.Equal(t, 2, count(t, pool, "ohlc"))
	require.Zero(t, count(t, pool, "dividend"))
	require.Equal(t, 1, count(t, pool, "dividend_fetch"))
	require.Equal(t, 1, count(t, pool, "corporate_action"))
}
