package dependency

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/lhjnilsson/foreverbull/internal/usercopy"
)

type snapshotCache interface {
	load(etf string, asOf time.Time) ([]byte, bool, error)
	store(etf string, asOf time.Time, body []byte) error
	list(etf string, start, end time.Time) ([]time.Time, error)
	// folder is where the cache keeps etf's files, for the user's sentence.
	folder(etf string) string
}

// SnapshotCacheError is a read or write of the ETF holdings cache that
// failed. Its text is the user's sentence with a plain cause; Err keeps the
// file system error for the log.
type SnapshotCacheError struct {
	Dir   string
	Write bool
	Err   error
}

func (e *SnapshotCacheError) Error() string {
	if e.Write {
		return usercopy.SnapshotCacheWriteFailed(e.Dir, plainCacheCause(e.Err, true))
	}
	return usercopy.SnapshotCacheReadFailed(e.Dir, plainCacheCause(e.Err, false))
}

func (e *SnapshotCacheError) Unwrap() error { return e.Err }

// plainCacheCause says in plain words why the cache folder failed.
func plainCacheCause(err error, write bool) string {
	switch {
	case errors.Is(err, fs.ErrPermission) && write:
		return "foreverbull isn't allowed to write there"
	case errors.Is(err, fs.ErrPermission):
		return "foreverbull isn't allowed to read it"
	case errors.Is(err, syscall.ENOSPC):
		return "the disk is full"
	case write:
		return "the file couldn't be written"
	default:
		return "the folder couldn't be read"
	}
}

type fsSnapshotCache struct {
	dir string
}

func (c *fsSnapshotCache) folder(etf string) string {
	if c == nil {
		return ""
	}
	return filepath.Join(c.dir, etfCacheSlug(etf))
}

func (c *fsSnapshotCache) path(etf string, asOf time.Time) string {
	return filepath.Join(c.dir, etfCacheSlug(etf), dateOnly(asOf).Format("20060102")+".json")
}

func (c *fsSnapshotCache) load(etf string, asOf time.Time) ([]byte, bool, error) {
	if c == nil || c.dir == "" {
		return nil, false, nil
	}
	body, err := os.ReadFile(c.path(etf, asOf))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return body, true, nil
}

func (c *fsSnapshotCache) store(etf string, asOf time.Time, body []byte) error {
	if c == nil || c.dir == "" {
		return nil
	}
	path := c.path(etf, asOf)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o644)
}

// list returns the cached as-of dates in [start, end]. A missing ETF
// directory is an empty cache. Any other read error is returned: the cached
// dates join the sampled ones, so a dropped list would change the universe.
// A file whose name isn't YYYYMMDD.json was not written here and is no as-of.
func (c *fsSnapshotCache) list(etf string, start, end time.Time) ([]time.Time, error) {
	if c == nil || c.dir == "" {
		return nil, nil
	}
	dir := filepath.Join(c.dir, etfCacheSlug(etf))
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	start = dateOnly(start)
	end = dateOnly(end)
	var dates []time.Time
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".json")
		if name == entry.Name() {
			continue
		}
		asOf, err := time.Parse("20060102", name)
		if err != nil {
			continue
		}
		asOf = dateOnly(asOf)
		if asOf.Before(start) || asOf.After(end) {
			continue
		}
		dates = append(dates, asOf)
	}
	return dates, nil
}

func etfCacheSlug(etf string) string {
	var b strings.Builder
	prevUnderscore := false
	for _, r := range etf {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			prevUnderscore = false
			continue
		}
		if !prevUnderscore {
			b.WriteByte('_')
			prevUnderscore = true
		}
	}
	return strings.Trim(b.String(), "_")
}

func dateOnly(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func monthEndDates(start, end time.Time) []time.Time {
	start = dateOnly(start)
	end = dateOnly(end)
	if end.Before(start) {
		return nil
	}
	var dates []time.Time
	year, month, _ := start.Date()
	for {
		last := time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC)
		if last.After(end) {
			break
		}
		if !last.Before(start) {
			dates = append(dates, last)
		}
		if month == time.December {
			year++
			month = time.January
		} else {
			month++
		}
	}
	return dates
}

