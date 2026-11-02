package finance

import (
	"errors"
	"testing"
	"time"

	"github.com/lhjnilsson/foreverbull/pkg/finance/internal/suppliers/marketdata"
	"github.com/lhjnilsson/foreverbull/pkg/finance/supplier"
	"github.com/stretchr/testify/require"
)

func TestListingGoneNeedsAnEmptySearchAndNoLaterTicker(t *testing.T) {
	t.Parallel()

	gone, later := listingGone(nil, "XYZ", false)
	require.True(t, gone)
	require.False(t, later)

	gone, later = listingGone([]string{"XYZ"}, "XYZ", false)
	require.False(t, gone)
	require.False(t, later)

	gone, later = listingGone([]string{"META"}, "FB", false)
	require.False(t, gone)
	require.True(t, later)

	gone, later = listingGone(nil, "XYZ", true)
	require.False(t, gone)
	require.True(t, later)
}

func TestEvidenceYahooListedNamesTheDate(t *testing.T) {
	t.Parallel()
	day := time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC)
	require.Equal(t, "yahoo firstTradeDate 2024-03-15", EvidenceYahooListed(day))
}

func TestSharedYahooDoesNotCacheAConstructError(t *testing.T) {
	yahooMu.Lock()
	prevClient := yahooShared
	prevNew := newYahooClient
	yahooShared = nil
	yahooMu.Unlock()
	t.Cleanup(func() {
		yahooMu.Lock()
		yahooShared = prevClient
		newYahooClient = prevNew
		yahooMu.Unlock()
	})

	calls := 0
	newYahooClient = func() (supplier.Marketdata, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("crumb failed")
		}
		return &marketdata.YahooClient{}, nil
	}
	_, err := sharedYahoo()
	require.EqualError(t, err, "crumb failed")
	got, err := sharedYahoo()
	require.NoError(t, err)
	require.NotNil(t, got)
	_, err = sharedYahoo()
	require.NoError(t, err)
	require.Equal(t, 2, calls)
}
