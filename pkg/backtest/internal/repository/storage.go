package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

func Recreate(ctx context.Context, conn *pgxpool.Pool) error { //nolint: cyclop
	if _, err := conn.Exec(ctx, `DROP TABLE IF EXISTS backtest_symbol_event;`); err != nil {
		return fmt.Errorf("failed to drop table backtest_symbol_event: %w", err)
	}

	if _, err := conn.Exec(ctx, `DROP TABLE IF EXISTS universe_member;`); err != nil {
		return fmt.Errorf("failed to drop table universe_member: %w", err)
	}

	if _, err := conn.Exec(ctx, `DROP TABLE IF EXISTS asset_symbol;`); err != nil {
		return fmt.Errorf("failed to drop table asset_symbol: %w", err)
	}

	if _, err := conn.Exec(ctx, `DROP TABLE IF EXISTS instrument;`); err != nil {
		return fmt.Errorf("failed to drop table instrument: %w", err)
	}

	if _, err := conn.Exec(ctx, `DROP TABLE IF EXISTS backtest_execution_period;`); err != nil {
		return fmt.Errorf("failed to drop table backtest_execution_period: %w", err)
	}

	if _, err := conn.Exec(ctx, `DROP TABLE IF EXISTS backtest_execution;`); err != nil {
		return fmt.Errorf("failed to drop table execution: %w", err)
	}

	if _, err := conn.Exec(ctx, `DROP TABLE IF EXISTS session;`); err != nil {
		return fmt.Errorf("failed to drop table session: %w", err)
	}

	if _, err := conn.Exec(ctx, `DROP TABLE IF EXISTS backtest_period_asset;`); err != nil {
		return fmt.Errorf("failed to drop table backtest_period_asset: %w", err)
	}

	if _, err := conn.Exec(ctx, `DROP TABLE IF EXISTS backtest_period;`); err != nil {
		return fmt.Errorf("failed to drop table backtest_period: %w", err)
	}

	if _, err := conn.Exec(ctx, `DROP TABLE IF EXISTS backtest;`); err != nil {
		return fmt.Errorf("failed to drop table backtest: %w", err)
	}

	if _, err := conn.Exec(context.Background(), `CREATE EXTENSION IF NOT EXISTS "uuid-ossp";`); err != nil {
		return fmt.Errorf("failed to create extension uuid-ossp: %w", err)
	}

	if _, err := conn.Exec(context.Background(), `CREATE EXTENSION IF NOT EXISTS btree_gist`); err != nil {
		return fmt.Errorf("failed to create extension btree_gist: %w", err)
	}

	if _, err := conn.Exec(ctx, BacktestTable); err != nil {
		return fmt.Errorf("failed to create table backtest: %w", err)
	}

	if err := migrateBacktestPriceSource(ctx, conn); err != nil {
		return err
	}

	if err := MigrateBacktestProvider(ctx, conn); err != nil {
		return err
	}

	if err := createSymbolEvents(ctx, conn); err != nil {
		return err
	}

	if err := createIdentityTables(ctx, conn); err != nil {
		return err
	}

	return createRunTables(ctx, conn)
}

func migrateBacktestPriceSource(ctx context.Context, conn *pgxpool.Pool) error {
	if _, err := conn.Exec(ctx, `ALTER TABLE backtest ADD COLUMN IF NOT EXISTS provider text NOT NULL DEFAULT 'yahoo'`); err != nil {
		return fmt.Errorf("failed to add backtest.provider: %w", err)
	}
	if _, err := conn.Exec(ctx, `ALTER TABLE backtest ADD COLUMN IF NOT EXISTS profile text NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("failed to add backtest.profile: %w", err)
	}
	return nil
}

func createIdentityTables(ctx context.Context, conn *pgxpool.Pool) error {
	if _, err := conn.Exec(ctx, InstrumentTable); err != nil {
		return fmt.Errorf("failed to create table instrument: %w", err)
	}
	if _, err := conn.Exec(ctx, AssetSymbolTable); err != nil {
		return fmt.Errorf("failed to create table asset_symbol: %w", err)
	}
	if err := MigrateAssetSymbol(ctx, conn); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, UniverseMemberTable); err != nil {
		return fmt.Errorf("failed to create table universe_member: %w", err)
	}
	return MigrateUniverseMember(ctx, conn)
}

func CreateTables(ctx context.Context, conn *pgxpool.Pool) error {
	if _, err := conn.Exec(context.Background(), `CREATE EXTENSION IF NOT EXISTS "uuid-ossp";`); err != nil {
		return fmt.Errorf("failed to create extension uuid-ossp: %w", err)
	}

	if _, err := conn.Exec(context.Background(), `CREATE EXTENSION IF NOT EXISTS btree_gist`); err != nil {
		return fmt.Errorf("failed to create extension btree_gist: %w", err)
	}

	if _, err := conn.Exec(ctx, BacktestTable); err != nil {
		return fmt.Errorf("failed to create table backtest: %w", err)
	}

	if err := migrateBacktestPriceSource(ctx, conn); err != nil {
		return err
	}

	if err := MigrateBacktestProvider(ctx, conn); err != nil {
		return err
	}

	if err := createSymbolEvents(ctx, conn); err != nil {
		return err
	}

	if err := createIdentityTables(ctx, conn); err != nil {
		return err
	}

	return createRunTables(ctx, conn)
}

func createRunTables(ctx context.Context, conn *pgxpool.Pool) error {
	if _, err := conn.Exec(ctx, SessionTable); err != nil {
		return fmt.Errorf("failed to create table session: %w", err)
	}
	if err := MigrateSessionBacktest(ctx, conn); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, ExecutionTable); err != nil {
		return fmt.Errorf("failed to create table execution: %w", err)
	}
	if err := MigrateExecutionProvider(ctx, conn); err != nil {
		return err
	}
	return MigrateExecutionPeriod(ctx, conn)
}
