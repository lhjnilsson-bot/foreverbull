from datetime import datetime
from datetime import timedelta

import yfinance
import yfinance.data

from sqlalchemy import Column
from sqlalchemy import Date
from sqlalchemy import DateTime
from sqlalchemy import Integer
from sqlalchemy import String
from sqlalchemy import UniqueConstraint
from sqlalchemy import engine
from sqlalchemy import text
from sqlalchemy.orm import declarative_base
from sqlalchemy.types import Numeric

from foreverbull.models.finance import ohlc_provider
from foreverbull.pb import pb_utils
from foreverbull.pb.foreverbull.backtest.v1 import backtest_pb
from foreverbull.pb.foreverbull.finance.v1 import marketdata_pb


Base = declarative_base()


class Asset(Base):
    __tablename__ = "asset"
    symbol = Column("symbol", String(), primary_key=True)
    name = Column("name", String())


class OHLC(Base):
    __tablename__ = "ohlc"
    id = Column(Integer, primary_key=True)
    symbol = Column(String())
    open = Column(Numeric())
    high = Column(Numeric())
    low = Column(Numeric())
    close = Column(Numeric())
    volume = Column(Integer())
    time = Column(DateTime())
    provider = Column(String(), nullable=False, server_default="yahoo")

    __table_args__ = (UniqueConstraint("provider", "symbol", "time", name="ohlc_provider_symbol_time_key"),)


class AssetSymbol(Base):
    """The broker's dated ticker alias (pkg/backtest/universe.md §4.3), without its constraints.

    Bar reads resolve a ticker to its ISIN through this table, so a test
    database needs it even when it holds no rows.
    """

    __tablename__ = "asset_symbol"
    isin = Column(String(), primary_key=True)
    symbol = Column(String(), nullable=False)
    from_date = Column(Date(), primary_key=True)
    to_date = Column(Date())


class UniverseMember(Base):
    """A backtest's dated index membership (pkg/backtest/universe.md §6.1), without its constraints.

    Ingest reads the ticker history through this table and fails when it
    can't, so a test database needs it even when it holds no rows.
    """

    __tablename__ = "universe_member"
    backtest_name = Column(String(), primary_key=True)
    isin = Column(String(), primary_key=True)
    from_date = Column(Date(), primary_key=True)
    to_date = Column(Date())


_OHLC_PROVIDER_COLUMN = text("ALTER TABLE ohlc ADD COLUMN IF NOT EXISTS provider text NOT NULL DEFAULT 'yahoo'")

_OHLC_PROVIDER_KEY = text(
    """
    DO $$
    DECLARE
        r record;
    BEGIN
        FOR r IN
            SELECT c.conname
            FROM pg_constraint c
            JOIN pg_class t ON t.oid = c.conrelid
            JOIN pg_namespace n ON n.oid = t.relnamespace
            WHERE t.relname = 'ohlc'
              AND n.nspname = current_schema()
              AND c.contype = 'u'
              AND (
                c.conname IN ('ohlc_symbol_time_key', 'symbol_time_uc')
                OR regexp_replace(pg_get_constraintdef(c.oid), '\\s+', ' ', 'g') IN (
                    'UNIQUE (symbol, time)',
                    'UNIQUE (symbol, "time")'
                )
              )
        LOOP
            EXECUTE format('ALTER TABLE ohlc DROP CONSTRAINT %I', r.conname);
        END LOOP;

        IF NOT EXISTS (
            SELECT 1
            FROM pg_constraint c
            JOIN pg_class t ON t.oid = c.conrelid
            JOIN pg_namespace n ON n.oid = t.relnamespace
            WHERE t.relname = 'ohlc'
              AND n.nspname = current_schema()
              AND c.conname = 'ohlc_provider_symbol_time_key'
        ) THEN
            ALTER TABLE ohlc
                ADD CONSTRAINT ohlc_provider_symbol_time_key UNIQUE (provider, symbol, time);
        END IF;
    END $$
    """
)


# Ingest reads confirmed listings and delistings and fails when it can't,
# so a test database needs the table even when it holds no rows. No foreign
# key: this database has no backtest table.
_SYMBOL_EVENT = text(
    """
    CREATE TABLE IF NOT EXISTS backtest_symbol_event (
        backtest_name text NOT NULL,
        symbol text NOT NULL,
        kind text NOT NULL,
        on_date date NOT NULL,
        evidence text NOT NULL,
        PRIMARY KEY (backtest_name, symbol, kind)
    )
    """
)


