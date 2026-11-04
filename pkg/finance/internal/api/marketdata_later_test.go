package api

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/internal/test_helper"
	"github.com/lhjnilsson/foreverbull/pkg/finance/internal/repository"
	"github.com/lhjnilsson/foreverbull/pkg/finance/internal/suppliers/marketdata"
	"github.com/lhjnilsson/foreverbull/pkg/finance/supplier"
	pb "github.com/lhjnilsson/foreverbull/pkg/pb/finance"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// A download cancelled while a later ticker is fetched returns the context
// error, not "no price data", even when GetOHLC answered with no bars.
func TestFetchBarsViaLaterTickerReturnsContextErrorWhenCancelled(t *testing.T) {
	test_helper.SetupEnvironment(t, &test_helper.Containers{Postgres: true})
	pool, err := pgxpool.New(context.Background(), environment.GetPostgresURL())
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, repository.Recreate(context.Background(), pool))

	const isin = "US30303M1027"
	instruments := repository.Instruments{Conn: pool}
	_, err = instruments.Store(context.Background(), isin, "Facebook Inc")
	require.NoError(t, err)
	_, err = pool.Exec(context.Background(), `
		INSERT INTO asset_symbol (isin, symbol, from_date, to_date)
		VALUES ($1, 'FB', '2019-01-01'::date, NULL)`, isin)
	require.NoError(t, err)

	md := supplier.NewMockMarketdata(t)
	md.On("SearchAsset", mock.Anything, isin).Return([]*pb.Asset{{Symbol: "META", ISIN: isin}}, nil)
	fs := NewMarketdataServer(pool, dividendFree{md})

	start := time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2020, 12, 31, 0, 0, 0, 0, time.UTC)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	md.On("GetOHLC", mock.Anything, "META", mock.Anything, mock.Anything).Run(func(mock.Arguments) { cancel() }).
		Return(nil, fmt.Errorf("fail to get OHLC data for symbol META: %w", marketdata.ErrNoBars)).Once()

	fetched, err := fs.fetchBarsViaLaterTicker(ctx, "FB", start, &end, md, repository.Asset{Conn: pool})
	require.Nil(t, fetched)
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, errNoPriceData)
	md.AssertNotCalled(t, "GetAsset", mock.Anything, "META")
}
