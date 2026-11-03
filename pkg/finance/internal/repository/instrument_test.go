package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lhjnilsson/foreverbull/internal/environment"
	"github.com/lhjnilsson/foreverbull/internal/test_helper"
	"github.com/lhjnilsson/foreverbull/pkg/finance/internal/repository"
	"github.com/stretchr/testify/suite"
)

const metaISIN = "US30303M1027"

type InstrumentTests struct {
	suite.Suite

	conn        *pgxpool.Pool
	instruments repository.Instruments
	assets      repository.Asset
	ohlc        repository.OHLC
}

func (test *InstrumentTests) SetupSuite() {
	test_helper.SetupEnvironment(test.T(), &test_helper.Containers{
		Postgres: true,
	})
}

func (test *InstrumentTests) SetupTest() {
	var err error
	test.conn, err = pgxpool.New(context.Background(), environment.GetPostgresURL())
	test.Require().NoError(err)
	err = repository.Recreate(context.Background(), test.conn)
	test.Require().NoError(err)
	test.instruments = repository.Instruments{Conn: test.conn}
	test.assets = repository.Asset{Conn: test.conn}
	test.ohlc = repository.OHLC{Conn: test.conn}
}

func (test *InstrumentTests) TearDownTest() {
	test.conn.Close()
}

func TestInstrument(t *testing.T) {
	suite.Run(t, new(InstrumentTests))
}

