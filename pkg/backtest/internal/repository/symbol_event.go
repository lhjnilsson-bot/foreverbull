package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/lhjnilsson/foreverbull/internal/postgres"
)

// Symbol events are the listing and delisting evidence create stored.
// A run reads them; bars ending on their own are not an event.
const SymbolEventTable = `
CREATE TABLE IF NOT EXISTS backtest_symbol_event (
	backtest_name text NOT NULL REFERENCES backtest (name) ON DELETE CASCADE,
	symbol text NOT NULL,
	kind text NOT NULL,
	on_date date NOT NULL,
	evidence text NOT NULL,
	PRIMARY KEY (backtest_name, symbol, kind),
	CONSTRAINT backtest_symbol_event_kind_check CHECK (kind IN ('listed', 'delisted')),
	CONSTRAINT backtest_symbol_event_symbol_check CHECK (char_length(symbol) > 0),
	CONSTRAINT backtest_symbol_event_evidence_check CHECK (char_length(evidence) > 0)
);
`

const (
	// EventListed is a symbol whose first in-period bar is a confirmed listing.
	EventListed = "listed"
	// EventDelisted is a symbol whose last bar is a confirmed delisting.
	EventDelisted = "delisted"
)

// SymbolEvent is one confirmed listing or delisting on a backtest.
// On is the first bar for a listing and the last bar for a delisting.
type SymbolEvent struct {
	Symbol   string
	Kind     string
	On       time.Time
	Evidence string
}

func createSymbolEvents(ctx context.Context, conn postgres.Query) error {
	if _, err := conn.Exec(ctx, SymbolEventTable); err != nil {
		return fmt.Errorf("failed to create table backtest_symbol_event: %w", err)
	}
	return nil
}

// SaveSymbolEvents replaces the backtest's listing and delisting rows.
// An empty list deletes any rows already stored.
func (db *Backtest) SaveSymbolEvents(ctx context.Context, name string, events []SymbolEvent) error {
	if _, err := db.Conn.Exec(ctx, `DELETE FROM backtest_symbol_event WHERE backtest_name = $1`, name); err != nil {
		return fmt.Errorf("failed to clear symbol events: %w", err)
	}
	for _, event := range events {
		if event.Kind != EventListed && event.Kind != EventDelisted {
			return fmt.Errorf("symbol event kind %q", event.Kind)
		}
		if event.Symbol == "" || event.Evidence == "" || event.On.IsZero() {
			return fmt.Errorf("symbol event for %s is incomplete", event.Symbol)
		}
		if _, err := db.Conn.Exec(ctx, `
			INSERT INTO backtest_symbol_event (backtest_name, symbol, kind, on_date, evidence)
			VALUES ($1, $2, $3, $4, $5)`,
			name, event.Symbol, event.Kind, event.On.UTC().Format(time.DateOnly), event.Evidence); err != nil {
			return fmt.Errorf("failed to store symbol event for %s: %w", event.Symbol, err)
		}
	}
	return nil
}

// SymbolEvents returns the backtest's confirmed listings and delistings,
// oldest date first, then symbol.
func (db *Backtest) SymbolEvents(ctx context.Context, name string) ([]SymbolEvent, error) {
	rows, err := db.Conn.Query(ctx, `
		SELECT symbol, kind, on_date, evidence
		  FROM backtest_symbol_event
		 WHERE backtest_name = $1
		 ORDER BY on_date, symbol, kind`, name)
	if err != nil {
		return nil, fmt.Errorf("failed to read symbol events: %w", err)
	}
	defer rows.Close()
	events := make([]SymbolEvent, 0)
	for rows.Next() {
		var event SymbolEvent
		if err := rows.Scan(&event.Symbol, &event.Kind, &event.On, &event.Evidence); err != nil {
			return nil, fmt.Errorf("failed to scan symbol event: %w", err)
		}
		event.On = time.Date(event.On.Year(), event.On.Month(), event.On.Day(), 0, 0, 0, 0, time.UTC)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read symbol events: %w", err)
	}
	return events, nil
}
