package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/lhjnilsson/foreverbull/internal/postgres"
	"github.com/lhjnilsson/foreverbull/pkg/pb/backtest"
)

// The resolutions live on the backtest row rather than in a child table, so
// they follow the row through the AUTH_ENABLED move from the name to the id
// primary key without a foreign key of their own to repoint.
const addTickerResolutions = `
ALTER TABLE backtest
	ADD COLUMN IF NOT EXISTS ticker_resolutions jsonb NOT NULL DEFAULT '[]'::jsonb`

const addTickerResolutionsCheck = `
ALTER TABLE backtest
	ADD CONSTRAINT backtest_ticker_resolutions_check
	CHECK (jsonb_typeof(ticker_resolutions) = 'array')`

// MaxTickerResolutions bounds the list one backtest stores and reads. Every
// resolution is a downloaded symbol, so it can't exceed the download cap.
const MaxTickerResolutions = 10000

// TickerResolution is a symbol whose bars were read under a later ticker of
// the same ISIN. It names no day: the provider doesn't say when the ticker
// changed, and a day the download wrote is bookkeeping, not that day.
type TickerResolution struct {
	Symbol string
	ReadAs string
}

// storedResolution is the JSON shape of one element of
// backtest.ticker_resolutions.
type storedResolution struct {
	Symbol string `json:"symbol"`
	ReadAs string `json:"read_as"`
}

// ErrInvalidTickerResolution is a resolution that names no symbol, no later
// ticker, the same ticker twice, a field it doesn't have, or a symbol
// already listed.
var ErrInvalidTickerResolution = errors.New("invalid ticker resolution")

// MigrateTickerResolutions adds backtest.ticker_resolutions and its array
// check. Calling it twice is safe.
func MigrateTickerResolutions(ctx context.Context, conn postgres.Query) error {
	if _, err := conn.Exec(ctx, addTickerResolutions); err != nil {
		return fmt.Errorf("backtest: ticker_resolutions: %w", err)
	}
	exists, err := tableConstraintExists(ctx, conn, "backtest", "backtest_ticker_resolutions_check")
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	if _, err := conn.Exec(ctx, addTickerResolutionsCheck); err != nil {
		return fmt.Errorf("backtest: ticker_resolutions check: %w", err)
	}
	return nil
}

// SaveTickerResolutions replaces the resolutions of the backtest called
// name. An empty list clears them. A backtest that isn't stored is
// pgx.ErrNoRows, so a resolution is never dropped without an error.
func (db *Backtest) SaveTickerResolutions(ctx context.Context, name string, resolutions []TickerResolution) error {
	if authOn() {
		return fmt.Errorf("save ticker resolutions by name is refused while AUTH_ENABLED=true")
	}
	return db.saveTickerResolutions(ctx, `UPDATE backtest SET ticker_resolutions = $2 WHERE name = $1`, name, resolutions)
}

// SaveTickerResolutionsByID is SaveTickerResolutions for one backtest id.
func (db *Backtest) SaveTickerResolutionsByID(ctx context.Context, id string, resolutions []TickerResolution) error {
	if id == "" {
		return fmt.Errorf("backtest id is empty")
	}
	return db.saveTickerResolutions(ctx, `UPDATE backtest SET ticker_resolutions = $2 WHERE id = $1`, id, resolutions)
}

func (db *Backtest) saveTickerResolutions(ctx context.Context, query, key string, resolutions []TickerResolution) error {
	encoded, err := encodeTickerResolutions(resolutions)
	if err != nil {
		return err
	}
	tag, err := db.Conn.Exec(ctx, query, key, encoded)
	if err != nil {
		return fmt.Errorf("failed to store ticker resolutions: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// encodeTickerResolutions checks every resolution and returns them as the
// stored JSON array, ordered by symbol.
func encodeTickerResolutions(resolutions []TickerResolution) ([]byte, error) {
	if len(resolutions) > MaxTickerResolutions {
		return nil, fmt.Errorf("%d ticker resolutions, at most %d: %w",
			len(resolutions), MaxTickerResolutions, ErrInvalidTickerResolution)
	}
	stored := make([]storedResolution, 0, len(resolutions))
	for _, resolution := range resolutions {
		if err := checkTickerResolution(resolution); err != nil {
			return nil, err
		}
		stored = append(stored, storedResolution(resolution))
	}
	if err := sortDistinct(stored); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(stored)
	if err != nil {
		return nil, fmt.Errorf("failed to encode ticker resolutions: %w", err)
	}
	return encoded, nil
}

func checkTickerResolution(resolution TickerResolution) error {
	if resolution.Symbol == "" || resolution.Symbol != strings.TrimSpace(resolution.Symbol) {
		return fmt.Errorf("symbol %q: %w", resolution.Symbol, ErrInvalidTickerResolution)
	}
	if resolution.ReadAs == "" || resolution.ReadAs != strings.TrimSpace(resolution.ReadAs) {
		return fmt.Errorf("%s read as %q: %w", resolution.Symbol, resolution.ReadAs, ErrInvalidTickerResolution)
	}
	if resolution.ReadAs == resolution.Symbol {
		return fmt.Errorf("%s read as itself: %w", resolution.Symbol, ErrInvalidTickerResolution)
	}
	return nil
}

// decodeTickerResolutions reads backtest.ticker_resolutions back into the
// protobuf list and checks each element again, so a row edited outside
// create fails the read instead of showing a wrong line. A field the
// resolution doesn't have, such as a date, fails it too.
func decodeTickerResolutions(raw []byte) ([]*backtest.Backtest_TickerResolution, error) {
	var stored []storedResolution
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&stored); err != nil {
		return nil, fmt.Errorf("failed to decode ticker resolutions: %w: %w", err, ErrInvalidTickerResolution)
	}
	if len(stored) > MaxTickerResolutions {
		return nil, fmt.Errorf("%d stored ticker resolutions, at most %d: %w",
			len(stored), MaxTickerResolutions, ErrInvalidTickerResolution)
	}
	for _, item := range stored {
		if err := checkTickerResolution(TickerResolution(item)); err != nil {
			return nil, err
		}
	}
	if err := sortDistinct(stored); err != nil {
		return nil, err
	}
	out := make([]*backtest.Backtest_TickerResolution, 0, len(stored))
	for _, item := range stored {
		out = append(out, &backtest.Backtest_TickerResolution{Symbol: item.Symbol, ReadAs: item.ReadAs})
	}
	return out, nil
}

// sortDistinct orders the resolutions by symbol, the order they are stored
// and shown in, and refuses a symbol listed twice. Encode and decode share
// it, so a row edited outside create can't list one symbol twice either.
func sortDistinct(stored []storedResolution) error {
	sort.Slice(stored, func(i, j int) bool { return stored[i].Symbol < stored[j].Symbol })
	for i := 1; i < len(stored); i++ {
		if stored[i].Symbol == stored[i-1].Symbol {
			return fmt.Errorf("%s is listed twice: %w", stored[i].Symbol, ErrInvalidTickerResolution)
		}
	}
	return nil
}
