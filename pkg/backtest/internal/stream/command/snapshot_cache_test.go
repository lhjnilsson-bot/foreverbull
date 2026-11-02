package command

import (
	"os"
	"testing"
	"time"

	"github.com/lhjnilsson/foreverbull/pkg/backtest/internal/stream/dependency"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// A cache folder that can't be read reaches the user as its sentence, not
// behind the "error listing snapshot dates" wrap.
func TestLoadETFSnapshotsCacheErrorIsTheSentence(t *testing.T) {
	scraper := dependency.NewMockETFScraper(t)
	scraper.On("ListSnapshotDates", mock.Anything, mock.Anything, mock.Anything).
		Return(nil, &dependency.SnapshotCacheError{Dir: "/cache/iShares_S_P_100", Err: os.ErrPermission})
	_, err := loadETFSnapshots(scraper, "iShares S&P 100",
		time.Date(2022, 1, 3, 0, 0, 0, 0, time.UTC), time.Date(2022, 12, 30, 0, 0, 0, 0, time.UTC))
	require.EqualError(t, err, "Couldn't read the saved ETF holdings in /cache/iShares_S_P_100: foreverbull isn't allowed "+
		"to read it. Please report it at https://github.com/lhjnilsson/foreverbull/issues with this message.")
}
