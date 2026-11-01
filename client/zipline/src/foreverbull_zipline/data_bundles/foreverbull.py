import logging
import os
import warnings

from collections.abc import Iterable
from collections.abc import Iterator
from dataclasses import dataclass
from datetime import date
from datetime import timedelta
from math import isnan
from typing import Any
from typing import cast

import numpy as np
import pandas as pd

from pandas import read_sql_query
from sqlalchemy import text
from sqlalchemy.exc import SQLAlchemyError
from zipline.utils.calendar_utils import get_calendar
from zipline.utils.cli import maybe_show_progress

from foreverbull.broker.utils import describe_exception
from foreverbull.models.finance import ohlc_provider
from foreverbull.pb import pb_utils
from foreverbull.pb.foreverbull.backtest.v1 import backtest_pb
from foreverbull_zipline.database import get_engine
from foreverbull_zipline.errors import PlainSentence


warnings.filterwarnings("ignore")

_logger = logging.getLogger(__name__)


@dataclass(frozen=True)
class AliasSpec:
    symbol: str
    from_date: date | None = None
    to_date: date | None = None


@dataclass(frozen=True)
class InstrumentGroup:
    """One Zipline sid. FB then META is one group; the final drop sets member_to."""

    aliases: tuple[AliasSpec, ...]
    member_to: date | None = None


def group_label(group: InstrumentGroup) -> str:
    """The group's latest ticker, with its earlier ones: "META (earlier FB)"."""
    ordered = sorted(group.aliases, key=lambda alias: alias.from_date or date.min)
    latest = ordered[-1].symbol
    earlier = [symbol for symbol in dict.fromkeys(alias.symbol for alias in reversed(ordered)) if symbol != latest]
    return f"{latest} (earlier {', '.join(earlier)})" if earlier else latest


def no_stored_prices_line(
    backtest: str,
    groups: list[InstrumentGroup],
    first,
    last,
    gaps: dict[str, list[pd.Timestamp]] | None = None,
) -> str:
    """Ingest's failure when one or more sids have no stored bars at all.

    One group fits one sentence; two or more get a line each under the
    lead, in ingest order. Tickers with missing sessions found in the same
    read get a line each after them, so one error names every cause.
    """
    gaps = gaps or {}
    span = f"between {first:%Y-%m-%d} and {last:%Y-%m-%d}"
    saved = "The backtest wasn't saved."
    if gaps:
        lines = [
            f"Couldn't load data for backtest '{backtest}': {len(groups) + len(gaps)} symbols are missing prices. {saved}"
        ]
        lines.extend(f"  {group_label(group)}: no prices {span}." for group in groups)
        lines.extend(
            f"  {symbol}: no prices on {', '.join(f'{day:%Y-%m-%d}' for day in days)}." for symbol, days in gaps.items()
        )
        lines.append(
            "Try another provider with --provider, or change the backtest dates, then run `fbull backtest create` again."
        )
        return "\n".join(lines)
    next_step = "Try another provider with --provider, then run `fbull backtest create` again."
    if len(groups) == 1:
        return (
            f"Couldn't load data for backtest '{backtest}': there are no stored prices for "
            f"{group_label(groups[0])} {span}. {saved} {next_step}"
        )
    lines = [f"Couldn't load data for backtest '{backtest}': {len(groups)} symbols have no stored prices. {saved}"]
    lines.extend(f"  {group_label(group)}: no prices {span}." for group in groups)
    lines.append(next_step)
    return "\n".join(lines)


def ticker_history_line(backtest: str) -> str:
    """Ingest's failure when the record of ticker changes can't be read."""
    return (
        f"Couldn't load data for backtest '{backtest}': foreverbull couldn't read its record of ticker changes, "
        "so renamed symbols can't be followed. The backtest wasn't saved. "
        "Run `fbenv status` to check that the environment is running, then run `fbull backtest create` again."
    )


