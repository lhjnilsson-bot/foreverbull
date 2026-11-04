from datetime import datetime
from multiprocessing import get_start_method
from multiprocessing import set_start_method
from pathlib import Path

import pandas as pd
import pytest

from foreverbull_testing import database
from sqlalchemy import text

from foreverbull.pb import pb_utils
from foreverbull.pb.foreverbull import common_pb
from foreverbull.pb.foreverbull.backtest.v1 import backtest_pb
from foreverbull.pb.foreverbull.backtest.v1 import execution_pb


@pytest.fixture(autouse=True, scope="session")
def spawn_process():
    method = get_start_method()
    if method != "spawn":
        set_start_method("spawn", force=True)


@pytest.fixture(scope="session")
def backtest_entity():
    return backtest_pb.Backtest(
        name="testing_backtest",
        benchmark="OEF",
        periods=[
            backtest_pb.Backtest.Period(
                start_date=common_pb.Date(year=2022, month=1, day=1),
                end_date=common_pb.Date(year=2022, month=12, day=31),
                assets=[
                    backtest_pb.Backtest.Period.Asset(
                        symbol="AAPL",
                        enabled=True,
                    ),
                    backtest_pb.Backtest.Period.Asset(
                        symbol="MSFT",
                        enabled=True,
                    ),
                ],
            ),
            backtest_pb.Backtest.Period(
                start_date=common_pb.Date(year=2023, month=1, day=1),
                end_date=common_pb.Date(year=2023, month=12, day=31),
                assets=[
                    backtest_pb.Backtest.Period.Asset(
                        symbol="AAPL",
                        enabled=True,
                    ),
                    backtest_pb.Backtest.Period.Asset(
                        symbol="AMZN",
                        enabled=True,
                    ),
                ],
            ),
        ],
    )


@pytest.fixture(scope="function")
def execution(backtest_entity):
    return execution_pb.Execution(
        id="test",
        backtest=backtest_entity.name,
        start_date=common_pb.Date(year=2022, month=1, day=1),
        end_date=common_pb.Date(year=2023, month=12, day=31),
    )


TESTDATA = Path(__file__).parent / "testdata"

# Yahoo daily bars recorded on 2026-10-29 for the symbols backtest_entity uses.
# The zipline tests read these instead of calling Yahoo, so they run offline.
# Prices are Yahoo's dividend-adjusted values (AAPL closes at 177.79 on 2022-01-03,
# the raw close was 182.01), not the split-only basis production stores. They are
# fine as test input but are not a golden for Yahoo's `close` basis.
RECORDED_RANGE = "2022-01-01_2023-12-31"


def _recorded_bars(symbol: str) -> pd.DataFrame:
    path = TESTDATA / f"yahoo_{symbol.lower()}_{RECORDED_RANGE}.csv"
    if not path.exists():
        raise LookupError(
            f"No recorded Yahoo bars for {symbol} in {path.parent}. "
            "The zipline tests do not fetch prices live; record a CSV for it first."
        )
    return pd.read_csv(path, parse_dates=["date"])


def _symbols(entity: backtest_pb.Backtest) -> list[str]:
    symbols = [asset.symbol for period in entity.periods for asset in period.assets if asset.enabled]
    if entity.benchmark:
        symbols.append(entity.benchmark)
    return list(dict.fromkeys(symbols))


@pytest.fixture(scope="session")
def fb_database(fb_database):
    """The plugin's Postgres container, filled from recorded Yahoo CSVs rather than live Yahoo."""
    engine, _ = fb_database

    def load_recorded(entity: backtest_pb.Backtest):
        start = pb_utils.from_proto_date_to_pydate(entity.periods[0].start_date)
        end = pb_utils.from_proto_date_to_pydate(entity.periods[-1].end_date)
        provider = database._bars_provider(entity)
        with engine.begin() as conn:
            for symbol in _symbols(entity):
                bars = _recorded_bars(symbol)
                bars = bars.loc[(bars.date.dt.date >= start) & (bars.date.dt.date <= end)]
                if bars.empty:
                    raise LookupError(f"Recorded Yahoo bars for {symbol} do not cover {start} to {end}")
                conn.execute(
                    text("INSERT INTO asset (symbol, name) VALUES (:symbol, '') ON CONFLICT DO NOTHING"),
                    {"symbol": symbol},
                )
                conn.execute(
                    text(
                        """INSERT INTO ohlc (provider, symbol, open, high, low, close, volume, time)
                        VALUES (:provider, :symbol, :open, :high, :low, :close, :volume, :time)
                        ON CONFLICT (provider, symbol, time) DO UPDATE SET
                            open = EXCLUDED.open, high = EXCLUDED.high, low = EXCLUDED.low,
                            close = EXCLUDED.close, volume = EXCLUDED.volume"""
                    ),
                    [
                        {
                            "provider": provider,
                            "symbol": symbol,
                            "open": row["open"],
                            "high": row["high"],
                            "low": row["low"],
                            "close": row["close"],
                            "volume": int(row["volume"]),
                            "time": datetime(row["date"].year, row["date"].month, row["date"].day),
                        }
                        for row in bars.to_dict("records")
                    ],
                )
                database.record_dividend_fetch(conn, provider, symbol)

    yield engine, load_recorded
