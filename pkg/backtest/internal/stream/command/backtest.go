package command

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/internal/numfmt"
	"github.com/lhjnilsson/foreverbull/internal/storage"
	"github.com/lhjnilsson/foreverbull/internal/stream"
	"github.com/lhjnilsson/foreverbull/pkg/backtest/engine"
	"github.com/lhjnilsson/foreverbull/pkg/backtest/internal/repository"
	"github.com/lhjnilsson/foreverbull/pkg/backtest/internal/stream/dependency"
	"github.com/lhjnilsson/foreverbull/pkg/backtest/internal/universe"
	ss "github.com/lhjnilsson/foreverbull/pkg/backtest/stream"
	"github.com/lhjnilsson/foreverbull/pkg/finance"
	financeapi "github.com/lhjnilsson/foreverbull/pkg/finance/api"
	"github.com/lhjnilsson/foreverbull/pkg/finance/supplier"
	"github.com/lhjnilsson/foreverbull/pkg/pb"
	"github.com/lhjnilsson/foreverbull/pkg/pb/backtest"
	financeServicePB "github.com/lhjnilsson/foreverbull/pkg/pb/finance/service"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func CreateBacktestFromBacktest(ctx context.Context, msg stream.Message) error {
	command := ss.CreateBacktestFromBacktestCommand{}
	err := msg.ParsePayload(&command)
	if err != nil {
		return fmt.Errorf("error unmarshalling CreateBacktest payload: %w", err)
	}

	b := backtest.Backtest{}
	err = proto.Unmarshal(command.Backtest, &b)
	if err != nil {
		return fmt.Errorf("error unmarshalling backtest: %w", err)
	}

	// A universe file may spell a share class BRK.B, BRK-B or BRKB; all are
	// stored as BRKB. ETF holdings keep their own names (aliases).
	if err = storeShareClassesOnce(&b); err != nil {
		return fmt.Errorf("error creating backtest: %w", err)
	}

	// Over the download cap: refuse before anything is stored.
	if err := tooManySymbols(b.GetName(), fileBacktestSymbols(&b)); err != nil {
		return err
	}

	db := msg.MustGet(stream.DBDep).(*pgxpool.Pool)
	backtests := repository.Backtest{Conn: db}
	if err = createStoredBacktest(ctx, backtests, command.OwnerID, &b); err != nil {
		return fmt.Errorf("error creating backtest: %w", err)
	}
	return nil
}

func CreateBacktestFromETF(ctx context.Context, msg stream.Message) error {
	command := ss.CreateBacktestFromETFCommand{}

	err := msg.ParsePayload(&command)
	if err != nil {
		return fmt.Errorf("error unmarshalling CreateBacktest payload: %w", err)
	}
	command.BacktestName = strings.TrimSpace(command.BacktestName)
	command.ETFName = strings.TrimSpace(command.ETFName)
	command.StartDate = strings.TrimSpace(command.StartDate)
	command.EndDate = strings.TrimSpace(command.EndDate)

	// Same sentences as the create prompt. Fail before any holdings download.
	start, end, err := validateETFCreateInput(command.BacktestName, command.ETFName, command.StartDate, command.EndDate, time.Now())
	if err != nil {
		return err
	}

	etfscraper, isETFScraper := msg.MustGet(dependency.GetETFScraper).(dependency.ETFScraper)
	if !isETFScraper {
		return errors.New("error casting ETFScraper")
	}

	snapshots, err := loadETFSnapshots(etfscraper, command.ETFName, start, end)
	if err != nil {
		return err
	}
	result := universe.Diff(snapshots)

	benchmark, err := etfscraper.GetBenchmark(command.ETFName)
	if err != nil {
		return fmt.Errorf("error getting benchmark: %w", err)
	}

	marketdataClient, isMarketdataClient := msg.MustGet(dependency.GetMarketdataClient).(financeServicePB.MarketdataClient)
	if !isMarketdataClient {
		return errors.New("error casting marketdata client")
	}

	symbols := uniqueAliasSymbols(result.Aliases)
	// Over the download cap: refuse before any provider call or store.
	if err := tooManySymbols(command.BacktestName, append([]string{benchmark}, symbols...)); err != nil {
		return err
	}
	// Every member must resolve before anything is stored. A member that
	// is left out instead changes the universe without a word.
	failures, err := lookupMemberAssets(ctx, marketdataClient, symbols)
	if err != nil {
		return err
	}
	if len(failures) > 0 {
		return memberLookupError(command.BacktestName, failures)
	}

	b := backtest.Backtest{
		Name:      command.BacktestName,
		Benchmark: benchmark,
		Provider:  command.Provider,
		Profile:   command.Profile,
		Periods:   calendarYearPeriods(start, end),
	}
	fillPeriodAssets(b.Periods, result.Aliases)

	if err := requireStoredOwner(command.OwnerID, ""); err != nil {
		return err
	}
	db := msg.MustGet(stream.DBDep).(*pgxpool.Pool)
	backtests := repository.Backtest{Conn: db}
	if err = createStoredBacktest(ctx, backtests, command.OwnerID, &b); err != nil {
		return fmt.Errorf("error creating backtest: %w", err)
	}
	backtestID := ""
	if environment.GetAuthEnabled() {
		backtestID, err = backtests.IDByOwnerAndName(ctx, command.OwnerID, command.BacktestName)
		if err != nil {
			return fmt.Errorf("error getting backtest: %w", err)
		}
	}
	if err := persistUniverse(ctx, db, command.BacktestName, backtestID, snapshots, result); err != nil {
		return err
	}

	return nil
}

var isoDatePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

func validateETFCreateInput(name, etf, startRaw, endRaw string, now time.Time) (time.Time, time.Time, error) {
	name = strings.TrimSpace(name)
	etf = strings.TrimSpace(etf)
	startRaw = strings.TrimSpace(startRaw)
	endRaw = strings.TrimSpace(endRaw)
	if name == "" {
		return time.Time{}, time.Time{}, plainSentence("Enter a name for the backtest.")
	}
	product, ok := etfByName(etf)
	if !ok {
		return time.Time{}, time.Time{}, plainSentence("Unknown ETF " + pythonRepr(etf) + ". Known funds: " + knownETFNames() + ".")
	}
	start, ok := parseISODate(startRaw)
	if !ok {
		return time.Time{}, time.Time{}, plainSentence(pythonRepr(startRaw) + " is not a date. Use YYYY-MM-DD, for example 2024-01-31.")
	}
	if start.Before(product.Inception) {
		return time.Time{}, time.Time{}, plainSentence(etf + " started on " + product.Inception.Format("2006-01-02") + ". Choose a start date on or after that day.")
	}
	today := dateOnly(now)
	if start.After(today) {
		return time.Time{}, time.Time{}, plainSentence(start.Format("2006-01-02") + " is after today (" + today.Format("2006-01-02") + ").")
	}
	end, ok := parseISODate(endRaw)
	if !ok {
		return time.Time{}, time.Time{}, plainSentence(pythonRepr(endRaw) + " is not a date. Use YYYY-MM-DD, for example 2024-01-31.")
	}
	if end.Before(start) {
		return time.Time{}, time.Time{}, plainSentence("End date " + end.Format("2006-01-02") + " is before start date " + start.Format("2006-01-02") + ".")
	}
	if end.After(today) {
		return time.Time{}, time.Time{}, plainSentence("End date " + end.Format("2006-01-02") + " is after today (" + today.Format("2006-01-02") + ").")
	}
	return start, end, nil
}

// plainSentence is the prompt text. It is not wrapped, so callers see that sentence only.
func plainSentence(text string) error {
	return errors.New(text)
}

func etfByName(name string) (dependency.ETFProduct, bool) {
	for _, product := range dependency.ETFCatalog() {
		if product.Name == name {
			return product, true
		}
	}
	return dependency.ETFProduct{}, false
}

func knownETFNames() string {
	products := dependency.ETFCatalog()
	names := make([]string, len(products))
	for i, product := range products {
		names[i] = product.Name
	}
	return strings.Join(names, ", ")
}

func parseISODate(raw string) (time.Time, bool) {
	if !isoDatePattern.MatchString(raw) {
		return time.Time{}, false
	}
	parsed, err := time.Parse("2006-01-02", raw)
	if err != nil {
		return time.Time{}, false
	}
	parsed = dateOnly(parsed)
	if parsed.Format("2006-01-02") != raw {
		return time.Time{}, false
	}
	return parsed, true
}

// pythonRepr matches Python str repr so the server sentence is the prompt sentence.
func pythonRepr(s string) string {
	quote := "'"
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		quote = `"`
	}
	var b strings.Builder
	b.WriteString(quote)
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '\'':
			if quote == "'" {
				b.WriteString(`\'`)
			} else {
				b.WriteRune(r)
			}
		case '"':
			if quote == `"` {
				b.WriteString(`\"`)
			} else {
				b.WriteRune(r)
			}
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\x%02x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteString(quote)
	return b.String()
}

func loadETFSnapshots(scraper dependency.ETFScraper, etf string, start, end time.Time) ([]universe.Snapshot, error) {
	listings, err := dependency.LoadSnapshotWindow(scraper, etf, start, end)
	if err != nil {
		return nil, err
	}
	listings = seedBacktestStart(etf, listings, start)
	recordLeadingHoldingsGap(etf, listings)

	snaps := make([]universe.Snapshot, 0, len(listings))
	for _, listing := range listings {
		holdings := holdingsFromAssets(listing.Assets)
		if len(holdings) == 0 {
			continue
		}
		// Copied rows keep the source ISIN and ticker, so a gap is not a drop or a rename.
		snaps = append(snaps, universe.Snapshot{AsOf: listing.AsOf, Holdings: holdings})
	}
	if len(snaps) == 0 {
		return nil, fmt.Errorf("iShares has no holdings file for %s between %s and %s",
			etf, dateOnly(start).Format("2006-01-02"), dateOnly(end).Format("2006-01-02"))
	}
	return snaps, nil
}

// seedBacktestStart opens membership on the backtest start from the last list
// published on or before it. Diff opens every interval at the first as-of, so
// that list is moved to the start and held until the next as-of. Listings
// before the start are dropped; the backtest does not read them.
//
// A list published after the start is a look-ahead. It is used only when
// nothing was published on or before the start, so the universe is not empty,
// and the warning names the ETF, the start, and the as-of that was used.
func seedBacktestStart(etf string, listings []dependency.Listing, start time.Time) []dependency.Listing {
	start = dateOnly(start)
	src := 0
	for i := range listings {
		if !dateOnly(listings[i].AsOf).After(start) {
			src = i
		}
	}
	published := publishedAsOf(listings[src])
	if published.After(start) {
		log.Warn().
			Str("etf", etf).
			Str("start_date", start.Format("2006-01-02")).
			Str("snapshot_date", published.Format("2006-01-02")).
			Msg("no holdings snapshot published on or before the backtest start, seeded from the first one after it")
	}
	seeded := make([]dependency.Listing, 0, len(listings)+1)
	seeded = append(seeded, dependency.Listing{
		AsOf:       start,
		Assets:     slices.Clone(listings[src].Assets),
		FilledFrom: published,
	})
	for _, listing := range listings {
		if dateOnly(listing.AsOf).After(start) {
			seeded = append(seeded, listing)
		}
	}
	return seeded
}