def membership_auto_close_date(
    end_date,
    membership_to: date | None,
    *,
    newest_member_end,
    bundle_end,
):
    """When Zipline closes a held position in this sid at its last close.

    The broker leaves a member out of the run from the session after its
    last bar only when that bar is before the newest bar of any member
    (§7.2). Zipline must agree: a delisted member auto-closes the day after
    its last bar, or on its drop when that is earlier, so the cash is
    credited the session it is left out. A member whose last bar is the
    newest member bar is where the stored data ends, not a delisting: it
    closes only on its drop, otherwise the day after the bundle ends, so a
    benchmark session no member has does not sell it. A rename does not
    auto-close the sid: end_date is the ISIN's last bar under any ticker.
    """
    dropped = None if membership_to is None else pd.Timestamp(membership_to)
    last_bar = cast(pd.Timestamp, pd.Timestamp(end_date))
    if last_bar < cast(pd.Timestamp, pd.Timestamp(newest_member_end)):
        after_last_bar = last_bar + pd.Timedelta(days=1)
        return after_last_bar if dropped is None else min(dropped, after_last_bar)
    if dropped is not None:
        return dropped
    return pd.Timestamp(bundle_end) + pd.Timedelta(days=1)


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
    drops: dict[str, list[date | None]] = {}
    symbol_isin: dict[str, str] = {}
    # itertuples is typed as a plain tuple; the names are the query columns.
    # A row is one ticker of an ISIN in one membership interval, so a member
    # that was dropped and re-added lists each ticker once per interval.
    for row in cast(Iterable[Any], aliases.itertuples(index=False)):
        isin = str(row.isin)
        symbol = str(row.symbol)
        from_date = _as_date(getattr(row, "from_date", None))
        to_date = _as_date(getattr(row, "to_date", None))
        alias = AliasSpec(symbol=symbol, from_date=from_date, to_date=to_date)
        if alias not in by_isin.setdefault(isin, []):
            by_isin[isin].append(alias)
        drops.setdefault(isin, []).append(_as_date(getattr(row, "member_to", None)))
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
        groups.append(InstrumentGroup(aliases=tuple(by_isin[isin]), member_to=final_drop(drops[isin])))
    return groups


def final_drop(drops: list[date | None]) -> date | None:
    """The end of a member's final membership interval, None while still a member.

    Intervals of one ISIN do not overlap, so the final one is the open one,
    else the one that ends last. An earlier drop is not where the member
    leaves the universe when it is added back later.
    """
    if any(dropped is None for dropped in drops):
        return None
    return max(cast(list[date], drops))


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


def alias_windows(aliases: tuple[AliasSpec, ...], from_date: date, to_date: date) -> list[tuple[AliasSpec, date, date]]:
    """Each alias's dates inside the backtest, from asset_symbol.

    asset_symbol.to_date is exclusive. The first alias reaches back to the
    backtest start and the last one forward to its end, so an ISIN keeps
    the bars it had before its ticker history was known.
    """
    ordered = sorted(aliases, key=lambda alias: alias.from_date or date.min)
    windows: list[tuple[AliasSpec, date, date]] = []
    for position, alias in enumerate(ordered):
        start = from_date if position == 0 or alias.from_date is None else max(alias.from_date, from_date)
        end = to_date
        if position < len(ordered) - 1 and alias.to_date is not None:
            end = min(alias.to_date - timedelta(days=1), to_date)
        if start <= end:
            windows.append((alias, start, end))
    return windows


def _in_window(frame: pd.DataFrame, start: date, end: date) -> pd.DataFrame:
    days = frame.index.date  # type: ignore[attr-defined]
    return cast(pd.DataFrame, frame[(days >= start) & (days <= end)])


