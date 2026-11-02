package marketdata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alpacahq/alpaca-trade-api-go/v3/marketdata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// alpacaSessionServer answers like Alpaca during a session: daily bars
// stamped at midnight New York for yesterday and today, both priced, since
// Alpaca's bar for today has prices while the market is open. Like Alpaca
// it sends a bar only when its stamp is at or before the inclusive end, and
// it records every end asked for.
func alpacaSessionServer(t *testing.T, yesterday, today time.Time) (*AlpacaBars, *[]time.Time) {
	t.Helper()
	return alpacaRangeServer(t, yesterday, today)
}

// alpacaRangeServer answers like Alpaca's daily bars: one priced bar per
// stamp given, each at midnight New York, sent when the stamp is at or
// after start and at or before the inclusive end. It records every end
// asked for.
func alpacaRangeServer(t *testing.T, stamps ...time.Time) (*AlpacaBars, *[]time.Time) {
	t.Helper()
	var mu sync.Mutex
	ends := make([]time.Time, 0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, err := time.Parse(time.RFC3339Nano, r.URL.Query().Get("start"))
		if !assert.NoError(t, err) {
			http.Error(w, "bad start", http.StatusBadRequest)
			return
		}
		end, err := time.Parse(time.RFC3339Nano, r.URL.Query().Get("end"))
		if !assert.NoError(t, err) {
			http.Error(w, "bad end", http.StatusBadRequest)
			return
		}
		mu.Lock()
		ends = append(ends, end)
		mu.Unlock()
		bars := make([]string, 0, len(stamps))
		for _, stamp := range stamps {
			if stamp.Before(start) || stamp.After(end) {
				continue
			}
			bars = append(bars, `{"t":"`+stamp.UTC().Format(time.RFC3339)+
				`","o":10,"h":10.5,"l":9.5,"c":10.2,"v":100,"n":1,"vw":10.1}`)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"bars":{"XYZ":[` + strings.Join(bars, ",") + `]},"next_page_token":null}`))
	}))
	t.Cleanup(srv.Close)
	return &AlpacaBars{opts: marketdata.ClientOpts{
		APIKey: "key", APISecret: "secret", BaseURL: srv.URL,
	}}, &ends
}

func alpacaDays(t *testing.T, bars *AlpacaBars, start time.Time, end *time.Time) []string {
	t.Helper()
	ohlcs, err := bars.GetOHLC(context.Background(), "XYZ", start, end)
	require.NoError(t, err)
	days := make([]string, 0, len(ohlcs))
	for _, ohlc := range ohlcs {
		days = append(days, ohlc.GetTimestamp().AsTime().In(xnysZone).Format(time.DateOnly))
	}
	return days
}

// Until 16:30 New York, when today's session counts as completed, a fetch
// that ends today or later stops just before midnight New York, so today's
// priced but unfinished bar is neither asked for nor returned to be
// stored. That holds during the session and just after the 16:00 close.
func TestAlpacaGetOHLCLeavesTodaysUnfinishedSessionOut(t *testing.T) {
	t.Parallel()
	for _, at := range []string{"2026-10-30 11:00", "2026-10-30 16:05"} {
		yesterday := newYork(t, "2026-10-29 00:00")
		today := newYork(t, "2026-10-30 00:00")
		bars, ends := alpacaSessionServer(t, yesterday, today)
		now := newYork(t, at)
		bars.now = func() time.Time { return now }
		start := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
		tomorrow := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)

		require.Equal(t, []string{"2026-10-29"}, alpacaDays(t, bars, start, &tomorrow), at)
		require.Equal(t, []string{"2026-10-29"}, alpacaDays(t, bars, start, nil), at)
		require.Len(t, *ends, 2)
		for _, end := range *ends {
			require.True(t, today.Add(-time.Nanosecond).Equal(end), "at %s, end %s, want just before today", at, end)
		}
	}
}

// Every Alpaca bars request ends at least alpacaRecentDataLimit before now,
// Alpaca's free-plan limit on recent SIP data, with an end date of today
// and with no end. During the session the end is already just before
// midnight New York. From 16:30 it is now minus 15 minutes, still after
// the 16:00 close, so today's bar is returned. Just after midnight it is
// 15 minutes before now, the evening before.
func TestAlpacaGetOHLCEndsAtLeast15MinutesBeforeNow(t *testing.T) {
	t.Parallel()
	yesterday := newYork(t, "2026-10-29 00:00")
	today := newYork(t, "2026-10-30 00:00")
	cases := []struct {
		at   string
		want time.Time
		days []string
	}{
		{"2026-10-30 11:00", today.Add(-time.Nanosecond), []string{"2026-10-29"}},
		{"2026-10-30 16:05", today.Add(-time.Nanosecond), []string{"2026-10-29"}},
		{"2026-10-30 16:35", newYork(t, "2026-10-30 16:20"), []string{"2026-10-29", "2026-10-30"}},
		{"2026-10-30 00:10", newYork(t, "2026-10-29 23:55"), []string{"2026-10-29"}},
	}
	start := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	endDate := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	for _, tc := range cases {
		for name, end := range map[string]*time.Time{"end today": &endDate, "no end": nil} {
			bars, ends := alpacaSessionServer(t, yesterday, today)
			now := newYork(t, tc.at)
			bars.now = func() time.Time { return now }

			require.Equal(t, tc.days, alpacaDays(t, bars, start, end), "%s, %s", tc.at, name)
			require.Len(t, *ends, 1)
			sent := (*ends)[0]
			require.True(t, tc.want.Equal(sent), "%s, %s: end %s, want %s", tc.at, name, sent.In(xnysZone), tc.want)
			require.False(t, sent.After(now.Add(-alpacaRecentDataLimit)), "%s, %s", tc.at, name)
			if tc.at == "2026-10-30 16:35" {
				require.True(t, sent.After(newYork(t, "2026-10-30 16:00")), "the end is after today's close")
			}
		}
	}
}

// From 16:30 New York, today's session is completed and is fetched.
func TestAlpacaGetOHLCIncludesTodaysSessionAfterTheClose(t *testing.T) {
	t.Parallel()
	yesterday := newYork(t, "2026-10-29 00:00")
	today := newYork(t, "2026-10-30 00:00")
	bars, _ := alpacaSessionServer(t, yesterday, today)
	now := newYork(t, "2026-10-30 16:35")
	bars.now = func() time.Time { return now }
	start := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	tomorrow := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)

	require.Equal(t, []string{"2026-10-29", "2026-10-30"}, alpacaDays(t, bars, start, nil))
	require.Equal(t, []string{"2026-10-29", "2026-10-30"}, alpacaDays(t, bars, start, &tomorrow))
}

// A fetch for today alone, before the close, asks Alpaca for nothing.
func TestAlpacaGetOHLCForTodayOnlyBeforeTheCloseIsEmpty(t *testing.T) {
	t.Parallel()
	bars, ends := alpacaSessionServer(t, newYork(t, "2026-10-29 00:00"), newYork(t, "2026-10-30 00:00"))
	now := newYork(t, "2026-10-30 11:00")
	bars.now = func() time.Time { return now }

	require.Empty(t, alpacaDays(t, bars, newYork(t, "2026-10-30 00:00"), nil))
	require.Empty(t, *ends)
}

// A fetch that ends before today ends just before the midnight New York
// after its end date, and the end date's bar, stamped at midnight New York
// that day, is returned.
func TestAlpacaGetOHLCKeepsAPastEnd(t *testing.T) {
	t.Parallel()
	bars, ends := alpacaSessionServer(t, newYork(t, "2026-10-19 00:00"), newYork(t, "2026-10-20 00:00"))
	now := newYork(t, "2026-10-30 11:00")
	bars.now = func() time.Time { return now }
	end := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)

	require.Equal(t, []string{"2026-10-19", "2026-10-20"},
		alpacaDays(t, bars, time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC), &end))
	require.Len(t, *ends, 1)
	want := newYork(t, "2026-10-21 00:00").Add(-time.Nanosecond)
	require.True(t, want.Equal((*ends)[0]), "end %s, want %s", (*ends)[0], want)
}

// Alpaca could send today's bar whatever the end asked for. Before 16:30
// New York it is unfinished and dropped.
func TestAlpacaGetOHLCDropsABarAfterTheLastCompletedSession(t *testing.T) {
	t.Parallel()
	yesterday := newYork(t, "2026-10-29 00:00")
	today := newYork(t, "2026-10-30 00:00")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"bars":{"XYZ":[{"t":"` + yesterday.UTC().Format(time.RFC3339) +
			`","o":10,"h":10.5,"l":9.5,"c":10.2,"v":100,"n":1,"vw":10.1},{"t":"` +
			today.UTC().Format(time.RFC3339) +
			`","o":10.3,"h":10.6,"l":10.1,"c":10.4,"v":50,"n":1,"vw":10.3}]},"next_page_token":null}`))
	}))
	t.Cleanup(srv.Close)
	bars := &AlpacaBars{opts: marketdata.ClientOpts{
		APIKey: "key", APISecret: "secret", BaseURL: srv.URL,
	}}
	start := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	tomorrow := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)

	now := newYork(t, "2026-10-30 11:00")
	bars.now = func() time.Time { return now }
	require.Equal(t, []string{"2026-10-29"}, alpacaDays(t, bars, start, &tomorrow))
	require.Equal(t, []string{"2026-10-29"}, alpacaDays(t, bars, start, nil))

	// From 16:30 New York today's session is completed, and its bar is kept.
	now = newYork(t, "2026-10-30 16:35")
	require.Equal(t, []string{"2026-10-29", "2026-10-30"}, alpacaDays(t, bars, start, nil))
}