// publishedAsOf is the as-of of the file a listing's rows came from.
func publishedAsOf(listing dependency.Listing) time.Time {
	if listing.FilledFrom.IsZero() {
		return dateOnly(listing.AsOf)
	}
	return dateOnly(listing.FilledFrom)
}

// recordLeadingHoldingsGap logs that a leading gap kept a later published listing.
func recordLeadingHoldingsGap(etf string, listings []dependency.Listing) {
	var first, last, from time.Time
	count := 0
	for _, listing := range listings {
		if !listing.Leading || listing.FilledFrom.IsZero() {
			continue
		}
		count++
		if count == 1 || listing.AsOf.Before(first) {
			first = listing.AsOf
			from = listing.FilledFrom
		}
		if !listing.AsOf.Before(last) {
			last = listing.AsOf
		}
	}
	if count == 0 {
		return
	}
	log.Warn().
		Str("etf", etf).
		Str("gap_start", first.Format("2006-01-02")).
		Str("gap_end", last.Format("2006-01-02")).
		Str("filled_from", from.Format("2006-01-02")).
		Msg("leading holdings gap filled from a later published listing")
}

func holdingsFromAssets(assets []dependency.Asset) []universe.Holding {
	holdings := make([]universe.Holding, 0, len(assets))
	for _, asset := range assets {
		holdings = append(holdings, universe.Holding{
			ISIN:      asset.ISIN,
			Symbol:    asset.Symbol,
			Name:      asset.Name,
			AssetType: asset.AssetType,
		})
	}
	return holdings
}

func calendarYearPeriods(start, end time.Time) []*backtest.Backtest_Period {
	periods := make([]*backtest.Backtest_Period, 0, end.Year()-start.Year()+1)
	for year := start.Year(); year <= end.Year(); year++ {
		periods = append(periods, &backtest.Backtest_Period{
			StartDate: &pb.Date{Year: int32(year), Month: 1, Day: 1},
			EndDate:   &pb.Date{Year: int32(year), Month: 12, Day: 31},
		})
	}
	return periods
}

func uniqueAliasSymbols(aliases []universe.Alias) []string {
	seen := make(map[string]struct{}, len(aliases))
	symbols := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		if alias.Symbol == "" {
			continue
		}
		if _, ok := seen[alias.Symbol]; ok {
			continue
		}
		seen[alias.Symbol] = struct{}{}
		symbols = append(symbols, alias.Symbol)
	}
	sort.Strings(symbols)
	return symbols
}

// memberLookupProvider is the provider every member lookup goes through.
// GetAsset is Yahoo for every backtest provider, so the copy names it. A
// download's asset and ISIN lookup is Yahoo too, whatever --provider
// supplies the bars, so its no-ISIN sentence names it and another
// --provider can't help.
const memberLookupProvider = "yahoo"

// memberLookupFailure is a member symbol whose GetAsset failed. noISIN is
// set only when the provider answered that the equity has no ISIN; any
// other error is a failed lookup.
type memberLookupFailure struct {
	symbol string
	noISIN bool
}

