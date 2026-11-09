package weblinks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBacktestURL(t *testing.T) {
	got, ok := BacktestURL("http://localhost:8080", "nasdaq100")
	require.True(t, ok)
	require.Equal(t, "http://localhost:8080/backtests/nasdaq100", got)

	got, ok = BacktestURL("https://example.com/fb/", "eu small")
	require.True(t, ok)
	require.Equal(t, "https://example.com/fb/backtests/eu%20small", got)

	got, ok = BacktestURL("http://localhost:8080", "a/b")
	require.True(t, ok)
	require.Equal(t, "http://localhost:8080/backtests/a%2Fb", got)

	got, ok = BacktestURL("http://localhost:8080", "å")
	require.True(t, ok)
	require.Equal(t, "http://localhost:8080/backtests/%C3%A5", got)

	got, ok = BacktestURL("https://ada:s3cret@example.com/app/?token=secret#x", "spy")
	require.True(t, ok)
	require.Equal(t, "https://example.com/app/backtests/spy", got)
	require.NotContains(t, got, "s3cret")
	require.NotContains(t, got, "secret")
}

func TestBacktestURLHidesUnusableInput(t *testing.T) {
	for _, origin := range []string{"", "   ", "not a url", "ftp://localhost:8080", "http://", "javascript:alert(1)"} {
		_, ok := BacktestURL(origin, "nasdaq100")
		require.False(t, ok, origin)
	}
	for _, name := range []string{"", "  ", ".", ".."} {
		_, ok := BacktestURL("http://localhost:8080", name)
		require.False(t, ok, name)
	}
}

func TestBacktestsURL(t *testing.T) {
	got, ok := BacktestsURL("http://localhost:8080")
	require.True(t, ok)
	require.Equal(t, "http://localhost:8080/backtests", got)

	got, ok = BacktestsURL("https://example.com/fb/")
	require.True(t, ok)
	require.Equal(t, "https://example.com/fb/backtests", got)

	got, ok = BacktestsURL("http://user:hunter2@localhost:8080/web?x=1")
	require.True(t, ok)
	require.Equal(t, "http://localhost:8080/web/backtests", got)
	require.NotContains(t, got, "hunter2")

	for _, origin := range []string{"", "not a url", "http://", "ftp://example.com", "http://localhost:8080/$home"} {
		_, ok := BacktestsURL(origin)
		require.False(t, ok, origin)
	}
}

func TestRewriteExecutionDashboard(t *testing.T) {
	raw := readProvisioned(t, "executions.json")
	require.Contains(t, string(raw), WebOriginToken+"${web_path}")
	require.NotContains(t, string(raw), "/backtests/")
	require.Contains(t, string(raw), `"tooltip": "Open this backtest in foreverbull to run it again or share it."`)
	require.Contains(t, string(raw), `"title": "Open in foreverbull"`)

	rewritten, err := Rewrite(raw, "https://ada:s3cret@example.com/fb/")
	require.NoError(t, err)
	text := string(rewritten)
	require.Contains(t, text, "https://example.com/fb${web_path}")
	require.NotContains(t, text, "s3cret")
	require.NotContains(t, text, WebOriginToken)
	require.Contains(t, text, "/d/fb-backtest-overview")
	require.Contains(t, text, `"uid": "ee9ep4jxtwkcgf"`)

	_, err = Rewrite(raw, "not a url")
	require.Error(t, err)
	_, err = Rewrite(raw, "http://localhost:8080/$home")
	require.Error(t, err)
	hidden, err := Rewrite(raw, "")
	require.NoError(t, err)
	require.NotContains(t, string(hidden), WebOriginToken)
	var doc struct {
		UID   string `json:"uid"`
		Links []struct {
			Title string `json:"title"`
			URL   string `json:"url"`
		} `json:"links"`
	}
	require.NoError(t, json.Unmarshal(hidden, &doc))
	require.Equal(t, "ee9ep4jxtwkcgf", doc.UID)
	titles := make([]string, 0, len(doc.Links))
	for _, link := range doc.Links {
		titles = append(titles, link.Title)
		require.NotEqual(t, "Open in foreverbull", link.Title)
	}
	require.ElementsMatch(t, []string{"Strategy compare", "Backtest overview"}, titles)
}

func TestRewriteOverviewDashboard(t *testing.T) {
	raw := readProvisioned(t, "backtest-overview.json")
	require.Contains(t, string(raw), WebOriginToken+"/backtests")
	require.Contains(t, string(raw), `"tooltip": "Create a backtest or start a run in foreverbull."`)
	require.NotContains(t, string(raw), "${backtest:percentencode}")

	rewritten, err := Rewrite(raw, "http://localhost:8080/")
	require.NoError(t, err)
	require.Contains(t, string(rewritten), "http://localhost:8080/backtests")
	require.NotContains(t, string(rewritten), "http://localhost:8080//backtests")
	require.NotContains(t, string(rewritten), WebOriginToken)
	require.Contains(t, string(rewritten), `"uid": "fb-backtest-overview"`)

	hidden, err := Rewrite(raw, "")
	require.NoError(t, err)
	var doc struct {
		Links []struct {
			Title string `json:"title"`
			URL   string `json:"url"`
		} `json:"links"`
	}
	require.NoError(t, json.Unmarshal(hidden, &doc))
	require.Len(t, doc.Links, 1)
	require.Equal(t, "Backtest execution", doc.Links[0].Title)
	require.Equal(t, "/d/ee9ep4jxtwkcgf", doc.Links[0].URL)
}

func TestInstallLeavesCompareDashboardUntouched(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	body := []byte("{\n  \"uid\": \"fb-strategy-compare\"\n}\n")
	require.NoError(t, os.WriteFile(filepath.Join(src, "strategy-compare.json"), body, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(src, "default.yaml"), []byte("apiVersion: 1\n"), 0o644))
	require.NoError(t, Install(src, dst, "http://localhost:8080"))
	got, err := os.ReadFile(filepath.Join(dst, "strategy-compare.json"))
	require.NoError(t, err)
	require.Equal(t, body, got)
	_, err = os.Stat(filepath.Join(dst, "default.yaml"))
	require.True(t, os.IsNotExist(err))
}

func readProvisioned(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "provisioning", "dashboards", name))
	require.NoError(t, err)
	return raw
}