def prepare(database: engine.Engine) -> None:
    """Create tables and add ohlc.provider on a database that already has the old unique key."""
    Base.metadata.create_all(database)
    with database.begin() as conn:
        conn.execute(_OHLC_PROVIDER_COLUMN)
        conn.execute(_OHLC_PROVIDER_KEY)
        conn.execute(_SYMBOL_EVENT)


def _bars_provider(backtest: backtest_pb.Backtest) -> str:
    named = (getattr(backtest, "provider", "") or "").strip().lower()
    if named:
        return named
    return ohlc_provider()


def verify(database: engine.Engine, backtest: backtest_pb.Backtest):
    # TODO: Refactor using exchange calendar
    with database.connect() as conn:
        provider = _bars_provider(backtest)
        for period in backtest.periods:
            for asset in period.assets:
                if not asset.enabled:
                    continue
                result = conn.execute(
                    text("SELECT min(time), max(time) FROM ohlc WHERE symbol = :symbol AND provider = :provider"),
                    {"symbol": asset.symbol, "provider": provider},
                )
                res = result.fetchone()
                if res is None:
                    return False
                start, end = res
                if start is None or end is None:
                    return False
                if start.date() != pb_utils.from_proto_date_to_pydate(period.start_date):
                    return False
                if end.date() != pb_utils.from_proto_date_to_pydate(period.end_date):
                    return False

        start_date = pb_utils.from_proto_date_to_pydate(backtest.periods[0].start_date)
        end_date = pb_utils.from_proto_date_to_pydate(backtest.periods[-1].end_date)
        result = conn.execute(
            text("SELECT min(time), max(time) FROM ohlc WHERE symbol = :symbol AND provider = :provider"),
            {"symbol": backtest.benchmark, "provider": provider},
        )
        res = result.fetchone()
        if res is None:
            return False
        start, end = res
        if start is None or end is None:
            return False
        if start.date() != start_date:
            return False
        return end.date() == end_date


def populate(database: engine.Engine, backtest: backtest_pb.Backtest):
    # Latest yfinance claims to have fixed the too-many-request issue, keep until verified
    # yfinance.data.YfData.user_agent_headers["User-Agent"] = (
    #    "Mozilla/5.0 (Windows NT 6.1; Trident/7.0; rv:11.0) like Gecko"
    # )
    def what_to_populate():
        for period in backtest.periods:
            for asset in period.assets:
                if not asset.enabled:
                    continue
                yield (
                    pb_utils.from_proto_date_to_pydate(backtest.periods[0].start_date),
                    pb_utils.from_proto_date_to_pydate(backtest.periods[-1].end_date),
                    asset.symbol,
                )
        if backtest.benchmark:
            yield (
                pb_utils.from_proto_date_to_pydate(backtest.periods[0].start_date),
                pb_utils.from_proto_date_to_pydate(backtest.periods[-1].end_date),
                backtest.benchmark,
            )

    provider = _bars_provider(backtest)
    with database.connect() as conn:
        for start, end, symbol in what_to_populate():
            feed = yfinance.Ticker(symbol)
            info = feed.info
            asset = marketdata_pb.Asset(
                symbol=info["symbol"],
                name="",
            )
            conn.execute(
                text(
                    """INSERT INTO asset (symbol, name)
                    VALUES (:symbol, :name) ON CONFLICT DO NOTHING"""
                ),
                {"symbol": asset.symbol, "name": asset.name},
            )
            data = feed.history(
                start=start,
                end=end + timedelta(days=1),
            )
            for idx, row in data.iterrows():
                time = datetime(
                    idx.year,  # type: ignore
                    idx.month,  # type: ignore
                    idx.day,  # type: ignore
                    idx.hour,  # type: ignore
                    idx.minute,  # type: ignore
                    idx.second,  # type: ignore
                )
                conn.execute(
                    text(
                        """INSERT INTO ohlc (provider, symbol, open, high, low, close, volume, time)
                        VALUES (:provider, :symbol, :open, :high, :low, :close, :volume, :time)
                        ON CONFLICT (provider, symbol, time) DO NOTHING"""
                    ),
                    {
                        "provider": provider,
                        "symbol": asset.symbol,
                        "open": row.Open.item(),
                        "high": row.High.item(),
                        "low": row.Low.item(),
                        "close": row.Close.item(),
                        "volume": int(row.Volume),
                        "time": time,
                    },
                )
            conn.commit()
