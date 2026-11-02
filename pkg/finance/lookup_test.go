package finance

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"testing"

	"github.com/lhjnilsson/foreverbull/pkg/finance/internal/suppliers/marketdata"
	"github.com/stretchr/testify/require"
)

func TestAsLookupPassesACancelledContextThrough(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cause := &url.Error{Op: "Get", URL: "https://provider.example/v2/assets/XYZ", Err: context.Canceled}
	err := asLookup(ctx, "alpaca", "XYZ", fmt.Errorf("asset status: %w", cause))
	require.ErrorIs(t, err, context.Canceled)
	var lookup *LookupError
	require.False(t, errors.As(err, &lookup), "a cancelled create is not a failed lookup")
}

func TestAsLookupKeepsANetworkFailureWhileTheContextIsLive(t *testing.T) {
	t.Parallel()

	cause := &url.Error{Op: "Get", URL: "https://provider.example/v2/assets/XYZ", Err: errors.New("connection refused")}
	err := asLookup(context.Background(), "alpaca", "XYZ", cause)
	var lookup *LookupError
	require.ErrorAs(t, err, &lookup)
	require.Equal(t, LookupNetwork, lookup.Kind)
}

// An Alpaca bars status is read from its type, not from its text: a
// message without "HTTP <code>" still gives the code.
func TestAsLookupReadsAnAlpacaBarsStatusByType(t *testing.T) {
	t.Parallel()

	cause := &marketdata.AlpacaBarsError{Symbol: "XYZ", HTTPCode: 503, HTTPStatus: "Service Unavailable", Tries: 4}
	err := asLookup(context.Background(), "alpaca", "XYZ", fmt.Errorf("benchmark bars: %w", cause))
	var lookup *LookupError
	require.ErrorAs(t, err, &lookup)
	require.Equal(t, LookupHTTP, lookup.Kind)
	require.Equal(t, 503, lookup.Status)
}
