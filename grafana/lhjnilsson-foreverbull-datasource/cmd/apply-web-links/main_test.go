package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRunEmptyOriginHidesLinks(t *testing.T) {
	dir := dashboardDir(t)
	var out strings.Builder
	code := run("", dir, dir, &out)
	require.Equal(t, 0, code)
	logged := out.String()
	require.Contains(t, logged, "level=INFO")
	require.Contains(t, logged, "FOREVERBULL_WEB_URL is empty, so the dashboards won't link to foreverbull.")
	body := readDash(t, dir, "executions.json")
	require.NotContains(t, body, "__FOREVERBULL_WEB_URL__")
	var doc struct {
		Links []struct {
			Title string `json:"title"`
		} `json:"links"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &doc))
	for _, link := range doc.Links {
		require.NotEqual(t, "Open in foreverbull", link.Title)
	}
	require.Contains(t, body, "Backtest overview")
}

func TestRunInvalidOriginExitsBeforeWriting(t *testing.T) {
	dir := dashboardDir(t)
	before := readDash(t, dir, "executions.json")
	var out strings.Builder
	code := run("not a url", dir, dir, &out)
	require.Equal(t, 1, code)
	require.Equal(t, "FOREVERBULL_WEB_URL isn't a web address foreverbull can link to. Set grafana.webUrl to the foreverbull page's address, such as http://localhost:8080, or leave it empty to hide the links. Grafana didn't start.\n", out.String())
	require.Equal(t, before, readDash(t, dir, "executions.json"))
}

func TestRunDollarOriginIsInvalid(t *testing.T) {
	dir := dashboardDir(t)
	var out strings.Builder
	code := run("http://localhost:8080/$home", dir, dir, &out)
	require.Equal(t, 1, code)
	require.Contains(t, out.String(), "FOREVERBULL_WEB_URL isn't a web address foreverbull can link to.")
	require.NotContains(t, out.String(), "$home")
	require.Contains(t, readDash(t, dir, "executions.json"), "__FOREVERBULL_WEB_URL__")
}

func dashboardDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	raw, err := os.ReadFile(filepath.Join("..", "..", "provisioning", "dashboards", "executions.json"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "executions.json"), raw, 0o644))
	return dir
}

func readDash(t *testing.T, dir, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, name))
	require.NoError(t, err)
	return string(raw)
}
