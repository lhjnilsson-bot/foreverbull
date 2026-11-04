package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lhjnilsson/foreverbull/internal/auth"
)

func Recreate(ctx context.Context, conn *pgxpool.Pool) error {
	if _, err := conn.Exec(ctx, `DROP TABLE IF EXISTS trading_calendar_date`); err != nil {
		return fmt.Errorf("failed to drop trading_calendar_date table: %w", err)
	}

	if _, err := conn.Exec(ctx, `DROP TABLE IF EXISTS corporate_action`); err != nil {
		return fmt.Errorf("failed to drop corporate_action table: %w", err)
	}

	if _, err := conn.Exec(ctx, `DROP TABLE IF EXISTS dividend_fetch`); err != nil {
		return fmt.Errorf("failed to drop dividend_fetch table: %w", err)
	}

	if _, err := conn.Exec(ctx, `DROP TABLE IF EXISTS dividend`); err != nil {
		return fmt.Errorf("failed to drop dividend table: %w", err)
	}

	if _, err := conn.Exec(ctx, `DROP TABLE IF EXISTS ohlc`); err != nil {
		return fmt.Errorf("failed to drop ohlc table: %w", err)
	}

	if _, err := conn.Exec(ctx, `DROP TABLE IF EXISTS asset_symbol`); err != nil {
		return fmt.Errorf("failed to drop asset_symbol table: %w", err)
	}

	if _, err := conn.Exec(ctx, `DROP TABLE IF EXISTS instrument`); err != nil {
		return fmt.Errorf("failed to drop instrument table: %w", err)
	}

	if _, err := conn.Exec(ctx, `DROP TABLE IF EXISTS asset`); err != nil {
		return fmt.Errorf("failed to drop asset table: %w", err)
	}

	if _, err := conn.Exec(ctx, `DROP TABLE IF EXISTS trading_session`); err != nil {
		return fmt.Errorf("failed to drop trading_session table: %w", err)
	}

	if _, err := conn.Exec(context.Background(), `CREATE EXTENSION IF NOT EXISTS "uuid-ossp";`); err != nil {
		return fmt.Errorf("failed to create extension uuid-ossp: %w", err)
	}

	if _, err := conn.Exec(context.Background(), `CREATE EXTENSION IF NOT EXISTS btree_gist`); err != nil {
		return fmt.Errorf("failed to create extension btree_gist: %w", err)
	}

	if _, err := conn.Exec(ctx, AssetTable); err != nil {
		return fmt.Errorf("failed to create asset table: %w", err)
	}

	if _, err := conn.Exec(ctx, InstrumentTable); err != nil {
		return fmt.Errorf("failed to create instrument table: %w", err)
	}

	if _, err := conn.Exec(ctx, AssetSymbolTable); err != nil {
		return fmt.Errorf("failed to create asset_symbol table: %w", err)
	}

	if _, err := conn.Exec(ctx, OHLCTable); err != nil {
		return fmt.Errorf("failed to create ohlc table: %w", err)
	}

	if err := migrateFinanceSchema(ctx, conn); err != nil {
		return err
	}

	if _, err := conn.Exec(ctx, DividendTable); err != nil {
		return fmt.Errorf("failed to create dividend table: %w", err)
	}

	if _, err := conn.Exec(ctx, TradingTable); err != nil {
		return fmt.Errorf("failed to create trading table: %w", err)
	}

	if _, err := conn.Exec(ctx, CalendarTable); err != nil {
		return fmt.Errorf("failed to create trading_calendar table: %w", err)
	}

	return auth.EnsureWorkerRole(ctx, conn)
}

func CreateTables(ctx context.Context, conn *pgxpool.Pool) error {
	if _, err := conn.Exec(context.Background(), `CREATE EXTENSION IF NOT EXISTS "uuid-ossp";`); err != nil {
		return fmt.Errorf("failed to create extension uuid-ossp: %w", err)
	}

	if _, err := conn.Exec(context.Background(), `CREATE EXTENSION IF NOT EXISTS btree_gist`); err != nil {
		return fmt.Errorf("failed to create extension btree_gist: %w", err)
	}

	if _, err := conn.Exec(ctx, AssetTable); err != nil {
		return fmt.Errorf("failed to create asset table: %w", err)
	}

	if _, err := conn.Exec(ctx, InstrumentTable); err != nil {
		return fmt.Errorf("failed to create instrument table: %w", err)
	}

	if _, err := conn.Exec(ctx, AssetSymbolTable); err != nil {
		return fmt.Errorf("failed to create asset_symbol table: %w", err)
	}

	if _, err := conn.Exec(ctx, OHLCTable); err != nil {
		return fmt.Errorf("failed to create ohlc table: %w", err)
	}

	if err := migrateFinanceSchema(ctx, conn); err != nil {
		return err
	}

	if _, err := conn.Exec(ctx, DividendTable); err != nil {
		return fmt.Errorf("failed to create dividend table: %w", err)
	}

	if _, err := conn.Exec(ctx, TradingTable); err != nil {
		return fmt.Errorf("failed to create trading table: %w", err)
	}

	if _, err := conn.Exec(ctx, CalendarTable); err != nil {
		return fmt.Errorf("failed to create trading_calendar table: %w", err)
	}

	return auth.EnsureWorkerRole(ctx, conn)
}

func migrateFinanceSchema(ctx context.Context, conn *pgxpool.Pool) error {
	if err := MigrateOHLC(ctx, conn); err != nil {
		return err
	}
	if err := MigrateOHLCBars(ctx, conn); err != nil {
		return err
	}
	return MigrateAssetSymbol(ctx, conn)
}
