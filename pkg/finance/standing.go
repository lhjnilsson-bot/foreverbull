package finance

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"

	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/pkg/finance/internal/suppliers/marketdata"
	"github.com/lhjnilsson/foreverbull/pkg/finance/internal/suppliers/trading"
)

// SymbolFacts is what a provider can say about one symbol. Gone is true only
// when the provider says the symbol is gone and no later ticker was found.
// LookbackEmpty is true only when a lookback window was fetched and returned
// no bar. FirstTrade is set only when the provider named a listing date.
type SymbolFacts struct {
	Gone          bool
	LaterTicker   bool
	FirstTrade    time.Time
	LookbackEmpty bool
	Evidence      string
}

const (
	// EvidenceAlpacaInactive is the stored evidence when Alpaca reports the asset inactive.
	EvidenceAlpacaInactive = "alpaca asset status inactive"
	// EvidenceYahooGone is the stored evidence when Yahoo lists neither the symbol nor a later ticker.
	EvidenceYahooGone = "yahoo search returned no listing and no later ticker"
)

// EvidenceYahooListed is the stored evidence for a Yahoo firstTradeDate.
func EvidenceYahooListed(day time.Time) string {
	return "yahoo firstTradeDate " + day.UTC().Format(time.DateOnly)
}

// FirstTradeDateFromChart reads meta.firstTradeDate from a recorded Yahoo chart body.
func FirstTradeDateFromChart(symbol string, body []byte) (time.Time, error) {
	return marketdata.FirstTradeDateFromChart(symbol, body)
}

// InspectSymbol asks the provider whether symbol is gone, and, when from and
// to are set, whether that window has bars and what Yahoo's first trade date
// is. Stooq has no listing source: the facts are empty and the error is nil,
// which confirms nothing. A failed lookup is an error and is not "gone".
func InspectSymbol(ctx context.Context, db *pgxpool.Pool, provider, profile, symbol string, from, to *time.Time) (SymbolFacts, error) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "stooq":
		return SymbolFacts{}, nil
	case "alpaca":
		return inspectAlpaca(ctx, db, profile, symbol)
	case "", "yahoo":
		return inspectYahoo(ctx, db, symbol, from, to)
	default:
		return SymbolFacts{}, fmt.Errorf("unknown price provider %q", provider)
	}
}

func inspectAlpaca(ctx context.Context, db *pgxpool.Pool, profile, symbol string) (SymbolFacts, error) {
	later, err := laterTicker(ctx, db, symbol)
	if err != nil {
		return SymbolFacts{}, err
	}
	if later {
		return SymbolFacts{LaterTicker: true}, nil
	}
	key, secret, err := trading.AlpacaPriceCredentials(environment.GetTradingProfilesFile(), profile)
	if err != nil {
		return SymbolFacts{}, err
	}
	client, err := marketdata.NewAlpacaBars(key, secret)
	if err != nil {
		return SymbolFacts{}, err
	}
	status, err := client.AssetStatus(ctx, symbol)
	if err != nil {
		return SymbolFacts{}, noteAlpacaKey(symbol, asLookup(ctx, "alpaca", symbol, err))
	}
	if status != "inactive" {
		return SymbolFacts{}, nil
	}
	return SymbolFacts{Gone: true, Evidence: EvidenceAlpacaInactive}, nil
}

func inspectYahoo(ctx context.Context, db *pgxpool.Pool, symbol string, from, to *time.Time) (SymbolFacts, error) {
	client, err := sharedYahoo()
	if err != nil {
		return SymbolFacts{}, asLookup(ctx, "yahoo", symbol, err)
	}
	facts, err := yahooGone(ctx, client, db, symbol)
	if err != nil {
		return SymbolFacts{}, err
	}
	if from == nil || to == nil {
		return facts, nil
	}
	when, err := client.FirstTradeDate(ctx, symbol)
	if err != nil {
		return SymbolFacts{}, asLookup(ctx, "yahoo", symbol, err)
	}
	empty, err := lookbackEmpty(ctx, client, symbol, *from, to)
	if err != nil {
		return SymbolFacts{}, asLookup(ctx, "yahoo", symbol, err)
	}
	facts.FirstTrade = when
	facts.LookbackEmpty = empty
	return facts, nil
}

