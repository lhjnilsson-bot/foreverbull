package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lhjnilsson/foreverbull/internal/postgres"
)

// InstrumentTable and AssetSymbolTable match pkg/backtest/internal/repository/instrument.go.
const InstrumentTable = `
CREATE TABLE IF NOT EXISTS instrument (
	id text PRIMARY KEY,
	isin text UNIQUE,
	name text
);
`

const assetSymbolSymbolIndex = `CREATE INDEX IF NOT EXISTS asset_symbol_symbol_from_date_idx ON asset_symbol (symbol, from_date);
`

const AssetSymbolTable = `
CREATE TABLE IF NOT EXISTS asset_symbol (
	isin text NOT NULL,
	symbol text NOT NULL,
	from_date date NOT NULL,
	to_date date,
	PRIMARY KEY (isin, from_date),
	CONSTRAINT asset_symbol_isin_fkey FOREIGN KEY (isin) REFERENCES instrument (isin),
	CONSTRAINT asset_symbol_no_overlap EXCLUDE USING gist (
		isin WITH =,
		daterange(from_date, to_date, '[)') WITH &&
	)
);

` + assetSymbolSymbolIndex

const addAssetSymbolForeignKey = `
ALTER TABLE asset_symbol
	ADD CONSTRAINT asset_symbol_isin_fkey
	FOREIGN KEY (isin) REFERENCES instrument (isin)`

const addAssetSymbolExclusion = `
ALTER TABLE asset_symbol
	ADD CONSTRAINT asset_symbol_no_overlap
	EXCLUDE USING gist (
		isin WITH =,
		daterange(from_date, to_date, '[)') WITH &&
	)`

// MigrateAssetSymbol adds the instrument foreign key, the per-ISIN
// half-open exclusion, and the (symbol, from_date) index.
// Rows that would violate the new constraint stay. Calling it twice is safe.
func MigrateAssetSymbol(ctx context.Context, conn postgres.Query) error {
	if _, err := conn.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS btree_gist`); err != nil {
		return fmt.Errorf("failed to create extension btree_gist: %w", err)
	}
	if err := migrateAssetSymbolForeignKey(ctx, conn); err != nil {
		return err
	}
	if err := migrateAssetSymbolExclusion(ctx, conn); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, assetSymbolSymbolIndex); err != nil {
		return fmt.Errorf("asset_symbol: symbol index: %w", err)
	}
	return nil
}

func migrateAssetSymbolForeignKey(ctx context.Context, conn postgres.Query) error {
	exists, err := tableConstraintExists(ctx, conn, "asset_symbol", "asset_symbol_isin_fkey")
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	var bad bool
	err = conn.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM asset_symbol s
			LEFT JOIN instrument i ON i.isin = s.isin
			WHERE i.isin IS NULL
		)`).Scan(&bad)
	if err != nil {
		return fmt.Errorf("asset_symbol: scan foreign key: %w", err)
	}
	if bad {
		return fmt.Errorf("asset_symbol: rows violate foreign key isin references instrument(isin)")
	}

	if _, err = conn.Exec(ctx, addAssetSymbolForeignKey); err != nil {
		return fmt.Errorf("asset_symbol: foreign key: %w", err)
	}
	return nil
}

func migrateAssetSymbolExclusion(ctx context.Context, conn postgres.Query) error {
	exists, err := tableConstraintExists(ctx, conn, "asset_symbol", "asset_symbol_no_overlap")
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	var bad bool
	err = conn.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM asset_symbol a
			JOIN asset_symbol b
			  ON a.isin = b.isin
			 AND a.from_date < b.from_date
			 AND daterange(a.from_date, a.to_date, '[)') && daterange(b.from_date, b.to_date, '[)')
		)`).Scan(&bad)
	if err != nil {
		return fmt.Errorf("asset_symbol: scan overlap: %w", err)
	}
	if bad {
		return fmt.Errorf("asset_symbol: rows violate exclusion on isin and daterange(from_date, to_date, '[)')")
	}

	if _, err = conn.Exec(ctx, addAssetSymbolExclusion); err != nil {
		return fmt.Errorf("asset_symbol: exclusion: %w", err)
	}
	return nil
}

func tableConstraintExists(ctx context.Context, conn postgres.Query, table, name string) (bool, error) {
	var exists bool
	err := conn.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM pg_constraint c
			JOIN pg_class t ON t.oid = c.conrelid
			JOIN pg_namespace n ON n.oid = t.relnamespace
			WHERE n.nspname = current_schema()
			  AND t.relname = $1
			  AND c.conname = $2
		)`, table, name).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("%s: read constraint %s: %w", table, name, err)
	}
	return exists, nil
}