def resolve_group_bars(
    group: InstrumentGroup,
    frames: dict[str, pd.DataFrame],
    from_date: date,
    to_date: date,
) -> tuple[pd.DataFrame | None, list[tuple[str, pd.Timestamp, pd.Timestamp]]]:
    """Bars and symbol mappings of one sid, alias by alias.

    Inside an alias's dates the bars stored under that ticker win. A
    session it has no bar for takes the bar another ticker of the ISIN
    has: Yahoo serves FB's history only under META. Each ticker maps to
    the sid from its first to its last bar inside its own dates, so a
    lookup on a date finds the ticker the universe lists on it.
    """
    pieces: list[pd.DataFrame] = []
    mapping_rows: list[tuple[str, pd.Timestamp, pd.Timestamp]] = []
    for alias, start, end in alias_windows(group.aliases, from_date, to_date):
        bars = _in_window(frames[alias.symbol], start, end)
        for other in group.aliases:
            if other.symbol == alias.symbol:
                continue
            fill = _in_window(frames[other.symbol], start, end)
            bars = pd.concat([bars, fill[~fill.index.isin(bars.index)]])
        if len(bars) == 0:
            continue
        bars = cast(pd.DataFrame, bars.sort_index())
        pieces.append(bars)
        mapping_rows.append((alias.symbol, _index_timestamp(bars.index, 0), _index_timestamp(bars.index, -1)))
    if not pieces:
        return None, mapping_rows
    combined = pd.concat(pieces).sort_index()
    combined = combined[~combined.index.duplicated(keep="first")]
    if not isinstance(combined, pd.DataFrame):
        combined = pd.DataFrame(combined)
    return combined, mapping_rows


_PRICE_COLUMNS = ["open", "high", "low", "close"]


def drop_unpriced_bars(bars: pd.DataFrame) -> pd.DataFrame:
    """Leave out rows with a missing, zero, or negative price.

    Such a row is a missing bar, not a price: the broker stored Yahoo's
    null as 0 before it refused non-positive prices, and Zipline would
    read a 0 close as a -100% day. A dropped row is a missing session, which
    fails the create.
    """
    priced = bars[_PRICE_COLUMNS].gt(0).all(axis=1)
    return cast(pd.DataFrame, bars[priced])


def missing_sessions(bars: pd.DataFrame, sessions: pd.DatetimeIndex) -> list[pd.Timestamp]:
    """Sessions between a sid's first and last bar that have no bar.

    The broker fails ``fbull backtest create`` on these before the ingest
    (universe.md section 7.5), so a backtest never runs on a price nobody
    sent. Days before the first bar or after the last are not missing.
    """
    if bars.empty:
        return []
    first, last = bars.index[0], bars.index[-1]
    in_range = sessions[(sessions >= first) & (sessions <= last)]
    return list(in_range.difference(bars.index))


class MissingSessionsError(ValueError):
    """Sids with no bar on sessions inside their span; nothing is filled.

    gaps maps each ticker to its missing sessions, oldest first. The text is
    the reason the broker reads to name them in the create's missing-session
    sentence (pkg/backtest/universe.md section 7.5), so keep its shape:
    ``missing prices on trading days: AAA 2024-02-14; BBB 2024-02-20,2024-03-05``.
    """

    def __init__(self, gaps: dict[str, list[pd.Timestamp]]):
        self.gaps = gaps
        listed = "; ".join(
            f"{symbol} {','.join(day.date().isoformat() for day in days)}" for symbol, days in gaps.items()
        )
        super().__init__(f"missing prices on trading days: {listed}")


