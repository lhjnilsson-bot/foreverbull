package command_test

import (
	"context"
	"errors"
	"time"

	"github.com/lhjnilsson/foreverbull/internal/stream"
	"github.com/lhjnilsson/foreverbull/pkg/finance/internal/stream/command"
	"github.com/lhjnilsson/foreverbull/pkg/finance/internal/stream/dependency"
	fs "github.com/lhjnilsson/foreverbull/pkg/finance/stream"
	pb "github.com/lhjnilsson/foreverbull/pkg/pb/finance"
	"github.com/stretchr/testify/mock"
)

// Ingest passes its ctx to the provider fetch: when the ctx ends, the fetch
// in flight stops and ingest returns, well before any provider timeout.
func (test *IngestCommandTest) TestIngestCancelStopsTheFetch() {
	test.Require().NoError(test.assets.Store(context.Background(), "AAPL", "Apple Inc."))
	entered := make(chan struct{})
	stopped := make(chan error, 1)
	test.marketdata.On("GetOHLC", mock.Anything, "AAPL", mock.Anything, mock.Anything).
		Return(func(ctx context.Context, _ string, _ time.Time, _ *time.Time) ([]*pb.OHLC, error) {
			close(entered)
			select {
			case <-ctx.Done():
				stopped <- ctx.Err()
			case <-time.After(10 * time.Second):
				stopped <- errors.New("the fetch ctx never ended")
			}
			return nil, ctx.Err()
		}).Once()

	message := new(stream.MockMessage)
	message.On("MustGet", stream.DBDep).Return(test.db)
	message.On("MustGet", dependency.MarketDataDep).Return(dividendFree{test.marketdata})
	message.On("ParsePayload", &fs.IngestCommand{}).Return(nil).Run(func(args mock.Arguments) {
		payload := args.Get(0).(*fs.IngestCommand)
		payload.Provider = "yahoo"
		payload.Symbols = []string{"AAPL"}
		payload.Start = "2024-06-03"
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- command.Ingest(ctx, message) }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		test.FailNow("the fetch never started")
	}
	cancel()
	select {
	case err := <-stopped:
		test.Require().ErrorIs(err, context.Canceled)
	case <-time.After(15 * time.Second):
		test.FailNow("the fetch never returned")
	}
	select {
	case err := <-done:
		test.Require().ErrorIs(err, context.Canceled)
	case <-time.After(10 * time.Second):
		test.FailNow("ingest did not return after its ctx ended")
	}
}