// lookupMemberAssets asks the finance service for every member symbol and
// returns each one that failed, in the order of symbols. The loop is
// bounded by symbols. A canceled context returns ctx.Err(). The cause is
// logged with any URL query removed; it never reaches the user.
func lookupMemberAssets(ctx context.Context, client financeServicePB.MarketdataClient, symbols []string) ([]memberLookupFailure, error) {
	failures := make([]memberLookupFailure, 0)
	for _, symbol := range symbols {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		_, err := client.GetAsset(ctx, &financeServicePB.GetAssetRequest{
			Identifier: &financeServicePB.GetAssetRequest_Symbol{Symbol: symbol},
		})
		if err == nil {
			continue
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		log.Error().Str("symbol", symbol).Str("code", status.Code(err).String()).
			Str("error", redactURLQuery(rpcErrorText(err))).Msg("member asset lookup failed")
		failures = append(failures, memberLookupFailure{symbol: symbol, noISIN: isNoISINLookupError(err)})
	}
	return failures, nil
}

// isNoISINLookupError reports whether a GetAsset error is the provider's
// answer that the equity has no ISIN: a NotFound status whose message is
// the finance service's no-ISIN text (financeapi.IsNoISIN). Without an ISIN
// the member can't be followed across ticker changes. Anything else is a
// failed lookup.
func isNoISINLookupError(err error) bool {
	st, ok := status.FromError(err)
	if !ok {
		return false
	}
	return st.Code() == codes.NotFound && financeapi.IsNoISIN(st.Message())
}

// rpcErrorText is the gRPC status message, or the error text when err
// carries no status.
func rpcErrorText(err error) string {
	if st, ok := status.FromError(err); ok {
		return st.Message()
	}
	return err.Error()
}

// urlQuery matches the query and fragment of a URL in error text. Yahoo
// requests carry the session crumb as ?crumb=.
var urlQuery = regexp.MustCompile(`(https?://[^\s?#"']+)[?#][^\s"']*`)

// redactURLQuery keeps scheme, host and path of every URL in text.
func redactURLQuery(text string) string {
	return urlQuery.ReplaceAllString(text, "$1")
}

// memberLookupError is the create error for members whose asset lookup
// failed. One symbol gets one sentence. Several get a header, one line per
// cause with its symbols A-Z, and the next step, so every symbol is named.
// Nothing was stored yet, so the backtest wasn't saved. The lookup is
// always Yahoo, so another --provider is not offered.
func memberLookupError(name string, failures []memberLookupFailure) error {
	if len(failures) == 1 {
		symbol := failures[0].symbol
		if failures[0].noISIN {
			return plainSentence(fmt.Sprintf("Couldn't load data for backtest '%s': %s %s %s",
				name, noISINLead(symbol), notSavedSentence, fmt.Sprintf(noISINNextStep, symbol)))
		}
		return plainSentence(fmt.Sprintf(
			"Couldn't load data for backtest '%s': %s didn't respond when looking up %s. The backtest wasn't saved. "+
				"Wait a few minutes, then run `fbull backtest create` again.",
			name, memberLookupProvider, symbol))
	}
	failed := make([]string, 0, len(failures))
	noISIN := make([]string, 0, len(failures))
	for _, failure := range failures {
		if failure.noISIN {
			noISIN = append(noISIN, failure.symbol)
		} else {
			failed = append(failed, failure.symbol)
		}
	}
	sort.Strings(failed)
	sort.Strings(noISIN)
	var b strings.Builder
	fmt.Fprintf(&b, "Couldn't load data for backtest '%s': %d symbols couldn't be used. The backtest wasn't saved.\n",
		name, len(failures))
	if len(failed) == 1 {
		fmt.Fprintf(&b, "  %s: %s didn't respond when looking it up. Try again in a few minutes.\n",
			failed[0], memberLookupProvider)
	} else if len(failed) > 1 {
		fmt.Fprintf(&b, "  %s: %s didn't respond when looking them up. Try again in a few minutes.\n",
			strings.Join(failed, ", "), memberLookupProvider)
	}
	if len(noISIN) == 1 {
		fmt.Fprintf(&b, "  %s: %s doesn't identify it well enough to follow ticker changes. Remove it.\n",
			noISIN[0], memberLookupProvider)
	} else if len(noISIN) > 1 {
		fmt.Fprintf(&b, "  %s: %s doesn't identify them well enough to follow ticker changes. Remove them.\n",
			strings.Join(noISIN, ", "), memberLookupProvider)
	}
	b.WriteString("Then run `fbull backtest create` again.")
	return plainSentence(b.String())
}

// fillPeriodAssets lists each member symbol in the periods it overlaps. Every
// symbol already resolved (lookupMemberAssets), so each one is enabled.
func fillPeriodAssets(periods []*backtest.Backtest_Period, aliases []universe.Alias) {
	for _, period := range periods {
		start := pb.DateToTime(period.StartDate)
		end := pb.DateToTime(period.EndDate)
		seen := make(map[string]struct{})
		assets := make([]*backtest.Backtest_Period_Asset, 0)
		for _, alias := range aliases {
			if alias.Symbol == "" || !intervalOverlapsPeriod(alias.FromDate, alias.ToDate, start, end) {
				continue
			}
			if _, ok := seen[alias.Symbol]; ok {
				continue
			}
			seen[alias.Symbol] = struct{}{}
			assets = append(assets, &backtest.Backtest_Period_Asset{
				Symbol:  alias.Symbol,
				Enabled: true,
			})
		}
		sort.Slice(assets, func(i, j int) bool { return assets[i].Symbol < assets[j].Symbol })
		period.Assets = assets
	}
}

func intervalOverlapsPeriod(from time.Time, to *time.Time, start, end time.Time) bool {
	from = dateOnly(from)
	start = dateOnly(start)
	end = dateOnly(end)
	if from.After(end) {
		return false
	}
	if to == nil {
		return true
	}
	return dateOnly(*to).After(start)
}

func persistUniverse(ctx context.Context, db *pgxpool.Pool, backtestName, backtestID string, snaps []universe.Snapshot, result universe.Result) error {
	names := instrumentNames(snaps)
	instruments := repository.Instrument{Conn: db}
	for _, member := range result.Members {
		if err := instruments.Upsert(ctx, member.ISIN, names[member.ISIN]); err != nil {
			return fmt.Errorf("error storing instrument %s: %w", member.ISIN, err)
		}
	}
	for _, alias := range result.Aliases {
		if err := instruments.StoreAlias(ctx, repository.AliasInterval{
			ISIN:     alias.ISIN,
			Symbol:   alias.Symbol,
			FromDate: alias.FromDate,
			ToDate:   alias.ToDate,
		}); err != nil {
			return fmt.Errorf("error recording symbol %s for %s: %w", alias.Symbol, alias.ISIN, err)
		}
	}

	intervals := make([]repository.MemberInterval, 0, len(result.Members))
	for _, member := range result.Members {
		intervals = append(intervals, repository.MemberInterval{
			ISIN:     member.ISIN,
			FromDate: member.FromDate,
			ToDate:   member.ToDate,
		})
	}
	members := repository.UniverseMember{Conn: db}
	if environment.GetAuthEnabled() {
		if backtestID == "" {
			return errors.New("backtest id is empty")
		}
		if err := members.ReplaceByID(ctx, backtestID, intervals); err != nil {
			return fmt.Errorf("error storing universe members: %w", err)
		}
		return nil
	}
	if err := members.Replace(ctx, backtestName, intervals); err != nil {
		return fmt.Errorf("error storing universe members: %w", err)
	}
	return nil
}

func instrumentNames(snaps []universe.Snapshot) map[string]string {
	names := make(map[string]string)
	for _, snap := range snaps {
		for _, holding := range snap.Holdings {
			if holding.ISIN == "" {
				continue
			}
			names[holding.ISIN] = holding.Name
		}
	}
	return names
}

func dateOnly(t time.Time) time.Time {
	year, month, day := t.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

func ingestSymbols(ctx context.Context, db *pgxpool.Pool, b *backtest.Backtest, backtestID string) ([]string, error) {
	symbols := []string{b.Benchmark}
	column := "m.backtest_name"
	key := b.Name
	if environment.GetAuthEnabled() {
		if backtestID == "" {
			return nil, errors.New("backtest id is empty")
		}
		column = "m.backtest_id"
		key = backtestID
	}

	rows, err := db.Query(ctx, `
		SELECT DISTINCT s.symbol
		  FROM universe_member m
		  JOIN asset_symbol s ON s.isin = m.isin
		 WHERE `+column+` = $1
		 ORDER BY s.symbol`, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	aliases := make([]string, 0)
	for rows.Next() {
		var symbol string
		if err := rows.Scan(&symbol); err != nil {
			return nil, err
		}
		aliases = append(aliases, symbol)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(aliases) > 0 {
		for _, symbol := range aliases {
			if slices.Contains(symbols, symbol) {
				continue
			}
			symbols = append(symbols, symbol)
		}
		return symbols, nil
	}

	for _, period := range b.Periods {
		for _, asset := range period.Assets {
			if slices.Contains(symbols, asset.Symbol) {
				continue
			}
			symbols = append(symbols, asset.Symbol)
		}
	}
	return symbols, nil
}

func DownloadFinancialData(ctx context.Context, msg stream.Message) error {
	command := ss.DownloadFinancialDataCommand{}

	err := msg.ParsePayload(&command)
	if err != nil {
		return fmt.Errorf("error unmarshalling DownloadFinancialDataCommand payload: %w", err)
	}

	financeClient, isMarketdataClient := msg.MustGet(dependency.GetMarketdataClient).(financeServicePB.MarketdataClient)
	if !isMarketdataClient {
		return errors.New("error casting marketdata client")
	}
	db := msg.MustGet(stream.DBDep).(*pgxpool.Pool)
	backtests := repository.Backtest{Conn: db}
	b, backtestID, err := loadStoredBacktest(ctx, db, command.BacktestName, command.OwnerID, command.BacktestID)
	if err != nil {
		return fmt.Errorf("error getting backtest: %w", err)
	}

	symbols, err := ingestSymbols(ctx, db, b, backtestID)
	if err != nil {
		return fmt.Errorf("error listing ingest symbols: %w", err)
	}
	if count := finance.DownloadSymbolCount(symbols); count > finance.MaxDownloadSymbols {
		return discardOversizedBacktest(ctx, backtests, command.BacktestName, backtestID, count)
	}

	provider := strings.TrimSpace(b.GetProvider())
	if provider == "" {
		provider = supplier.DefaultProvider
	}
	if err := finance.ValidatePriceSource(provider, b.GetProfile()); err != nil {
		return fmt.Errorf("price source: %w", err)
	}

	startDate := b.Periods[0].StartDate
	endDate := b.Periods[len(b.Periods)-1].EndDate
	period, err := readClosedPeriod(ctx, msg, backtests, command.BacktestName, backtestID,
		pb.DateToTime(startDate), pb.DateToTime(endDate))
	if err != nil {
		return err
	}
	progress := newDownloadProgress(ctx, finance.DownloadSymbolCount(symbols))
	progress.start()
	rsp, err := financeClient.DownloadHistoricalData(ctx, &financeServicePB.DownloadHistoricalDataRequest{
		Symbols:   symbols,
		StartDate: startDate,
		EndDate:   endDate,
		Provider:  provider,
		Profile:   b.GetProfile(),
	})
	if err != nil {
		if position, ok := invalidTickerPosition(err, len(symbols)); ok {
			return discardInvalidTickerBacktest(ctx, backtests, command.BacktestName, backtestID, position, len(symbols))
		}
		if overCapRefusal(err) {
			return discardOversizedBacktest(ctx, backtests, command.BacktestName, backtestID, finance.DownloadSymbolCount(symbols))
		}
		return discardFailedBacktest(ctx, backtests, command.BacktestName, backtestID, err)
	}
	// Days the provider sent no price for, anywhere in the range, per
	// request symbol. They join the stored-bar check, so create fails once.
	reported := make(map[string][]time.Time)
	for {
		msg, err := rsp.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			if blocked := blockedDownload(err); blocked != nil {
				discardBlockedBacktest(ctx, backtests, command.BacktestName, backtestID)
				return blocked
			}
			if limit, ok := downloadTimeLimit(err); ok {
				return discardTimeLimitBacktest(ctx, backtests, command.BacktestName, backtestID,
					limit, progress.done, progress.total)
			}
			if position, ok := invalidTickerPosition(err, len(symbols)); ok {
				return discardInvalidTickerBacktest(ctx, backtests, command.BacktestName, backtestID, position, len(symbols))
			}
			if overCapRefusal(err) {
				return discardOversizedBacktest(ctx, backtests, command.BacktestName, backtestID, finance.DownloadSymbolCount(symbols))
			}
			return discardFailedBacktest(ctx, backtests, command.BacktestName, backtestID, err)
		}
		if msg.GetStatus() == financeServicePB.DownloadHistoricalDataResponse_STATUS_ERROR {
			if days, ok := providerMissingDays(msg.GetError()); ok {
				reported[msg.Symbol] = append(reported[msg.Symbol], days...)
				progress.symbolDone()
				continue
			}
			// Any per-symbol download failure must fail create. Disabling the
			// member and exiting 0 left an incomplete universe with no warning.
			return discardSymbolDownloadError(ctx, backtests, command.BacktestName, backtestID,
				provider, msg.Symbol, startDate, endDate, msg.GetError())
		}
		progress.symbolDone()
	}

	return checkMissingSessions(ctx, msg, db, backtests, command.BacktestName, backtestID,
		strings.ToLower(provider), b.GetProfile(), symbols, reported, period)
}

// isNoPriceDataError reports the finance service's no-bars STATUS_ERROR.
func isNoPriceDataError(text string) bool {
	return strings.HasPrefix(text, "no price data for ")
}

// refusedPriceError and refusedVolumeError match the finance service's
// sentences for a bar its storage refused. Only these exact shapes are shown;
// other raw text is not.
var (
	refusedPriceError  = regexp.MustCompile(`^(yahoo|stooq|alpaca) sent an invalid price for [A-Za-z0-9.^=_-]{1,32} on \d{4}-\d{2}-\d{2} \(0, negative or not a number\)$`)
	refusedVolumeError = regexp.MustCompile(`^(yahoo|stooq|alpaca) sent an invalid volume for [A-Za-z0-9.^=_-]{1,32} on \d{4}-\d{2}-\d{2} \(out of range\)$`)
)

// ambiguousSymbolError matches the finance service's STATUS_ERROR for a ticker
// that maps to more than one instrument. Only this exact shape is shown.
var ambiguousSymbolError = regexp.MustCompile(`^(yahoo|stooq|alpaca) has more than one instrument for ([A-Za-z0-9.^=_-]{1,32})$`)

// ambiguousSymbolProvider returns the provider named in text when it is the
// ambiguous-ticker sentence for symbol, the ticker this response is about.
func ambiguousSymbolProvider(text, symbol string) (string, bool) {
	match := ambiguousSymbolError.FindStringSubmatch(text)
	if match == nil {
		return "", false
	}
	if match[2] == symbol {
		return match[1], true
	}
	return "", false
}

// providerTimeoutError matches the finance service's STATUS_ERROR for a
// provider that didn't answer for a symbol in time. Only this exact shape is
// shown.
var providerTimeoutError = regexp.MustCompile(`^(yahoo|stooq|alpaca) took too long to respond for ([A-Za-z0-9.^=_-]{1,32})$`)

// providerTimeoutProvider returns the provider named in text when it is the
// timeout sentence for symbol, the ticker this response is about.
func providerTimeoutProvider(text, symbol string) (string, bool) {
	match := providerTimeoutError.FindStringSubmatch(text)
	if match == nil {
		return "", false
	}
	if match[2] == symbol {
		return match[1], true
	}
	return "", false
}

// timeLimitError matches the finance service's DeadlineExceeded text when the
// download stopped at its own time limit (FINANCE_TASK_TIME_LIMIT), not at a
// slow provider. The limit is in Go's duration form.
var timeLimitError = regexp.MustCompile(`^the download hit its time limit of ([0-9][0-9hms.]{0,24}) at [A-Za-z0-9.^=_-]{1,32}$`)

// downloadTimeLimit returns the limit when err is the finance service stopping
// the download at its time limit: DeadlineExceeded with that exact text and a
// positive limit. The client's own deadline carries other text and keeps the
// generic sentence.
func downloadTimeLimit(err error) (time.Duration, bool) {
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.DeadlineExceeded {
		return 0, false
	}
	match := timeLimitError.FindStringSubmatch(st.Message())
	if match == nil {
		return 0, false
	}
	limit, err := time.ParseDuration(match[1])
	if err != nil || limit <= 0 {
		return 0, false
	}
	return limit, true
}

// discardTimeLimitBacktest deletes the backtest after the download hit the
// finance service's time limit. done and total are the counts the progress
// line shows.
func discardTimeLimitBacktest(ctx context.Context, backtests repository.Backtest, name, id string,
	limit time.Duration, done, total int,
) error {
	err := deleteStoredBacktest(ctx, backtests, name, id)
	removed := err == nil || errors.Is(err, pgx.ErrNoRows)
	if !removed {
		log.Warn().Err(err).Str("backtest", name).Msg("error deleting backtest after the download time limit")
	}
	return plainSentence(timeLimitMessage(name, limit, done, total, removed))
}

// timeLimitLead names the limit, such as "the 2-hour limit", and how far
// the download got, with the progress line's count and thousands formatting.
func timeLimitLead(limit time.Duration, done, total int) string {
	return fmt.Sprintf("the download hit the %s limit after %s of %s symbols.",
		numfmt.LimitSpan(limit), numfmt.Thousands(done), numfmt.Thousands(total))
}

// timeLimitMessage is the create sentence for the time limit. "wasn't
// saved" only when the delete worked; otherwise it was partly saved and the
// step says to delete it first.
func timeLimitMessage(name string, limit time.Duration, done, total int, removed bool) string {
	saved, next := savedAndNext(name,
		"Split the symbols across several backtests, then run `fbull backtest create` again.", removed)
	return fmt.Sprintf("Couldn't load data for backtest '%s': %s %s %s", name, timeLimitLead(limit, done, total), saved, next)
}

// isRefusedBarError reports the finance service's refused-bar sentence.
func isRefusedBarError(text string) bool {
	return refusedPriceError.MatchString(text) || refusedVolumeError.MatchString(text)
}

// providerFaultNextStep ends a sentence about bad provider data. A bad bar
// normally fails create as a missing session (universe.md §7.5), so reaching
// this means an adapter missed one; a plain retry won't help.
const providerFaultNextStep = "Try another provider with --provider, " +
	"or report it at https://github.com/lhjnilsson/foreverbull/issues."

// noPriceDataNextStep ends the sentence when a provider returned no bars. The
// finance service already retried an empty download once, so a wrong symbol
// or wrong dates come first, then another provider.
const noPriceDataNextStep = "Check the symbol and dates, or try another provider " +
	"with --provider, then run `fbull backtest create` again."

// noISINLead says that the asset lookup can't identify symbol, an asked
// equity it has no ISIN for. A member lookup and a download share it.
func noISINLead(symbol string) string {
	return fmt.Sprintf("%s doesn't identify %s well enough to follow ticker changes, so it can't be used.",
		memberLookupProvider, symbol)
}

// noISINNextStep ends the sentence when the asset lookup can't identify an
// asked equity. A retry or another price provider can't help; only leaving
// it out can.
const noISINNextStep = "Remove %s, then run `fbull backtest create` again."

// discardSymbolDownloadError deletes the backtest when a symbol's download
// returns STATUS_ERROR. The create must not keep a universe that silently
// omits a member.
func discardSymbolDownloadError(ctx context.Context, backtests repository.Backtest, name, id, provider, symbol string, start, end *pb.Date, errText string) error {
	err := deleteStoredBacktest(ctx, backtests, name, id)
	removed := err == nil || errors.Is(err, pgx.ErrNoRows)
	if !removed {
		log.Warn().Err(err).Str("backtest", name).Msg("error deleting backtest after download error")
	}
	return plainSentence(symbolDownloadMessage(name, provider, symbol, start, end, errText, removed))
}

// symbolDownloadMessage is the message for a symbol's STATUS_ERROR. The lead
// names the symbol; raw finance/gRPC text stays out of the user sentence.
// "wasn't saved" only when the delete worked; otherwise it was partly saved
// and the step says to delete it first.
func symbolDownloadMessage(name, provider, symbol string, start, end *pb.Date, errText string, removed bool) string {
	var lead, next string
	switch {
	case isNoPriceDataError(errText):
		lead = fmt.Sprintf("%s has no prices for %s between %s and %s.", strings.ToLower(provider), symbol,
			pb.DateToDateString(start), pb.DateToDateString(end))
		next = noPriceDataNextStep
	case financeapi.IsNoISIN(errText):
		lead = noISINLead(symbol)
		next = fmt.Sprintf(noISINNextStep, symbol)
	case isRefusedBarError(errText):
		lead = errText + "."
		next = providerFaultNextStep
	default:
		if timedOut, ok := providerTimeoutProvider(errText, symbol); ok {
			lead = fmt.Sprintf("%s took too long to respond for %s.", timedOut, symbol)
			next = "Try again later, or use another provider with --provider."
		} else if ambiguous, ok := ambiguousSymbolProvider(errText, symbol); ok {
			lead = fmt.Sprintf("%s has more than one instrument for %s.", ambiguous, symbol)
			next = "Use the exact ticker with its exchange suffix (for example VOD.L), " +
				"or use another provider with --provider."
		} else {
			lead = fmt.Sprintf("the price download failed for %s.", symbol)
			next = "Try again in a few minutes, or use another provider with --provider."
		}
	}
	saved, next := savedAndNext(name, next, removed)
	return fmt.Sprintf("Couldn't load data for backtest '%s': %s %s %s", name, lead, saved, next)
}

// invalidTickerError matches the finance service's InvalidArgument refusal
// of a request symbol outside the ticker pattern. Only the position and the
// count are taken from it; the symbol is never in it.
var invalidTickerError = regexp.MustCompile(`^symbol ([1-9][0-9]{0,5}) of ([1-9][0-9]{0,5}) isn't a valid ticker \(use 1 to 32 letters, digits, or \. \^ = _ -\)$`)

// invalidTickerPosition returns the 1-based position of the first invalid
// symbol when err is that refusal for this request: InvalidArgument, the
// exact sentence, a position within the count, and the count this create
// sent. Anything else is not shown this way.
func invalidTickerPosition(err error, sent int) (int, bool) {
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.InvalidArgument {
		return 0, false
	}
	match := invalidTickerError.FindStringSubmatch(st.Message())
	if match == nil {
		return 0, false
	}
	position, err := strconv.Atoi(match[1])
	if err != nil {
		return 0, false
	}
	total, err := strconv.Atoi(match[2])
	if err != nil {
		return 0, false
	}
	if total != sent {
		return 0, false
	}
	if position <= total {
		return position, true
	}
	return 0, false
}

// discardInvalidTickerBacktest deletes the backtest after the finance service
// refused a request symbol. The sentence names the symbol's position and the
// count, not the symbol, which failed the ticker check.
func discardInvalidTickerBacktest(ctx context.Context, backtests repository.Backtest, name, id string, position, total int) error {
	err := deleteStoredBacktest(ctx, backtests, name, id)
	removed := err == nil || errors.Is(err, pgx.ErrNoRows)
	if !removed {
		log.Warn().Err(err).Str("backtest", name).Msg("error deleting backtest after an invalid ticker")
	}
	return plainSentence(invalidTickerMessage(name, position, total, removed))
}

// invalidTickerMessage is the message for a refused request symbol.
func invalidTickerMessage(name string, position, total int, removed bool) string {
	lead := fmt.Sprintf("symbol %d of %d isn't a valid ticker. Use 1 to 32 letters, digits, or . ^ = _ -.", position, total)
	saved, next := savedAndNext(name, "Fix that symbol and run `fbull backtest create` again.", removed)
	return fmt.Sprintf("Couldn't load data for backtest '%s': %s %s %s", name, lead, saved, next)
}

// blockedDownload returns the finance service's sentence when the price
// provider blocked the download, or nil. The service uses FailedPrecondition
// only for that, and the sentence is the whole error, not wrapped in gRPC
// detail.
func blockedDownload(err error) error {
	if st, ok := status.FromError(err); ok && st.Code() == codes.FailedPrecondition {
		return errors.New(st.Message())
	}
	return nil
}

// discardBlockedBacktest deletes the backtest that the CREATING step stored,
// so retrying the same create with the other provider is not refused as a
// duplicate name. A block leaves nothing that belongs to the backtest: bars
// are stored per provider and symbol, not per backtest. Per-symbol
// STATUS_ERROR also removes the backtest (see discardSymbolDownloadError).
func discardBlockedBacktest(ctx context.Context, backtests repository.Backtest, name, id string) {
	if err := deleteStoredBacktest(ctx, backtests, name, id); err != nil {
		log.Warn().Err(err).Str("backtest", name).Msg("error deleting blocked backtest")
	}
}

// discardFailedBacktest deletes the backtest after a stream or connection
// error from the finance service. We can't tell which bars were stored, so
// the backtest record, its periods and its assets are removed, and a retry
// under the same name works. Bars are stored per provider and symbol, not per
// backtest, so they are kept. The returned error is the step's whole message.
func discardFailedBacktest(ctx context.Context, backtests repository.Backtest, name, id string, cause error) error {
	reason := cause.Error()
	if st, ok := status.FromError(cause); ok && st.Message() != "" {
		reason = st.Message()
	}
	reason = strings.TrimRight(strings.TrimSpace(reason), ".")
	if err := deleteStoredBacktest(ctx, backtests, name, id); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		log.Warn().Err(err).Str("backtest", name).Msg("error deleting backtest after failed download")
		return plainSentence(fmt.Sprintf("Downloading prices failed: %s. The backtest %s could not be removed; "+
			"delete it with `fbull backtest delete %s` before you retry.", reason, name, name))
	}
	if reason != "" {
		runes := []rune(reason)
		runes[0] = unicode.ToLower(runes[0])
		reason = string(runes)
	}
	return plainSentence(fmt.Sprintf("Downloading prices failed: %s. Nothing was changed. Run the same create again.", reason))
}

func Ingest(ctx context.Context, msg stream.Message) error {
	command := ss.IngestCommand{}
	err := msg.ParsePayload(&command)
	if err != nil {
		return fmt.Errorf("error unmarshalling Ingest payload: %w", err)
	}

	db := msg.MustGet(stream.DBDep).(*pgxpool.Pool)
	backtests := repository.Backtest{Conn: db}
	b, backtestID, err := loadStoredBacktest(ctx, db, command.BacktestName, command.OwnerID, command.BacktestID)
	if err != nil {
		return fmt.Errorf("error getting backtest: %w", err)
	}

	store, isStorage := msg.MustGet(stream.StorageDep).(storage.Storage)
	if !isStorage {
		return errors.New("error casting storage")
	}

	metadata := map[string]string{}
	// The two setup failures below are wrapped with %v, not %w, so only the
	// data service's own gRPC status can select the broken-install copy.
	objectKey := b.Name
	if environment.GetAuthEnabled() {
		objectKey = backtestID
	}
	object, err := store.CreateObject(ctx, storage.IngestionsBucket, objectKey, storage.WithMetadata(metadata))
	if err != nil {
		return discardFailedIngest(ctx, backtests, b.Name, backtestID, fmt.Errorf("error creating ingestion: %v", err))
	}

	ze, err := msg.Call(ctx, dependency.GetEngineKey)
	if err != nil {
		return discardFailedIngest(ctx, backtests, b.Name, backtestID, fmt.Errorf("error getting zipline engine: %v", err))
	}

	engine, isEngine := ze.(engine.Engine)
	if !isEngine {
		return errors.New("error casting zipline engine")
	}

	notices, err := engine.Ingest(ctx, b, object)
	if err != nil {
		if refused := ingestMissingSessions(ctx, engine, db, backtests, b, backtestID, err); refused != nil {
			return refused
		}
		return discardFailedIngest(ctx, backtests, b.Name, backtestID, err)
	}
	if len(notices) > 0 {
		// The backtest is kept. The notice rides on the completed INGESTING
		// step, so the CLI prints it.
		return &stream.Notice{Lines: notices}
	}
	return nil
}