def ticker_on_day(group: InstrumentGroup, from_date: date, to_date: date, day: pd.Timestamp) -> str:
    """The ticker of the group whose dates hold day, else the last one."""
    windows = alias_windows(group.aliases, from_date, to_date)
    for alias, start, end in windows:
        if pd.Timestamp(start) <= day <= pd.Timestamp(end):
            return alias.symbol
    return group.aliases[-1].symbol


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
        """Bars stored on any UTC day from from_date to to_date, both included.

        A provider stamps a daily bar inside its session (Yahoo at the
        14:30 UTC open), so the end is the start of the day after to_date:
        BETWEEN with a date stops at midnight and drops the last day's bar.
        """
        query = text(
            """SELECT open, high, low, close, volume, time FROM ohlc
            WHERE symbol = :symbol AND provider = :provider
              AND time >= :from_date AND time < :after_to_date
            ORDER BY time asc"""
        )
        return read_sql_query(
            query,
            self._engine,
            params={
                "symbol": symbol,
                "provider": self.provider or ohlc_provider(),
                "from_date": _as_date(from_date),
                "after_to_date": cast(date, _as_date(to_date)) + timedelta(days=1),
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
        except SQLAlchemyError as exc:
            # The cause can quote the database URL, so it is logged redacted
            # and kept out of the message and the exception chain.
            _logger.error(
                "Couldn't read the ticker history of backtest %r from the database: %s",
                self.backtest.name,
                describe_exception(exc),
            )
            raise PlainSentence(ticker_history_line(self.backtest.name)) from None

    def ingest_groups(self) -> list[InstrumentGroup]:
        return group_symbols_by_instrument(ingest_symbols(self.backtest), self.load_aliases())

    def writer(self, show_progress: bool, calendar=None) -> Iterator[tuple[int, pd.DataFrame]]:
        from_date = pb_utils.from_proto_date_to_pydate(self.backtest.periods[0].start_date)
        to_date = pb_utils.from_proto_date_to_pydate(self.backtest.periods[-1].end_date)
        groups = self.ingest_groups()
        calendar = calendar if calendar is not None else get_calendar("XNYS")
        sessions = calendar.sessions_in_range(from_date, to_date)

        # The auto-close of a sid depends on the newest bar of every member,
        # so the rows are written once every sid's bars are read.
        ends: list[tuple[int, pd.Timestamp, date | None, bool, list[tuple[str, pd.Timestamp, pd.Timestamp]]]] = []
        gaps: dict[str, list[pd.Timestamp]] = {}
        # Every group is read before any is written, so one error names every
        # group without bars and every gap, and nothing is ingested.
        resolved: list[tuple[InstrumentGroup, pd.DataFrame, list[tuple[str, pd.Timestamp, pd.Timestamp]]]] = []
        empty: list[InstrumentGroup] = []
        with maybe_show_progress(groups, show_progress, label="Ingesting from SQL"):
            for group in groups:
                frames = {
                    symbol: self._bars(symbol, from_date, to_date)
                    for symbol in dict.fromkeys(alias.symbol for alias in group.aliases)
                }
                combined, mapping_rows = resolve_group_bars(group, frames, from_date, to_date)
                if combined is None:
                    empty.append(group)
                    continue
                missing = missing_sessions(combined, sessions)
                if missing:
                    # The broker checks this before the ingest. If a bar went
                    # missing in between, fail rather than write a bundle
                    # Zipline would refuse or a price nobody sent.
                    for day in missing:
                        gaps.setdefault(ticker_on_day(group, from_date, to_date, day), []).append(day)
                    continue
                resolved.append((group, combined, mapping_rows))
        if empty:
            first, last = (sessions[0], sessions[-1]) if len(sessions) else (from_date, to_date)
            raise PlainSentence(no_stored_prices_line(self.backtest.name, empty, first, last, gaps))
        if gaps:
            raise MissingSessionsError(gaps)
        for index, (group, combined, mapping_rows) in enumerate(resolved):
            is_benchmark = all(alias.symbol == self.backtest.benchmark for alias in group.aliases)
            ends.append((index, _index_timestamp(combined.index, -1), group.member_to, is_benchmark, mapping_rows))
            yield index, combined
        member_ends = [last for _, last, _, is_benchmark, _ in ends if not is_benchmark] or [
            last for _, last, *_ in ends
        ]
        newest_member_end = max(member_ends)
        for index, last, member_to, _, mapping_rows in ends:
            auto_close = membership_auto_close_date(
                last, member_to, newest_member_end=newest_member_end, bundle_end=to_date
            )
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

    def _bars(self, symbol: str, from_date: date, to_date: date) -> pd.DataFrame:
        data = self.get_stock_data(symbol, from_date, to_date)
        data.dropna(inplace=True)
        return drop_unpriced_bars(data)

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
        daily_bar_writer.write(self.writer(show_progress, calendar), show_progress=show_progress)
        asset_db_writer.write(equities=self._df_metadata)
        adjustment_writer.write()