// ErrMissingISIN is returned when identity cannot be stored without an ISIN.
// Holdings rows with an empty ISIN are skipped; ticker and name are not identity.
var ErrMissingISIN = errors.New("missing ISIN")

// ErrAmbiguousSymbol is returned by Resolve when a ticker maps to more than
// one instrument on the asked day. Guessing one would store or serve another
// company's prices, so callers refuse it.
var ErrAmbiguousSymbol = errors.New("symbol maps to more than one instrument")

// Instrument is a stable economic asset. ID equals ISIN when iShares supplies one.
type Instrument struct {
	ID   string
	ISIN string
	Name string
}

// Instruments stores ISIN identity and dated ticker aliases.
type Instruments struct {
	Conn postgres.Query
}

func dateOnly(t time.Time) time.Time {
	year, month, day := t.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

func normalizeISIN(isin string) string {
	return strings.TrimSpace(isin)
}

func (db *Instruments) Store(ctx context.Context, isin, name string) (*Instrument, error) {
	isin = normalizeISIN(isin)
	if isin == "" {
		return nil, ErrMissingISIN
	}

	_, err := db.Conn.Exec(ctx,
		`INSERT INTO instrument(id, isin, name) VALUES($1, $1, $2)
		ON CONFLICT (id) DO UPDATE SET name = COALESCE(NULLIF(EXCLUDED.name, ''), instrument.name)`,
		isin, name)
	if err != nil {
		return nil, fmt.Errorf("failed to store instrument: %w", err)
	}

	return db.Get(ctx, isin)
}

func (db *Instruments) Get(ctx context.Context, isin string) (*Instrument, error) {
	isin = normalizeISIN(isin)
	if isin == "" {
		return nil, ErrMissingISIN
	}

	inst := Instrument{}
	err := db.Conn.QueryRow(ctx,
		`SELECT id, isin, name FROM instrument WHERE isin=$1`, isin).
		Scan(&inst.ID, &inst.ISIN, &inst.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to get instrument: %w", err)
	}

	return &inst, nil
}

// Resolve maps a ticker at t to the ISIN-keyed instrument. Ticker is not identity.
func (db *Instruments) Resolve(ctx context.Context, symbol string, t time.Time) (*Instrument, error) {
	symbol = strings.TrimSpace(symbol)
	if symbol == "" {
		return nil, fmt.Errorf("missing symbol")
	}

	t = dateOnly(t)
	tStr := t.Format("2006-01-02")

	rows, err := db.Conn.Query(ctx, `
		SELECT i.id, i.isin, i.name
		  FROM asset_symbol s
		  JOIN instrument i ON i.isin = s.isin
		 WHERE s.symbol = $1
		   AND s.from_date <= $2::date
		   AND (s.to_date IS NULL OR $2::date < s.to_date)`,
		symbol, tStr)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve symbol: %w", err)
	}
	defer rows.Close()

	var found []Instrument
	for rows.Next() {
		var inst Instrument
		if err := rows.Scan(&inst.ID, &inst.ISIN, &inst.Name); err != nil {
			return nil, fmt.Errorf("failed to scan instrument: %w", err)
		}
		found = append(found, inst)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to resolve symbol: %w", err)
	}

	if len(found) == 0 {
		return nil, fmt.Errorf("failed to resolve symbol: %w", pgx.ErrNoRows)
	}
	if len(found) > 1 {
		return nil, fmt.Errorf("symbol %s maps to %d instruments at %s: %w",
			symbol, len(found), t.Format("2006-01-02"), ErrAmbiguousSymbol)
	}

	inst := found[0]
	return &inst, nil
}

// Symbols returns the ticker of instrument at t. Adds/drops are membership, not identity.
func (db *Instruments) Symbols(ctx context.Context, inst *Instrument, t time.Time) (string, error) {
	if inst == nil {
		return "", fmt.Errorf("missing instrument")
	}

	isin := normalizeISIN(inst.ISIN)
	if isin == "" {
		isin = normalizeISIN(inst.ID)
	}
	if isin == "" {
		return "", ErrMissingISIN
	}

	t = dateOnly(t)
	tStr := t.Format("2006-01-02")

	var symbol string
	err := db.Conn.QueryRow(ctx, `
		SELECT symbol FROM asset_symbol
		 WHERE isin = $1
		   AND from_date <= $2::date
		   AND (to_date IS NULL OR $2::date < to_date)`,
		isin, tStr).Scan(&symbol)
	if err != nil {
		return "", fmt.Errorf("failed to get symbol: %w", err)
	}

	return symbol, nil
}

// AliasInterval is one dated ticker of an ISIN in asset_symbol ([from, to)).
type AliasInterval struct {
	ISIN     string
	Symbol   string
	FromDate time.Time
	ToDate   *time.Time
}