func yahooGone(ctx context.Context, client *marketdata.YahooClient, db *pgxpool.Pool, symbol string) (SymbolFacts, error) {
	quotes, err := client.SearchSymbols(ctx, symbol)
	if err != nil {
		return SymbolFacts{}, asLookup(ctx, "yahoo", symbol, err)
	}
	later, err := laterTicker(ctx, db, symbol)
	if err != nil {
		return SymbolFacts{}, err
	}
	isin, err := storedISIN(ctx, db, symbol)
	if err != nil {
		return SymbolFacts{}, err
	}
	if isin != "" {
		found, err := client.SearchSymbols(ctx, isin)
		if err != nil {
			return SymbolFacts{}, asLookup(ctx, "yahoo", symbol, err)
		}
		quotes = append(quotes, found...)
	}
	gone, alias := listingGone(quotes, symbol, later)
	if alias {
		return SymbolFacts{LaterTicker: true}, nil
	}
	if !gone {
		return SymbolFacts{}, nil
	}
	return SymbolFacts{Gone: true, Evidence: EvidenceYahooGone}, nil
}

// listingGone reports a positive "this symbol is gone". A quote that is not
// the asked ticker is a later alias. The asked ticker itself is still listed.
// An empty quote list with no alias is gone.
func listingGone(quotes []string, asked string, laterInDB bool) (gone, later bool) {
	if laterInDB {
		return false, true
	}
	listed := false
	for _, quote := range quotes {
		if sameTicker(quote, asked) {
			listed = true
			continue
		}
		return false, true
	}
	if listed {
		return false, false
	}
	return true, false
}

func sameTicker(a, b string) bool {
	return strings.EqualFold(marketdata.StoredSymbol(a), marketdata.StoredSymbol(b))
}

func lookbackEmpty(ctx context.Context, client *marketdata.YahooClient, symbol string, from time.Time, to *time.Time) (bool, error) {
	bars, err := client.GetOHLC(ctx, symbol, from, to)
	if err != nil {
		if errors.Is(err, marketdata.ErrNoBars) {
			return true, nil
		}
		return false, err
	}
	return len(bars) == 0, nil
}

func laterTicker(ctx context.Context, db *pgxpool.Pool, symbol string) (bool, error) {
	if db == nil {
		return false, errors.New("no database to read ticker aliases")
	}
	var later bool
	err := db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			  FROM asset_symbol asked
			  JOIN asset_symbol other
			    ON other.isin = asked.isin
			   AND other.symbol <> asked.symbol
			 WHERE asked.symbol = $1
		)`, symbol).Scan(&later)
	if err != nil {
		return false, fmt.Errorf("read later ticker for %s: %w", symbol, err)
	}
	return later, nil
}

func storedISIN(ctx context.Context, db *pgxpool.Pool, symbol string) (string, error) {
	if db == nil {
		return "", errors.New("no database to read ticker aliases")
	}
	var isin string
	var n int
	err := db.QueryRow(ctx, `
		SELECT COALESCE(min(isin), ''), count(DISTINCT isin)
		  FROM asset_symbol
		 WHERE symbol = $1`, symbol).Scan(&isin, &n)
	if err != nil {
		return "", fmt.Errorf("read ISIN for %s: %w", symbol, err)
	}
	if n > 1 {
		return "", fmt.Errorf("%s belongs to more than one instrument", symbol)
	}
	if n == 0 {
		return "", nil
	}
	return isin, nil
}

var (
	yahooMu     sync.Mutex
	yahooShared *marketdata.YahooClient
)

// newYahooClient builds the shared Yahoo client. Tests replace it.
var newYahooClient = marketdata.NewYahooClient

// sharedYahoo returns the process's Yahoo client. A client that was built is
// reused. A failure is not: the next call tries again.
func sharedYahoo() (*marketdata.YahooClient, error) {
	yahooMu.Lock()
	defer yahooMu.Unlock()
	if yahooShared != nil {
		return yahooShared, nil
	}
	client, err := newYahooClient()
	if err != nil {
		return nil, err
	}
	yahoo, ok := client.(*marketdata.YahooClient)
	if !ok {
		return nil, errors.New("yahoo client has the wrong type")
	}
	yahooShared = yahoo
	return yahooShared, nil
}

// noteAlpacaKey logs that a 401 or 403 came from the paper host. A live-only
// key can be refused there. The user sentence names the paper API, not the
// live-only key.
func noteAlpacaKey(symbol string, err error) error {
	var lookup *LookupError
	if errors.As(err, &lookup) && (lookup.Status == http.StatusUnauthorized || lookup.Status == http.StatusForbidden) {
		log.Warn().Str("symbol", symbol).Str("host", marketdata.AlpacaPaperHost).
			Msg("alpaca asset status uses the paper API; a live-only key may be refused")
	}
	return err
}
