package dependency

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type failingStoreCache struct {
	*fsSnapshotCache
	err error
}

func (c failingStoreCache) store(string, time.Time, []byte) error { return c.err }

// A lookback day read from the cache is copied under the requested as-of.
// When that copy fails, the snapshot fails with the cause, the same as when
// a fetched body can't be cached.
func TestGetSnapshotFailsWhenLookbackCopyFails(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(holdingsJSON(t, nil))
	}))
	t.Cleanup(srv.Close)

	cache := &fsSnapshotCache{dir: t.TempDir()}
	published := holdingsJSON(t, [][]interface{}{
		{"AAPL", "Apple Inc.", "Technology", "Equity", "", "", "", "", "US0378331005"},
	})
	require.NoError(t, cache.store("iShares S&P 100", testDate(t, "2022-12-30"), published))
	broken := errors.New("disk full")
	scraper := &ISharesETFLoopup{
		products: map[string]string{"iShares S&P 100": srv.URL},
		cache:    failingStoreCache{fsSnapshotCache: cache, err: broken},
		client:   srv.Client(),
	}

	_, err := scraper.GetSnapshot("iShares S&P 100", testDate(t, "2022-12-31"))
	require.ErrorIs(t, err, broken)
	dir := filepath.Join(cache.dir, "iShares_S_P_100")
	require.EqualError(t, err, "Couldn't save the downloaded ETF holdings in "+dir+": the file couldn't be written. "+
		"Please report it at https://github.com/lhjnilsson/foreverbull/issues with this message.")
}

// The cached as-of dates join the sampled month-ends. A cache directory
// that can't be read fails the list instead of dropping those dates.
func TestListSnapshotDatesFailsWhenCacheUnreadable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// A file where the ETF's directory should be makes ReadDir fail.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "iShares_S_P_100"), nil, 0o600))
	scraper := &ISharesETFLoopup{
		products: map[string]string{"iShares S&P 100": "http://127.0.0.1:1"},
		cache:    &fsSnapshotCache{dir: dir},
	}

	_, err := scraper.ListSnapshotDates("iShares S&P 100", testDate(t, "2022-01-01"), testDate(t, "2022-12-31"))
	var cacheErr *SnapshotCacheError
	require.ErrorAs(t, err, &cacheErr)
	require.EqualError(t, err, "Couldn't read the saved ETF holdings in "+filepath.Join(dir, "iShares_S_P_100")+
		": the folder couldn't be read. Please report it at https://github.com/lhjnilsson/foreverbull/issues with this message.")

	denied := &SnapshotCacheError{Dir: "/cache/x", Err: os.ErrPermission}
	require.EqualError(t, denied, "Couldn't read the saved ETF holdings in /cache/x: foreverbull isn't allowed to read it. "+
		"Please report it at https://github.com/lhjnilsson/foreverbull/issues with this message.")

	empty := &ISharesETFLoopup{
		products: map[string]string{"iShares S&P 100": "http://127.0.0.1:1"},
		cache:    &fsSnapshotCache{dir: t.TempDir()},
	}
	dates, err := empty.ListSnapshotDates("iShares S&P 100", testDate(t, "2022-01-01"), testDate(t, "2022-01-31"))
	require.NoError(t, err)
	require.Len(t, dates, 1)
}
