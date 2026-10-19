import os
import warnings

from collections.abc import Iterable
from collections.abc import Iterator
from dataclasses import dataclass
from datetime import date
from math import isnan
from typing import Any
from typing import cast

import numpy as np
import pandas as pd

from pandas import read_sql_query
from sqlalchemy import text
from sqlalchemy.exc import SQLAlchemyError
from zipline.utils.cli import maybe_show_progress

from foreverbull.models import get_engine
from foreverbull.models.finance import ohlc_provider
from foreverbull.pb import pb_utils
from foreverbull.pb.foreverbull.backtest.v1 import backtest_pb


warnings.filterwarnings("ignore")


@dataclass(frozen=True)
class AliasSpec:
    symbol: str
    from_date: date | None = None
    to_date: date | None = None


@dataclass(frozen=True)
class InstrumentGroup:
    """One Zipline sid. FB then META is one group; a drop sets member_to."""

    aliases: tuple[AliasSpec, ...]
    member_to: date | None = None


def membership_auto_close_date(end_date, membership_to: date | None):
    """Close positions on a drop. Rename does not auto-close the sid."""
    if membership_to is not None:
        return pd.Timestamp(membership_to)
    return pd.Timestamp(end_date) + pd.Timedelta(days=1)


def bundle_provider(backtest: backtest_pb.Backtest) -> str:
    """Bar provider for a bundle. The backtest field wins. Otherwise OHLC_PROVIDER, else DEFAULT_PROVIDER (yahoo)."""
    named = (getattr(backtest, "provider", "") or "").strip().lower()
    if named:
        return named
    return ohlc_provider()


def ingest_symbols(backtest: backtest_pb.Backtest) -> list[str]:
    symbols = {asset.symbol for period in backtest.periods for asset in period.assets if asset.enabled and asset.symbol}
    if backtest.benchmark:
        symbols.add(backtest.benchmark)
    return sorted(symbols)


def group_symbols_by_instrument(
    symbols: Iterable[str],
    aliases: pd.DataFrame | None,
) -> list[InstrumentGroup]:
    """Collapse ticker aliases of one ISIN into one sid. Unknown symbols stay solo."""
    wanted = list(dict.fromkeys(symbol for symbol in symbols if symbol))
    if aliases is None or aliases.empty:
        return [InstrumentGroup(aliases=(AliasSpec(symbol=symbol),)) for symbol in wanted]

    by_isin: dict[str, list[AliasSpec]] = {}
    member_to: dict[str, date | None] = {}
    symbol_isin: dict[str, str] = {}
    # itertuples is typed as a plain tuple; the names are the query columns.
    for row in cast(Iterable[Any], aliases.itertuples(index=False)):
        isin = str(row.isin)
        symbol = str(row.symbol)
        from_date = _as_date(getattr(row, "from_date", None))
        to_date = _as_date(getattr(row, "to_date", None))
        dropped = _as_date(getattr(row, "member_to", None))
        by_isin.setdefault(isin, []).append(AliasSpec(symbol=symbol, from_date=from_date, to_date=to_date))
        member_to[isin] = dropped
        symbol_isin[symbol] = isin

    groups: list[InstrumentGroup] = []
    seen_isins: set[str] = set()
    for symbol in wanted:
        isin = symbol_isin.get(symbol)
        if isin is None:
            groups.append(InstrumentGroup(aliases=(AliasSpec(symbol=symbol),)))
            continue
        if isin in seen_isins:
            continue
        seen_isins.add(isin)
        groups.append(InstrumentGroup(aliases=tuple(by_isin[isin]), member_to=member_to.get(isin)))
    return groups


def _as_date(value) -> date | None:
    if value is None:
        return None
    if isinstance(value, float) and isnan(value):
        return None
    if type(value) is date:
        return value
    ts = pd.to_datetime(value, errors="coerce")
    if not isinstance(ts, pd.Timestamp) or ts is pd.NaT:
        return None
    return date(int(ts.year), int(ts.month), int(ts.day))


def session_labels(times) -> pd.DatetimeIndex:
    """Zipline session labels for stored daily bars.

    A provider stamps a daily bar inside its session, not on the session
    label: Yahoo stamps the 09:30 ET open (14:30 UTC), while a date-only
    source stamps midnight. Zipline keys daily bars, the asset start and
    end dates, and its benchmark series on the calendar session label
    (midnight of the trading day), so a 14:30 first bar reads as an asset
    that starts after the first session. Keep the UTC calendar date of
    the stored bar and drop the time of day.
    """
    parsed = pd.Series(pd.to_datetime(times))
    if parsed.dt.tz is not None:
        parsed = parsed.dt.tz_convert("UTC").dt.tz_localize(None)
    return pd.DatetimeIndex(parsed.dt.normalize())


def _index_timestamp(index: pd.Index, position: int) -> pd.Timestamp:
    values = index.to_numpy()
    return cast(pd.Timestamp, pd.Timestamp(str(values[position])))