func mergeDates(groups ...[]time.Time) []time.Time {
	seen := make(map[string]time.Time)
	for _, group := range groups {
		for _, d := range group {
			d = dateOnly(d)
			seen[d.Format("20060102")] = d
		}
	}
	dates := make([]time.Time, 0, len(seen))
	for _, d := range seen {
		dates = append(dates, d)
	}
	sort.Slice(dates, func(i, j int) bool { return dates[i].Before(dates[j]) })
	return dates
}

func trapBOM(fileBytes []byte) []byte {
	return bytes.Trim(fileBytes, "\xef\xbb\xbf")
}

func assetFromRow(row []interface{}) (Asset, bool) {
	if len(row) < 9 {
		return Asset{}, false
	}
	symbol, ok0 := row[0].(string)
	name, ok1 := row[1].(string)
	sector, ok2 := row[2].(string)
	assetType, ok3 := row[3].(string)
	isin, ok8 := row[8].(string)
	if !ok0 || !ok1 || !ok2 || !ok3 || !ok8 {
		return Asset{}, false
	}
	return Asset{
		Symbol:    symbol,
		Name:      name,
		Sector:    sector,
		AssetType: assetType,
		ISIN:      isin,
	}, true
}

// errHoldingsPage means the holdings URL returned the product web page.
// iShares does that for as-of dates it does not publish.
var errHoldingsPage = errors.New("iShares returned a web page instead of holdings")

func parseHoldings(body []byte) (assets []Asset, published bool, err error) {
	body = bytes.TrimSpace(trapBOM(body))
	if len(body) == 0 {
		return nil, false, errors.New("iShares returned an empty holdings response")
	}
	if body[0] == '<' {
		return nil, false, errHoldingsPage
	}
	var data Response
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, false, fmt.Errorf("iShares holdings were not valid JSON: %w", err)
	}
	if len(data.Data) == 0 {
		return nil, false, nil
	}
	assets = make([]Asset, 0, len(data.Data))
	for _, row := range data.Data {
		asset, ok := assetFromRow(row)
		if !ok || asset.AssetType != "Equity" || asset.ISIN == "" {
			continue
		}
		assets = append(assets, asset)
	}
	return assets, true, nil
}

const productDataHoldingsURL = "https://www.ishares.com/varnish-api/blk-one01-product-data/product-data/api/v2/get-product-data"

