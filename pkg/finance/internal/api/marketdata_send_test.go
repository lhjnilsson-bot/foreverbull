package api

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/lhjnilsson/foreverbull/pkg/finance/supplier"
	internal_pb "github.com/lhjnilsson/foreverbull/pkg/pb"
	servicePb "github.com/lhjnilsson/foreverbull/pkg/pb/finance/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// countingStream fails the test when two Sends overlap. Its unguarded append
// also trips -race if Send is ever called concurrently.
type countingStream struct {
	grpc.ServerStream

	inFlight atomic.Int32
	overlaps atomic.Int32
	sent     []*servicePb.DownloadHistoricalDataResponse
}

func (s *countingStream) Send(rsp *servicePb.DownloadHistoricalDataResponse) error {
	if s.inFlight.Add(1) > 1 {
		s.overlaps.Add(1)
	}
	defer s.inFlight.Add(-1)
	s.sent = append(s.sent, rsp)
	return nil
}

func TestDownloadSenderSerialisesSend(t *testing.T) {
	stream := &countingStream{}
	sender := newDownloadSender(stream, func() {})

	const workers, perWorker = downloadWorkers, 50
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perWorker {
				sender.send(&servicePb.DownloadHistoricalDataResponse{Symbol: "X"})
			}
		}()
	}
	wg.Wait()

	assert.Zero(t, stream.overlaps.Load(), "Send must never run concurrently")
	assert.Len(t, stream.sent, workers*perWorker)
}

var errClientGone = errors.New("transport is closing")

// failingStream fails every Send, like a stream whose client has gone while
// the server context is still live.
type failingStream struct {
	grpc.ServerStream

	sends atomic.Int32
}

func (s *failingStream) Context() context.Context { return context.Background() }

func (s *failingStream) Send(*servicePb.DownloadHistoricalDataResponse) error {
	s.sends.Add(1)
	return errClientGone
}

// Once a Send fails no worker starts another symbol: at most the symbols
// already in flight (one per worker) reach the provider, and nothing is sent
// after the failure.
func TestDownloadHistoricalDataStopsAfterSendFails(t *testing.T) {
	const symbolCount = 200
	md := supplier.NewMockMarketdata(t)
	var lookups atomic.Int32
	md.On("GetAsset", mock.Anything, mock.Anything).Run(func(mock.Arguments) { lookups.Add(1) }).
		Return(nil, errors.New("not found"))

	symbols := make([]string, 0, symbolCount)
	for i := range symbolCount {
		symbols = append(symbols, fmt.Sprintf("SYM%03d", i))
	}
	stream := &failingStream{}
	server := NewMarketdataServer(nil, dividendFree{md})
	err := server.DownloadHistoricalData(&servicePb.DownloadHistoricalDataRequest{
		Symbols:   symbols,
		StartDate: &internal_pb.Date{Year: 2026, Month: 5, Day: 1},
		Provider:  "yahoo",
	}, stream)

	require.ErrorIs(t, err, errClientGone)
	assert.Equal(t, int32(1), stream.sends.Load(), "nothing is sent after the failed Send")
	assert.LessOrEqual(t, lookups.Load(), int32(downloadWorkers), "no symbol starts after the failed Send")
}

// A sender with no stop (built without newDownloadSender) must not panic on
// its first failed Send; it records the error and sends nothing more.
func TestDownloadSenderWithoutStopDoesNotPanic(t *testing.T) {
	stream := &failingStream{}
	sender := &downloadSender{stream: stream}

	require.NotPanics(t, func() {
		sender.send(&servicePb.DownloadHistoricalDataResponse{Symbol: "X"})
		sender.send(&servicePb.DownloadHistoricalDataResponse{Symbol: "Y"})
	})
	require.ErrorIs(t, sender.sendErr(), errClientGone)
	assert.Equal(t, int32(1), stream.sends.Load())
}

// newDownloadSender wires stop: the first failed Send calls it once.
func TestNewDownloadSenderStopsOnFailedSend(t *testing.T) {
	var stops atomic.Int32
	stream := &failingStream{}
	sender := newDownloadSender(stream, func() { stops.Add(1) })

	sender.send(&servicePb.DownloadHistoricalDataResponse{Symbol: "X"})
	sender.send(&servicePb.DownloadHistoricalDataResponse{Symbol: "Y"})

	require.ErrorIs(t, sender.sendErr(), errClientGone)
	assert.Equal(t, int32(1), stops.Load())
}