class DatabaseEngine:
    def __init__(self):
        database_uri = os.environ.get("DATABASE_URL")
        if database_uri is None:
            raise KeyError("DATABASE_URL environment variable not set")

        self._engine = get_engine(database_uri)
        self.provider = ohlc_provider()

    def get_data(self, symbol, from_date, to_date):
        query = text(
            """SELECT open, high, low, close, volume, time FROM ohlc
            WHERE symbol = :symbol AND provider = :provider
              AND time BETWEEN :from_date AND :to_date
            ORDER BY time asc"""
        )
        return read_sql_query(
            query,
            self._engine,
            params={
                "symbol": symbol,
                "provider": self.provider or ohlc_provider(),
                "from_date": from_date,
                "to_date": to_date,
            },
        )


class SQLIngester:
    engine: DatabaseEngine
    symbols: list[str]
    from_date: date
    to_date: date

    def __init__(
        self,
        backtest: backtest_pb.Backtest,
        engine=None,
        aliases: pd.DataFrame | None = None,
    ):
        self.backtest = backtest
        self.engine = engine or DatabaseEngine()
        if hasattr(self.engine, "provider"):
            self.engine.provider = bundle_provider(backtest)
        self._aliases = aliases

    def create_metadata(self) -> pd.DataFrame:
        return pd.DataFrame(
            np.empty(
                0,
                dtype=[
                    ("sid", "int64"),
                    ("start_date", "datetime64[ns]"),
                    ("end_date", "datetime64[ns]"),
                    ("auto_close_date", "datetime64[ns]"),
                    ("symbol", "object"),
                    ("exchange", "object"),
                ],
            )
        )

    def get_stock_data(self, symbol: str, from_date: date, to_date: date) -> pd.DataFrame:
        data = self.engine.get_data(symbol, from_date, to_date)
        data["time"] = session_labels(data["time"])
        data.rename(columns={"time": "Date"}, inplace=True, copy=False)
        data.set_index("Date", inplace=True)
        return data

    def load_aliases(self) -> pd.DataFrame | None:
        if self._aliases is not None:
            return self._aliases
        try:
            query = f"""SELECT s.isin, s.symbol, s.from_date, s.to_date, m.to_date AS member_to
            FROM universe_member m
            JOIN asset_symbol s ON s.isin = m.isin
            WHERE m.backtest_name = '{self.backtest.name}'
            ORDER BY s.isin, s.from_date"""
            return read_sql_query(query, self.engine._engine)
        except SQLAlchemyError:
            return None

    def ingest_groups(self) -> list[InstrumentGroup]:
        return group_symbols_by_instrument(ingest_symbols(self.backtest), self.load_aliases())

    def writer(self, show_progress: bool) -> Iterator[tuple[int, pd.DataFrame]]:
        from_date = pb_utils.from_proto_date_to_pydate(self.backtest.periods[0].start_date)
        to_date = pb_utils.from_proto_date_to_pydate(self.backtest.periods[-1].end_date)
        groups = self.ingest_groups()

        with maybe_show_progress(groups, show_progress, label="Ingesting from SQL"):
            for index, group in enumerate(groups):
                frames: list[pd.DataFrame] = []
                mapping_rows: list[tuple[str, pd.Timestamp, pd.Timestamp]] = []
                for alias in group.aliases:
                    data = self.get_stock_data(alias.symbol, from_date, to_date)
                    if len(data) == 0:
                        continue
                    data.dropna(inplace=True)
                    if len(data) == 0:
                        continue
                    frames.append(data)
                    mapping_rows.append(
                        (
                            alias.symbol,
                            _index_timestamp(data.index, 0),
                            _index_timestamp(data.index, -1),
                        )
                    )
                if not frames:
                    symbols = ", ".join(alias.symbol for alias in group.aliases)
                    raise ValueError(f"No data found for {symbols} on {from_date} to {to_date}")
                combined = pd.concat(frames).sort_index()
                combined = combined[~combined.index.duplicated(keep="last")]
                if not isinstance(combined, pd.DataFrame):
                    combined = pd.DataFrame(combined)
                auto_close = membership_auto_close_date(combined.index[-1], group.member_to)
                for symbol, start_date, end_date in mapping_rows:
                    asset_row = pd.DataFrame(
                        {
                            "sid": [index],
                            "start_date": [start_date],
                            "end_date": [end_date],
                            "auto_close_date": [auto_close],
                            "symbol": [symbol],
                            "exchange": ["NASDAQ"],
                        }
                    )
                    self._df_metadata = pd.concat([self._df_metadata, asset_row], ignore_index=True)
                yield index, combined

    def __call__(
        self,
        environ,
        asset_db_writer,
        minute_bar_writer,
        daily_bar_writer,
        adjustment_writer,
        calendar,
        start_session,
        end_session,
        cache,
        show_progress,
        output_dir,
    ):
        self._df_metadata = self.create_metadata()
        daily_bar_writer.write(self.writer(show_progress), show_progress=show_progress)
        asset_db_writer.write(equities=self._df_metadata)
        adjustment_writer.write()