func (test *InstrumentTests) date(year int, month time.Month, day int) time.Time {
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

// storeSymbol writes a half-open [from, to) ticker row; a zero to leaves it open.
func (test *InstrumentTests) storeSymbol(ctx context.Context, isin, symbol string, from, to time.Time) {
	var toDate any
	if !to.IsZero() {
		toDate = to
	}
	_, err := test.conn.Exec(ctx,
		`INSERT INTO asset_symbol (isin, symbol, from_date, to_date) VALUES ($1, $2, $3::date, $4::date)`,
		isin, symbol, from, toDate)
	test.Require().NoError(err)
}

func (test *InstrumentTests) TestStoreMissingISIN() {
	inst, err := test.instruments.Store(context.TODO(), "", "Meta Platforms Inc")
	test.Require().ErrorIs(err, repository.ErrMissingISIN)
	test.Nil(inst)

	inst, err = test.instruments.Store(context.TODO(), "   ", "Meta Platforms Inc")
	test.Require().ErrorIs(err, repository.ErrMissingISIN)
	test.Nil(inst)
}

func (test *InstrumentTests) TestStoreDoesNotMergeByName() {
	fb, err := test.instruments.Store(context.TODO(), metaISIN, "Meta Platforms Inc")
	test.Require().NoError(err)
	other, err := test.instruments.Store(context.TODO(), "US1234567890", "Meta Platforms Inc")
	test.Require().NoError(err)
	test.NotEqual(fb.ID, other.ID)
	test.NotEqual(fb.ISIN, other.ISIN)
	test.Equal(fb.Name, other.Name)

	same, err := test.instruments.Store(context.TODO(), metaISIN, "Facebook Inc")
	test.Require().NoError(err)
	test.Equal(fb.ID, same.ID)
	test.Equal(metaISIN, same.ISIN)
	test.Equal("Facebook Inc", same.Name)

	got, err := test.instruments.Get(context.TODO(), metaISIN)
	test.Require().NoError(err)
	test.Equal(fb.ID, got.ID)
	gotOther, err := test.instruments.Get(context.TODO(), "US1234567890")
	test.Require().NoError(err)
	test.Equal(other.ID, gotOther.ID)
}

func (test *InstrumentTests) TestTickerChangeIsNewAliasNotNewInstrument() {
	ctx := context.TODO()
	fbDate := test.date(2021, time.June, 30)
	metaDate := test.date(2021, time.October, 31)

	inst, err := test.instruments.Store(ctx, metaISIN, "Facebook Inc")
	test.Require().NoError(err)
	test.storeSymbol(ctx, metaISIN, "FB", fbDate, metaDate)
	renamed, err := test.instruments.Store(ctx, metaISIN, "Meta Platforms Inc")
	test.Require().NoError(err)
	test.Equal(inst.ID, renamed.ID)
	test.storeSymbol(ctx, metaISIN, "META", metaDate, time.Time{})

	var n int64
	err = test.conn.QueryRow(ctx, `SELECT COUNT(*) FROM instrument WHERE isin=$1`, metaISIN).Scan(&n)
	test.Require().NoError(err)
	test.Equal(int64(1), n)

	err = test.conn.QueryRow(ctx, `SELECT COUNT(*) FROM asset_symbol WHERE isin=$1`, metaISIN).Scan(&n)
	test.Require().NoError(err)
	test.Equal(int64(2), n)

	resolvedFB, err := test.instruments.Resolve(ctx, "FB", fbDate)
	test.Require().NoError(err)
	test.Equal(inst.ID, resolvedFB.ID)
	test.Equal(metaISIN, resolvedFB.ISIN)

	resolvedMeta, err := test.instruments.Resolve(ctx, "META", metaDate)
	test.Require().NoError(err)
	test.Equal(inst.ID, resolvedMeta.ID)

	_, err = test.instruments.Resolve(ctx, "FB", metaDate)
	test.Require().ErrorIs(err, pgx.ErrNoRows)

	_, err = test.instruments.Resolve(ctx, "META", fbDate)
	test.Require().ErrorIs(err, pgx.ErrNoRows)

	symbol, err := test.instruments.Symbols(ctx, inst, fbDate)
	test.Require().NoError(err)
	test.Equal("FB", symbol)

	symbol, err = test.instruments.Symbols(ctx, renamed, metaDate)
	test.Require().NoError(err)
	test.Equal("META", symbol)

	symbol, err = test.instruments.Symbols(ctx, inst, metaDate.AddDate(0, 0, -1))
	test.Require().NoError(err)
	test.Equal("FB", symbol)
}

func (test *InstrumentTests) TestAssetSymbolRejectsOverlapAndAcceptsAdjacent() {
	ctx := context.TODO()
	test.Require().NoError(repository.MigrateAssetSymbol(ctx, test.conn))
	test.Require().NoError(repository.MigrateAssetSymbol(ctx, test.conn))

	_, err := test.instruments.Store(ctx, metaISIN, "Meta Platforms Inc")
	test.Require().NoError(err)
	_, err = test.conn.Exec(ctx, `
		INSERT INTO asset_symbol (isin, symbol, from_date, to_date)
		VALUES ($1, 'FB', '2021-06-30', '2021-10-31')`, metaISIN)
	test.Require().NoError(err)
	_, err = test.conn.Exec(ctx, `
		INSERT INTO asset_symbol (isin, symbol, from_date, to_date)
		VALUES ($1, 'META', '2021-08-01', NULL)`, metaISIN)
	test.Require().ErrorContains(err, "asset_symbol_no_overlap")
	_, err = test.conn.Exec(ctx, `
		INSERT INTO asset_symbol (isin, symbol, from_date, to_date)
		VALUES ($1, 'META', '2021-10-31', NULL)`, metaISIN)
	test.Require().NoError(err)

	_, err = test.conn.Exec(ctx, `
		INSERT INTO asset_symbol (isin, symbol, from_date)
		VALUES ('US0000000000', 'ZZZ', '2020-01-01')`)
	test.Require().ErrorContains(err, "asset_symbol_isin_fkey")
}

func (test *InstrumentTests) TestMigrateAssetSymbolLeavesOverlappingRows() {
	ctx := context.TODO()
	_, err := test.conn.Exec(ctx, `DROP TABLE asset_symbol`)
	test.Require().NoError(err)
	_, err = test.conn.Exec(ctx, `
		CREATE TABLE asset_symbol (
			isin text NOT NULL,
			symbol text NOT NULL,
			from_date date NOT NULL,
			to_date date,
			PRIMARY KEY (isin, from_date)
		)`)
	test.Require().NoError(err)
	_, err = test.instruments.Store(ctx, metaISIN, "Meta Platforms Inc")
	test.Require().NoError(err)
	_, err = test.conn.Exec(ctx, `
		INSERT INTO asset_symbol (isin, symbol, from_date, to_date) VALUES
		($1, 'FB', '2021-01-01', '2021-06-01'),
		($1, 'META', '2021-03-01', NULL)`, metaISIN)
	test.Require().NoError(err)

	err = repository.MigrateAssetSymbol(ctx, test.conn)
	test.Require().ErrorContains(err, "asset_symbol")

	var n int64
	err = test.conn.QueryRow(ctx, `SELECT COUNT(*) FROM asset_symbol WHERE isin=$1`, metaISIN).Scan(&n)
	test.Require().NoError(err)
	test.Equal(int64(2), n)
}

func (test *InstrumentTests) TestMigrateAssetSymbolUpgradesOldTable() {
	ctx := context.TODO()
	_, err := test.conn.Exec(ctx, `DROP TABLE asset_symbol`)
	test.Require().NoError(err)
	_, err = test.conn.Exec(ctx, `
		CREATE TABLE asset_symbol (
			isin text NOT NULL,
			symbol text NOT NULL,
			from_date date NOT NULL,
			to_date date,
			PRIMARY KEY (isin, from_date)
		)`)
	test.Require().NoError(err)
	_, err = test.instruments.Store(ctx, metaISIN, "Meta Platforms Inc")
	test.Require().NoError(err)
	_, err = test.conn.Exec(ctx, `
		INSERT INTO asset_symbol (isin, symbol, from_date, to_date)
		VALUES ($1, 'FB', '2021-06-30', '2021-10-31')`, metaISIN)
	test.Require().NoError(err)

	test.Require().NoError(repository.MigrateAssetSymbol(ctx, test.conn))
	test.Require().NoError(repository.MigrateAssetSymbol(ctx, test.conn))

	_, err = test.conn.Exec(ctx, `
		INSERT INTO asset_symbol (isin, symbol, from_date, to_date)
		VALUES ($1, 'META', '2021-10-31', NULL)`, metaISIN)
	test.Require().NoError(err)
	_, err = test.conn.Exec(ctx, `
		INSERT INTO asset_symbol (isin, symbol, from_date, to_date)
		VALUES ($1, 'META', '2021-08-01', NULL)`, metaISIN)
	test.Require().ErrorContains(err, "asset_symbol_no_overlap")

	var indexed bool
	err = test.conn.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_indexes
			WHERE schemaname = current_schema()
			  AND indexname = 'asset_symbol_symbol_from_date_idx'
		)`).Scan(&indexed)
	test.Require().NoError(err)
	test.True(indexed)
}

func (test *InstrumentTests) TestIngestAssetAndOHLCStillKeyedBySymbol() {
	ctx := context.TODO()
	_, err := test.instruments.Store(ctx, metaISIN, "Meta Platforms Inc")
	test.Require().NoError(err)
	test.storeSymbol(ctx, metaISIN, "FB", test.date(2021, time.June, 30), test.date(2021, time.October, 31))
	test.storeSymbol(ctx, metaISIN, "META", test.date(2021, time.October, 31), time.Time{})

	test.Require().NoError(test.assets.Store(ctx, "FB", "Facebook Inc"))
	test.Require().NoError(test.assets.Store(ctx, "META", "Meta Platforms Inc"))
	test.Require().NoError(test.ohlc.Store(ctx, "yahoo", "FB", test.date(2021, time.June, 30), 1, 1, 1, 1, 100))
	test.Require().NoError(test.ohlc.Store(ctx, "yahoo", "META", test.date(2021, time.October, 31), 2, 2, 2, 2, 200))

	fb, err := test.assets.Get(ctx, "FB")
	test.Require().NoError(err)
	test.Equal("FB", fb.Symbol)
	meta, err := test.assets.Get(ctx, "META")
	test.Require().NoError(err)
	test.Equal("META", meta.Symbol)

	var n int64
	err = test.conn.QueryRow(ctx, `SELECT COUNT(*) FROM ohlc WHERE symbol=$1`, "FB").Scan(&n)
	test.Require().NoError(err)
	test.Equal(int64(1), n)
	err = test.conn.QueryRow(ctx, `SELECT COUNT(*) FROM ohlc WHERE symbol=$1`, "META").Scan(&n)
	test.Require().NoError(err)
	test.Equal(int64(1), n)
}

// EnsureLaterAlias writes META from laterFrom and closes FB there. A second
// call finds META and writes nothing.
func (test *InstrumentTests) TestEnsureLaterAliasWritesTheLaterTickerOnce() {
	ctx := context.TODO()
	_, err := test.instruments.Store(ctx, metaISIN, "Meta Platforms Inc")
	test.Require().NoError(err)
	test.storeSymbol(ctx, metaISIN, "FB", test.date(2019, 1, 1), time.Time{})

	test.Require().NoError(test.instruments.EnsureLaterAlias(ctx, metaISIN, "FB", "META", test.date(2021, 1, 1)))
	test.Require().NoError(test.instruments.EnsureLaterAlias(ctx, metaISIN, "FB", "META", test.date(2023, 1, 1)))

	aliases, err := test.instruments.ListAliases(ctx, metaISIN)
	test.Require().NoError(err)
	to := test.date(2021, 1, 1)
	test.Equal([]repository.AliasInterval{
		{ISIN: metaISIN, Symbol: "FB", FromDate: test.date(2019, 1, 1), ToDate: &to},
		{ISIN: metaISIN, Symbol: "META", FromDate: test.date(2021, 1, 1)},
	}, aliases)
}

// Another ticker already starts on laterFrom, so the insert stores nothing.
// EnsureLaterAlias fails and rolls back, so FB stays open.
func (test *InstrumentTests) TestEnsureLaterAliasFailsWhenTheLaterTickerIsNotStored() {
	ctx := context.TODO()
	_, err := test.instruments.Store(ctx, metaISIN, "Meta Platforms Inc")
	test.Require().NoError(err)
	test.storeSymbol(ctx, metaISIN, "FB", test.date(2019, 1, 1), test.date(2021, 1, 1))
	test.storeSymbol(ctx, metaISIN, "FBB", test.date(2021, 1, 1), time.Time{})

	err = test.instruments.EnsureLaterAlias(ctx, metaISIN, "FBB", "META", test.date(2021, 1, 1))
	test.Require().ErrorIs(err, repository.ErrLaterAliasNotStored)

	aliases, err := test.instruments.ListAliases(ctx, metaISIN)
	test.Require().NoError(err)
	test.Require().Len(aliases, 2)
	test.Equal("FBB", aliases[1].Symbol)
	test.Nil(aliases[1].ToDate)
}