// ListAliases returns ticker intervals for isin, ordered by from_date.
func (db *Instruments) ListAliases(ctx context.Context, isin string) ([]AliasInterval, error) {
	isin = normalizeISIN(isin)
	if isin == "" {
		return nil, ErrMissingISIN
	}

	rows, err := db.Conn.Query(ctx, `
		SELECT isin, symbol, from_date, to_date
		  FROM asset_symbol
		 WHERE isin = $1
		 ORDER BY from_date`, isin)
	if err != nil {
		return nil, fmt.Errorf("failed to list asset_symbol: %w", err)
	}
	defer rows.Close()

	aliases := make([]AliasInterval, 0)
	for rows.Next() {
		var a AliasInterval
		var to *time.Time
		if err := rows.Scan(&a.ISIN, &a.Symbol, &a.FromDate, &to); err != nil {
			return nil, fmt.Errorf("failed to scan asset_symbol: %w", err)
		}
		a.FromDate = dateOnly(a.FromDate)
		if to != nil {
			t := dateOnly(*to)
			a.ToDate = &t
		}
		aliases = append(aliases, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to list asset_symbol: %w", err)
	}
	return aliases, nil
}

// EnsureLaterAlias records later as a ticker of isin that starts on laterFrom,
// closing an open current alias at that day when needed. When later is already
// stored for the ISIN, it is a no-op. Both writes run in one transaction, so a
// failed insert doesn't leave the current alias closed with no later one.
//
// A later ticker the table still doesn't hold after the write is
// ErrLaterAliasNotStored and rolls the transaction back, so a caller never
// reports a resolution that wasn't stored.
func (db *Instruments) EnsureLaterAlias(ctx context.Context, isin, current, later string, laterFrom time.Time) error {
	conn, ok := db.Conn.(txBeginner)
	if !ok {
		return errInstrumentsNoTransaction
	}
	return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		inTx := Instruments{Conn: tx}
		if err := inTx.ensureLaterAlias(ctx, isin, current, later, laterFrom); err != nil {
			return err
		}
		return inTx.requireAlias(ctx, isin, later)
	})
}

// ErrLaterAliasNotStored is returned when the later ticker is not in
// asset_symbol after EnsureLaterAlias wrote it, such as when another ticker
// of the ISIN already starts on that day.
var ErrLaterAliasNotStored = errors.New("later ticker is not stored in asset_symbol")

// requireAlias is ErrLaterAliasNotStored when isin has no alias for symbol.
func (db *Instruments) requireAlias(ctx context.Context, isin, symbol string) error {
	symbol = strings.TrimSpace(symbol)
	aliases, err := db.ListAliases(ctx, isin)
	if err != nil {
		return err
	}
	for _, a := range aliases {
		if a.Symbol == symbol {
			return nil
		}
	}
	return fmt.Errorf("%s for %s: %w", symbol, normalizeISIN(isin), ErrLaterAliasNotStored)
}

// errInstrumentsNoTransaction is returned when the connection can't open a
// transaction; nothing is written.
var errInstrumentsNoTransaction = errors.New("instruments: connection can't open a transaction")

func (db *Instruments) ensureLaterAlias(ctx context.Context, isin, current, later string, laterFrom time.Time) error {
	isin = normalizeISIN(isin)
	current = strings.TrimSpace(current)
	later = strings.TrimSpace(later)
	if isin == "" {
		return ErrMissingISIN
	}
	if later == "" {
		return fmt.Errorf("missing later symbol for isin %s", isin)
	}
	laterFrom = dateOnly(laterFrom)

	aliases, err := db.ListAliases(ctx, isin)
	if err != nil {
		return err
	}
	for _, a := range aliases {
		if a.Symbol == later {
			return nil
		}
	}

	// Close an open current alias that still covers laterFrom.
	if current != "" {
		_, err = db.Conn.Exec(ctx, `
			UPDATE asset_symbol
			   SET to_date = $3::date
			 WHERE isin = $1
			   AND symbol = $2
			   AND to_date IS NULL
			   AND from_date < $3::date`,
			isin, current, laterFrom.Format("2006-01-02"))
		if err != nil {
			return fmt.Errorf("failed to close asset_symbol: %w", err)
		}
	}

	_, err = db.Conn.Exec(ctx, `
		INSERT INTO asset_symbol (isin, symbol, from_date, to_date)
		VALUES ($1, $2, $3::date, NULL)
		ON CONFLICT (isin, from_date) DO NOTHING`,
		isin, later, laterFrom.Format("2006-01-02"))
	if err != nil {
		return fmt.Errorf("failed to store later asset_symbol: %w", err)
	}
	return nil
}