func holdingsURL(baseurl string, asOf time.Time) (string, error) {
	if resolved, ok, err := productDataHoldingsRequest(baseurl, asOf); ok || err != nil {
		return resolved, err
	}
	u, err := url.Parse(baseurl)
	if err != nil {
		return "", fmt.Errorf("error creating request: %w", err)
	}
	q := u.Query()
	q.Set("tab", "all")
	q.Set("fileType", "json")
	q.Set("asOfDate", dateOnly(asOf).Format("20060102"))
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// productDataHoldingsRequest is the holdings call the iShares product page uses.
// The old *.ajax?fileType=json URL now returns the HTML product page for every as-of.
func productDataHoldingsRequest(baseurl string, asOf time.Time) (string, bool, error) {
	u, err := url.Parse(baseurl)
	if err != nil {
		return "", false, fmt.Errorf("error creating request: %w", err)
	}
	host := strings.ToLower(u.Hostname())
	if host != "www.ishares.com" && host != "ishares.com" {
		return "", false, nil
	}
	portfolioID := ""
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i, part := range parts {
		if part == "products" && i+1 < len(parts) && isDigits(parts[i+1]) {
			portfolioID = parts[i+1]
			break
		}
	}
	if portfolioID == "" {
		return "", false, nil
	}
	q := url.Values{}
	q.Set("appSubType", "ISHARES")
	q.Set("appType", "PRODUCT_PAGE")
	q.Set("component", "holdings.all")
	q.Set("locale", "en_US")
	q.Set("portfolioId", portfolioID)
	q.Set("targetSite", "us-ishares")
	q.Set("userType", "individual")
	q.Set("excludeContent", "true")
	q.Set("includeConfig", "true")
	q.Set("asOfDate", dateOnly(asOf).Format("20060102"))
	return productDataHoldingsURL + "?" + q.Encode(), true, nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (e *ISharesETFLoopup) fetchHoldings(baseurl string, asOf time.Time) ([]Asset, []byte, bool, error) {
	rawURL, err := holdingsURL(baseurl, asOf)
	if err != nil {
		return nil, nil, false, err
	}
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, nil, false, fmt.Errorf("error creating request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-application-id", "pp-ui-csr")
	resp, err := e.httpClient().Do(req)
	if err != nil {
		return nil, nil, false, fmt.Errorf("error doing request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, false, fmt.Errorf("error status code: %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, false, fmt.Errorf("error reading body: %w", err)
	}
	body = trapBOM(body)
	if isProductDataHoldings(body) {
		assets, gotAsOf, err := parseProductDataHoldings(body)
		if err != nil {
			return nil, nil, false, err
		}
		want := dateOnly(asOf).Format("20060102")
		if gotAsOf != want || len(assets) == 0 {
			return nil, nil, false, nil
		}
		return assets, body, true, nil
	}
	assets, published, err := parseHoldings(body)
	if err != nil {
		return nil, nil, false, err
	}
	return assets, body, published, nil
}

func parseCachedHoldings(body []byte) ([]Asset, bool, error) {
	if isProductDataHoldings(body) {
		assets, _, err := parseProductDataHoldings(body)
		if err != nil {
			return nil, false, err
		}
		return assets, len(assets) > 0, nil
	}
	return parseHoldings(body)
}

func isProductDataHoldings(body []byte) bool {
	return bytes.Contains(body, []byte(`"componentsByNameMap"`))
}

func parseProductDataHoldings(body []byte) ([]Asset, string, error) {
	var payload struct {
		Components map[string]struct {
			Containers map[string]struct {
				Points map[string]struct {
					Value json.RawMessage `json:"value"`
				} `json:"dataPointsByNameMap"`
			} `json:"containersByNameMap"`
		} `json:"componentsByNameMap"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, "", fmt.Errorf("iShares holdings were not valid JSON: %w", err)
	}
	all := payload.Components["holdings"].Containers["all"].Points
	asOf := rawScalar(all["asOfDate"].Value)
	tickers := rawStrings(all["ticker"].Value)
	names := rawStrings(all["issueName"].Value)
	sectors := rawStrings(all["sectorName"].Value)
	classes := rawStrings(all["assetClass"].Value)
	isins := rawStrings(all["isin"].Value)
	assets := make([]Asset, 0, len(tickers))
	for i, ticker := range tickers {
		if ticker == "" || fieldAt(classes, i) != "Equity" {
			continue
		}
		isin := fieldAt(isins, i)
		if isin == "" {
			continue
		}
		assets = append(assets, Asset{
			Symbol:    ticker,
			Name:      fieldAt(names, i),
			Sector:    fieldAt(sectors, i),
			AssetType: "Equity",
			ISIN:      isin,
		})
	}
	return assets, asOf, nil
}

func fieldAt(values []string, i int) string {
	if i < 0 || i >= len(values) {
		return ""
	}
	return values[i]
}

func rawStrings(raw json.RawMessage) []string {
	if len(bytes.TrimSpace(raw)) == 0 || string(raw) == "null" {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = rawScalar(item)
	}
	return out
}

func rawScalar(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var number json.Number
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&number); err == nil {
		return number.String()
	}
	return ""
}

func (e *ISharesETFLoopup) loadCached(etf string, asOf time.Time) ([]byte, bool, error) {
	if e.cache == nil {
		return nil, false, nil
	}
	body, ok, err := e.cache.load(etf, asOf)
	if err != nil {
		return nil, false, &SnapshotCacheError{Dir: e.cache.folder(etf), Err: err}
	}
	return body, ok, nil
}

func (e *ISharesETFLoopup) storeCached(etf string, asOf time.Time, body []byte) error {
	if e.cache == nil {
		return nil
	}
	if err := e.cache.store(etf, asOf, body); err != nil {
		return &SnapshotCacheError{Dir: e.cache.folder(etf), Write: true, Err: err}
	}
	return nil
}

// GetSnapshot returns Equity holdings (with ISIN) for etf as of asOf.
// Empty aaData is unpublished and is not cached; the previous 6 calendar days are tried.
// An HTML product page means that day has no holdings file. It is not cached and is not an error:
// the caller fills that month from another published listing.
func (e *ISharesETFLoopup) GetSnapshot(etf string, asOf time.Time) ([]Asset, error) {
	baseurl, err := e.productURL(etf)
	if err != nil {
		return nil, err
	}
	asOf = dateOnly(asOf)
	if body, ok, err := e.loadCached(etf, asOf); err != nil {
		return nil, err
	} else if ok {
		assets, _, err := parseCachedHoldings(body)
		if !errors.Is(err, errHoldingsPage) {
			return assets, err
		}
	}
	for i := 0; i <= snapshotLookbackDays; i++ {
		day := asOf.AddDate(0, 0, -i)
		if i > 0 {
			if body, ok, err := e.loadCached(etf, day); err != nil {
				return nil, err
			} else if ok {
				assets, published, err := parseCachedHoldings(body)
				if errors.Is(err, errHoldingsPage) {
					err = nil
					published = false
				}
				if err != nil {
					return nil, err
				}
				if published {
					if err := e.storeCached(etf, asOf, body); err != nil {
						return nil, err
					}
					return assets, nil
				}
			}
		}
		assets, body, published, err := e.fetchHoldings(baseurl, day)
		if errors.Is(err, errHoldingsPage) {
			// The product page is not a file for this day. The requested as-of is a gap;
			// a lookback day is just unpublished, so keep walking.
			if i == 0 {
				return []Asset{}, nil
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		if !published {
			continue
		}
		if err := e.storeCached(etf, asOf, body); err != nil {
			return nil, err
		}
		return assets, nil
	}
	return []Asset{}, nil
}

// Listing is the equity holdings used for one as-of in a create window.
// A gap month keeps a copy of another listing. FilledFrom is that as-of.
// Leading is set when the copy comes from a later listing because nothing earlier was published.
type Listing struct {
	AsOf       time.Time
	Assets     []Asset
	FilledFrom time.Time
	Leading    bool
}

// LoadSnapshotWindow loads each sampled as-of in [start, end], plus the seed:
// the latest listing published on or before start (§6.2 of universe.md). The
// seed keeps its own as-of, which can be before start, and a gap in the window
// is filled from it before any later listing, so no day reads ahead.
// A month with no holdings file is filled from the latest published listing on
// or before that date, or from the earliest listing after it when nothing
// earlier was published. The error names the ETF and the window only when
// nothing parsed.
func LoadSnapshotWindow(scraper ETFScraper, etf string, start, end time.Time) ([]Listing, error) {
	start = dateOnly(start)
	end = dateOnly(end)
	dates, err := scraper.ListSnapshotDates(etf, start, end)
	if err != nil {
		return nil, fmt.Errorf("error listing snapshot dates: %w", err)
	}
	seed, seeded, err := loadSeedListing(scraper, etf, start)
	if err != nil {
		return nil, err
	}
	listings := make([]Listing, 0, len(dates)+1)
	if seeded {
		listings = append(listings, seed)
	}
	for _, asOf := range dates {
		asOf = dateOnly(asOf)
		if seeded && asOf.Equal(seed.AsOf) {
			continue
		}
		assets, err := scraper.GetSnapshot(etf, asOf)
		if err != nil {
			return nil, err
		}
		listings = append(listings, Listing{AsOf: asOf, Assets: assets})
	}
	sort.Slice(listings, func(i, j int) bool { return listings[i].AsOf.Before(listings[j].AsOf) })
	if err := fillListings(etf, start, end, listings); err != nil {
		return nil, err
	}
	return listings, nil
}

// loadSeedListing returns the latest listing published on or before start.
// iShares serves a holdings file for any past as-of, so the month-ends before
// start are read the same way as the ones in the window. The walk is newest
// first and stops at the first published file; seedLookbackMonths bounds it,
// so an ETF that launched after start costs at most that many reads.
func loadSeedListing(scraper ETFScraper, etf string, start time.Time) (Listing, bool, error) {
	year, month, _ := start.Date()
	from := time.Date(year, month-seedLookbackMonths, 1, 0, 0, 0, 0, time.UTC)
	dates, err := scraper.ListSnapshotDates(etf, from, start)
	if err != nil {
		return Listing{}, false, fmt.Errorf("error listing snapshot dates before the start: %w", err)
	}
	candidates := make([]time.Time, 0, len(dates))
	for _, asOf := range dates {
		asOf = dateOnly(asOf)
		if asOf.Before(from) || asOf.After(start) {
			continue
		}
		candidates = append(candidates, asOf)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].After(candidates[j]) })
	for _, asOf := range candidates {
		assets, err := scraper.GetSnapshot(etf, asOf)
		if err != nil {
			return Listing{}, false, err
		}
		if len(assets) > 0 {
			return Listing{AsOf: asOf, Assets: assets}, true, nil
		}
	}
	return Listing{}, false, nil
}

func fillListings(etf string, start, end time.Time, listings []Listing) error {
	published := make([]int, 0, len(listings))
	for i := range listings {
		listings[i].AsOf = dateOnly(listings[i].AsOf)
		if len(listings[i].Assets) > 0 {
			published = append(published, i)
		}
	}
	if len(published) == 0 {
		return fmt.Errorf("iShares has no holdings file for %s between %s and %s",
			etf, dateOnly(start).Format("2006-01-02"), dateOnly(end).Format("2006-01-02"))
	}
	sort.Slice(published, func(i, j int) bool {
		return listings[published[i]].AsOf.Before(listings[published[j]].AsOf)
	})
	for i := range listings {
		if len(listings[i].Assets) > 0 {
			continue
		}
		gap := listings[i].AsOf
		src := latestPublishedOnOrBefore(listings, published, gap)
		leading := false
		if src < 0 {
			src = earliestPublishedAfter(listings, published, gap)
			leading = src >= 0
		}
		if src < 0 {
			continue
		}
		listings[i].Assets = cloneAssets(listings[src].Assets)
		listings[i].FilledFrom = listings[src].AsOf
		listings[i].Leading = leading
	}
	return nil
}

func latestPublishedOnOrBefore(listings []Listing, published []int, gap time.Time) int {
	src := -1
	for _, j := range published {
		if listings[j].AsOf.After(gap) {
			break
		}
		src = j
	}
	return src
}

func earliestPublishedAfter(listings []Listing, published []int, gap time.Time) int {
	for _, j := range published {
		if listings[j].AsOf.After(gap) {
			return j
		}
	}
	return -1
}

func cloneAssets(in []Asset) []Asset {
	out := make([]Asset, len(in))
	copy(out, in)
	return out
}

// ListSnapshotDates returns last-calendar-day-of-month dates in [start, end],
// plus any as-of files already present in the snapshot cache.
func (e *ISharesETFLoopup) ListSnapshotDates(etf string, start, end time.Time) ([]time.Time, error) {
	if _, err := e.productURL(etf); err != nil {
		return nil, err
	}
	if start.IsZero() || end.IsZero() {
		return nil, fmt.Errorf("start and end dates are required")
	}
	var cached []time.Time
	if e.cache != nil {
		var err error
		cached, err = e.cache.list(etf, start, end)
		if err != nil {
			return nil, &SnapshotCacheError{Dir: e.cache.folder(etf), Err: err}
		}
	}
	return mergeDates(monthEndDates(start, end), cached), nil
}
