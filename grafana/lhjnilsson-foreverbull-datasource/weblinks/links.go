// Package weblinks builds the Grafana "Open in foreverbull" URLs and writes
// them into the provisioned dashboards before Grafana starts.
//
// Grafana does not expand environment variables inside dashboard JSON, so the
// public web origin is substituted here. An empty origin removes the links.
// A non-empty origin that is not an http(s) address, or that contains $,
// is rejected so Grafana does not interpolate it.
package weblinks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const (
	// WebOriginToken is the placeholder in dashboard link URLs.
	WebOriginToken = "__FOREVERBULL_WEB_URL__"

	// WebOriginEnv is the Grafana process environment variable.
	// Helm grafana.webUrl sets it. The default for local Kind is http://localhost:8080.
	WebOriginEnv = "FOREVERBULL_WEB_URL"

	backtestsPath = "/backtests"

	// maxDashboards bounds the provisioning directory. The image ships three.
	maxDashboards = 32

	// maxDashboardBytes caps one dashboard file.
	maxDashboardBytes = 4 << 20
)

// BacktestURL is the foreverbull page for one backtest: {origin}/backtests/{name}.
// The name is a single path segment. An unusable origin or name returns false.
func BacktestURL(webOrigin, name string) (string, bool) {
	origin, ok := publicOrigin(webOrigin)
	if !ok {
		return "", false
	}
	segment, ok := pathSegment(name)
	if !ok {
		return "", false
	}
	return origin + backtestsPath + "/" + segment, true
}

// BacktestsURL is the foreverbull backtest list: {origin}/backtests.
func BacktestsURL(webOrigin string) (string, bool) {
	origin, ok := publicOrigin(webOrigin)
	if !ok {
		return "", false
	}
	return origin + backtestsPath, true
}

// Rewrite substitutes WebOriginToken in dashboard JSON.
// A usable origin is written without userinfo, query, or fragment.
// An empty origin removes every link whose URL still contains the token.
// A non-empty unusable origin is an error. Dashboards without the token
// are returned unchanged.
func Rewrite(raw []byte, webOrigin string) ([]byte, error) {
	if !bytes.Contains(raw, []byte(WebOriginToken)) {
		return raw, nil
	}
	if strings.TrimSpace(webOrigin) == "" {
		return dropTokenLinks(raw)
	}
	origin, ok := publicOrigin(webOrigin)
	if !ok {
		return nil, fmt.Errorf("web origin is not an http(s) address")
	}
	if strings.Contains(origin, WebOriginToken) {
		return nil, fmt.Errorf("web origin contains %s", WebOriginToken)
	}
	replaced := bytes.ReplaceAll(raw, []byte(WebOriginToken), []byte(origin))
	if !json.Valid(replaced) {
		return nil, fmt.Errorf("dashboard json is invalid after substituting the web origin")
	}
	if bytes.Contains(replaced, []byte(WebOriginToken)) {
		return nil, fmt.Errorf("dashboard still contains %s", WebOriginToken)
	}
	return replaced, nil
}

// Install reads the source dashboards and writes them to dst.
// FOREVERBULL_WEB_URL is applied to every JSON file. Other files are skipped.
func Install(src, dst, webOrigin string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("read dashboards: %w", err)
	}
	jsonFiles := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		jsonFiles = append(jsonFiles, name)
		if len(jsonFiles) > maxDashboards {
			return fmt.Errorf("more than %d dashboard files in %s", maxDashboards, src)
		}
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("create dashboard dir: %w", err)
	}
	for _, name := range jsonFiles {
		if err := installOne(src, dst, name, webOrigin); err != nil {
			return err
		}
	}
	return nil
}

func installOne(src, dst, name, webOrigin string) error {
	if name != filepath.Base(name) || strings.Contains(name, "..") {
		return fmt.Errorf("dashboard name %q", name)
	}
	inPath := filepath.Join(src, name)
	raw, err := os.ReadFile(inPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}
	if len(raw) > maxDashboardBytes {
		return fmt.Errorf("%s exceeds %d bytes", name, maxDashboardBytes)
	}
	rewritten, err := Rewrite(raw, webOrigin)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	// Overwrite the file in place. The dashboard directory in the image can
	// stay root-owned; uid 472 only needs to be able to write the file it owns.
	out := filepath.Join(dst, name)
	if err := os.WriteFile(out, rewritten, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	return nil
}

func dropTokenLinks(raw []byte) ([]byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("dashboard json: %w", err)
	}
	linksRaw, ok := doc["links"]
	if !ok {
		return raw, nil
	}
	var links []json.RawMessage
	if err := json.Unmarshal(linksRaw, &links); err != nil {
		return nil, fmt.Errorf("dashboard links: %w", err)
	}
	kept := make([]json.RawMessage, 0, len(links))
	for _, linkRaw := range links {
		var link struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(linkRaw, &link); err != nil {
			return nil, fmt.Errorf("dashboard link: %w", err)
		}
		if strings.Contains(link.URL, WebOriginToken) {
			continue
		}
		kept = append(kept, linkRaw)
	}
	encoded, err := json.Marshal(kept)
	if err != nil {
		return nil, fmt.Errorf("encode links: %w", err)
	}
	doc["links"] = encoded
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("encode dashboard: %w", err)
	}
	if bytes.Contains(out, []byte(WebOriginToken)) {
		return nil, fmt.Errorf("dashboard still contains %s", WebOriginToken)
	}
	return out, nil
}

func publicOrigin(raw string) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || strings.ContainsAny(trimmed, " \t\r\n") || strings.Contains(trimmed, "$") {
		return "", false
	}
	u, err := url.Parse(trimmed)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return "", false
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	u.RawPath = ""
	cleaned := path.Clean("/" + strings.Trim(u.Path, "/"))
	if cleaned == "/" || cleaned == "." {
		u.Path = ""
	} else {
		u.Path = cleaned
	}
	origin := u.String()
	if strings.Contains(origin, "@") || strings.ContainsAny(origin, " \t\r\n\"\\") {
		return "", false
	}
	return origin, true
}

func pathSegment(name string) (string, bool) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" || trimmed == "." || trimmed == ".." {
		return "", false
	}
	if strings.ContainsAny(trimmed, "\r\n") {
		return "", false
	}
	escaped := url.PathEscape(trimmed)
	if escaped == "" || strings.Contains(escaped, "/") {
		return "", false
	}
	return escaped, true
}