// An end date of today, asked at 16:35 New York, is capped at 16:20, after
// the 16:00 close, so today's bar is returned.
func TestAlpacaGetOHLCEndDateTodayAfterTheCloseKeepsTodaysBar(t *testing.T) {
	t.Parallel()
	bars, ends := alpacaSessionServer(t, newYork(t, "2026-10-29 00:00"), newYork(t, "2026-10-30 00:00"))
	now := newYork(t, "2026-10-30 16:35")
	bars.now = func() time.Time { return now }
	start := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)

	require.Equal(t, []string{"2026-10-29", "2026-10-30"}, alpacaDays(t, bars, start, &end))
	require.Len(t, *ends, 1)
	want := newYork(t, "2026-10-30 16:20")
	require.True(t, want.Equal((*ends)[0]), "end %s, want %s", (*ends)[0].In(xnysZone), want)
}

// When the 15-minute cap leaves nothing after start, NothingToFetch says so
// and GetOHLC sends Alpaca no request.
func TestAlpacaNothingToFetchWhenTheCapEmptiesTheRange(t *testing.T) {
	t.Parallel()
	bars, ends := alpacaSessionServer(t, newYork(t, "2026-10-29 00:00"), newYork(t, "2026-10-30 00:00"))
	// 2026-10-30 00:00 UTC is 20:00 New York on 10-29; the cap at 20:10
	// ends at 19:55, before start, though the 10-29 session has completed.
	start := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	now := newYork(t, "2026-10-29 20:10")
	bars.now = func() time.Time { return now }

	require.True(t, bars.NothingToFetch(start, nil))
	require.Empty(t, alpacaDays(t, bars, start, nil))
	require.Empty(t, *ends, "no request is sent")

	// Ten minutes later the capped end, 20:05, is after start.
	later := newYork(t, "2026-10-29 20:20")
	bars.now = func() time.Time { return later }
	require.False(t, bars.NothingToFetch(start, nil))
}
