"""Cash dividends in the bundle.

Bars are split-adjusted only, so dividends come in as cash events, credited
to cash at the open of the ex-date session: shares held at the previous
close times the amount, negative for a short. The provider's pay date is
not used; the bundle writes the ex-date as the pay date, so Zipline earns
and pays the dividend on the same session. Stooq bars are already
dividend-adjusted and get none.

Every dividend is checked before anything is written, and one error names
every refused dividend with its provider, symbol, ex-date and reason.
"""

import math
import sqlite3

from collections.abc import Iterable
from dataclasses import dataclass
from datetime import date
from typing import Any
from typing import cast

import numpy as np
import pandas as pd


# The columns a dividend frame carries, read from the broker's dividend table.
DIVIDEND_COLUMNS = ["symbol", "ex_date", "pay_date", "record_date", "declared_date", "amount", "currency", "is_foreign"]

# The columns of the actions a backtest can't book, read from the broker's
# corporate_action table: an Alpaca stock dividend, spin-off or merger.
CORPORATE_ACTION_COLUMNS = ["symbol", "kind", "ex_date"]

# What each corporate action kind is called in a refusal.
CORPORATE_ACTION_WORDS = {
    "stock_dividend": "stock dividend",
    "spin_off": "spin-off",
    "cash_merger": "cash merger",
    "stock_merger": "stock merger",
    "stock_and_cash_merger": "stock and cash merger",
}

# Rule 6.2: cash dividends on stooq bars would count each dividend twice.
# No user flow reaches it: the service never hands a stooq ingest dividends,
# and a stooq bundle reads none, so it means foreverbull itself went wrong.
STOOQ_CASH_DIVIDENDS = (
    "foreverbull has cash dividends stored for stooq, whose prices already include them, so they'd be counted twice"
)

# The one currency a dividend is booked in: the bars and the account are USD.
DIVIDEND_CURRENCY = "USD"

# The table foreverbull adds to Zipline's adjustments database: which
# booked dividends are of a foreign issuer, so a run can count them.
FOREIGN_DIVIDEND_TABLE = "foreverbull_foreign_dividend"


def no_dividends() -> pd.DataFrame:
    """An empty dividend list, for an ingest whose caller knows there are none."""
    return pd.DataFrame(columns=pd.Index(DIVIDEND_COLUMNS))


def no_corporate_actions() -> pd.DataFrame:
    """An empty list of actions a backtest can't book."""
    return pd.DataFrame(columns=pd.Index(CORPORATE_ACTION_COLUMNS))


def stooq_cash_dividends_line(backtest: str) -> str:
    """Ingest's failure when a stooq backtest is asked to book cash dividends."""
    return (
        f"Couldn't load data for backtest '{backtest}': {STOOQ_CASH_DIVIDENDS}. The backtest wasn't saved. "
        "Please report it at https://github.com/lhjnilsson/foreverbull/issues with this message."
    )


def books_cash_dividends(provider: str) -> bool:
    """Whether a provider's bundle books cash dividends.

    yahoo and alpaca bars are split-adjusted only, so dividends are cash.
    stooq bars already include them (an adjusted-close return), so cash
    would count them twice.
    """
    match provider:
        case "yahoo" | "alpaca":
            return True
        case "stooq":
            return False
        case _:
            raise ValueError(f"unknown price provider {provider!r}")


@dataclass(frozen=True)
class DividendProblem:
    provider: str
    symbol: str
    ex_date: date | None
    reason: str
    # A corporate action a backtest can't book, rather than a dividend.
    action: bool = False
    # The stored dividends don't cover the stored prices, so loading them
    # again fixes it. Every other dividend problem is in the provider's data.
    reload: bool = False

    def line(self) -> str:
        day = f" ex-date {self.ex_date:%Y-%m-%d}" if self.ex_date is not None else ""
        return f"  {self.provider} {self.symbol}{day}: {self.reason}."


# The step when the stored dividends don't cover the stored prices.
RELOAD_DIVIDENDS_STEP = (
    "Run `fbull backtest create` again to load the missing dividends, or use another provider with --provider."
)
# The step when the provider sent a dividend that can't be booked; loading it
# again gets the same dividend. It matches the broker's DividendError step.
BAD_DIVIDENDS_STEP = (
    "Use another provider with --provider, or report it at "
    "https://github.com/lhjnilsson/foreverbull/issues with this message."
)


# The step when some symbols' dividends weren't loaded and others came wrong
# from the provider.
MIXED_DIVIDENDS_STEP = (
    "Run `fbull backtest create` again to load the missing dividends. For the others, use another provider "
    "with --provider, or report it at https://github.com/lhjnilsson/foreverbull/issues with this message."
)


def _counted(count: int, noun: str) -> str:
    return f"1 {noun}" if count == 1 else f"{count} {noun}s"


def _problems_lead(problems: list[DividendProblem]) -> str:
    """What can't be booked: dividends counted by symbol, corporate actions one by one.

    A not-loaded symbol has no dividends to count, so dividends are counted
    by symbol whatever the reason.
    """
    actions = sum(1 for problem in problems if problem.action)
    dividends = [problem for problem in problems if not problem.action]
    symbols = _counted(len({problem.symbol for problem in dividends}), "symbol")
    counted = _counted(actions, "corporate action")
    if dividends and all(problem.reload for problem in dividends):
        lead = f"dividends for {symbols} weren't loaded"
        return f"{lead} and {counted} can't be booked" if actions else lead
    if dividends and actions:
        return f"dividends for {symbols} and {counted} can't be booked"
    if dividends:
        return f"dividends for {symbols} can't be booked"
    return f"{counted} can't be booked"


def dividend_problems_line(backtest: str, problems: list[DividendProblem]) -> str:
    """Ingest's failure when one or more dividends or corporate actions can't be booked."""
    actions = any(problem.action for problem in problems)
    lines = [f"Couldn't load data for backtest '{backtest}': {_problems_lead(problems)}. The backtest wasn't saved."]
    lines.extend(problem.line() for problem in problems)
    steps = []
    reload = any(problem.reload for problem in problems)
    bad = any(not problem.action and not problem.reload for problem in problems)
    if reload and bad:
        steps.append(MIXED_DIVIDENDS_STEP)
    elif reload:
        steps.append(RELOAD_DIVIDENDS_STEP)
    elif bad:
        steps.append(BAD_DIVIDENDS_STEP)
    if actions:
        steps.append(
            "Change the backtest dates to leave out the corporate action, then run `fbull backtest create` again."
        )
    lines.append(" ".join(steps))
    return "\n".join(lines)


def corporate_action_problems(
    provider: str,
    symbols: Iterable[str],
    actions: pd.DataFrame,
    from_date: date,
    to_date: date,
) -> list[DividendProblem]:
    """Every action a backtest can't book with an ex-date inside its window.

    A stock dividend, spin-off or merger would change the position in a
    way the bundle can't express, so a window that holds one fails. One
    outside the window, such as after its end, changes nothing in the run.
    """
    wanted = set(symbols)
    problems = []
    for record in cast(list[dict[str, Any]], actions.to_dict("records")):
        symbol = str(record["symbol"])
        ex_date = _as_timestamp(record.get("ex_date"))
        if symbol not in wanted or ex_date is None or not from_date <= ex_date.date() <= to_date:
            continue
        kind = str(record["kind"])
        words = CORPORATE_ACTION_WORDS.get(kind, kind)
        problems.append(
            DividendProblem(provider, symbol, ex_date.date(), f"a {words}, which foreverbull can't book", action=True)
        )
    return sorted(problems, key=lambda problem: (problem.ex_date or date.min, problem.symbol))


@dataclass(frozen=True)
class SidBars:
    """One sid's bundle bars and the tickers whose dividends are its own."""

    sid: int
    symbols: tuple[str, ...]
    # Closes on session labels, oldest first.
    closes: pd.Series


@dataclass(frozen=True)
class BundleDividends:
    """The frame Zipline's adjustment writer takes, and the foreign ones."""

    frame: pd.DataFrame
    foreign: pd.DataFrame


def empty_dividends() -> BundleDividends:
    return BundleDividends(frame=_zipline_frame([]), foreign=_foreign_frame([]))


def _as_timestamp(value: Any) -> pd.Timestamp | None:
    if value is None:
        return None
    if isinstance(value, float) and math.isnan(value):
        return None
    stamp = pd.Timestamp(value)
    if stamp is pd.NaT:
        return None
    stamp = cast(pd.Timestamp, stamp)
    if stamp.tzinfo is not None:
        stamp = stamp.tz_convert("UTC").tz_localize(None)
    return stamp.normalize()


def _currency(value: Any) -> str:
    text = "" if value is None or (isinstance(value, float) and math.isnan(value)) else str(value)
    return text.strip().upper() or DIVIDEND_CURRENCY


def build_bundle_dividends(
    provider: str,
    sids: Iterable[SidBars],
    dividends: pd.DataFrame,
    calendar,
) -> tuple[BundleDividends, list[DividendProblem]]:
    """Check each dividend of each sid and build the frames for the bundle.

    A sid's dividends are those of its tickers with an ex-date inside its
    bars. The ex-date must be a trading day after the sid's first bar with
    a bar on the trading day before it: Zipline earns on the shares held at
    that close and would skip the dividend without a word otherwise. The
    amount must be below that close, or Zipline drops its price ratio with
    only a log line. A dividend on the sid's first bar is not booked,
    because no position can be held at a close before it. The amount must
    be a positive number in USD. The pay date is not checked: a missing or
    odd one changes nothing, as the cash lands on the ex-date. Two tickers
    of one sid that disagree on one ex-date are refused.
    """
    rows: list[dict[str, Any]] = []
    foreign: list[dict[str, Any]] = []
    problems: list[DividendProblem] = []
    by_symbol = {symbol: frame for symbol, frame in dividends.groupby("symbol")} if not dividends.empty else {}
    for sid in sids:
        if sid.closes.empty:
            continue
        booked: dict[pd.Timestamp, tuple[str, float]] = {}
        for symbol in sid.symbols:
            frame = by_symbol.get(symbol)
            if frame is None:
                continue
            for record in cast(list[dict[str, Any]], frame.to_dict("records")):
                row, problem = _check_dividend(provider, symbol, record, sid.closes, calendar)
                if problem is not None:
                    problems.append(problem)
                    continue
                if row is None:
                    continue
                earlier = booked.get(row["ex_date"])
                if earlier is not None:
                    if not math.isclose(earlier[1], row["amount"], rel_tol=1e-9):
                        problems.append(
                            DividendProblem(
                                provider,
                                symbol,
                                row["ex_date"].date(),
                                f"{earlier[0]} and {symbol} are the same company but have different dividends on this ex-date",
                            )
                        )
                    continue
                booked[row["ex_date"]] = (symbol, row["amount"])
                is_foreign = bool(row.pop("is_foreign"))
                rows.append({"sid": sid.sid, **row})
                if is_foreign:
                    foreign.append({"sid": sid.sid, "ex_date": row["ex_date"]})
    return BundleDividends(frame=_zipline_frame(rows), foreign=_foreign_frame(foreign)), problems


def _check_dividend(
    provider: str,
    symbol: str,
    record: dict[str, Any],
    closes: pd.Series,
    calendar,
) -> tuple[dict[str, Any] | None, DividendProblem | None]:
    """One dividend as a bundle row, None when it is outside the sid's bars, or why it is refused."""
    ex_date = _as_timestamp(record.get("ex_date"))
    if ex_date is None:
        return None, DividendProblem(provider, symbol, None, "missing ex-date")
    sessions = pd.DatetimeIndex(closes.index)
    if ex_date < sessions[0] or ex_date > sessions[-1]:
        return None, None

    def refused(reason: str) -> tuple[None, DividendProblem]:
        return None, DividendProblem(provider, symbol, ex_date.date(), reason)

    amount = record.get("amount")
    if amount is None or not np.isfinite(float(amount)) or float(amount) <= 0:
        return refused("amount is 0, negative or not a number")
    currency = _currency(record.get("currency"))
    if currency != DIVIDEND_CURRENCY:
        return refused(f"currency {currency} is not USD")
    if not calendar.is_session(ex_date):
        return refused("the ex-date is not a trading day")
    if ex_date == sessions[0]:
        return None, None
    previous = calendar.previous_session(ex_date)
    if previous not in sessions:
        return refused("there is no stored price on the trading day before the ex-date")
    if float(amount) >= float(closes[previous]):
        return refused(
            f"the amount {float(amount):g} is at or above the {float(closes[previous]):g} close "
            "on the trading day before the ex-date"
        )
    return {
        "ex_date": ex_date,
        # Cash lands on the ex-date: Zipline pays on the session equal to
        # pay_date, so writing the ex-date credits it at that open.
        "pay_date": ex_date,
        "record_date": _as_timestamp(record.get("record_date")),
        "declared_date": _as_timestamp(record.get("declared_date")),
        "amount": float(amount),
        "is_foreign": bool(record.get("is_foreign")),
    }, None


def _zipline_frame(rows: list[dict[str, Any]]) -> pd.DataFrame:
    frame = pd.DataFrame(
        rows, columns=pd.Index(["sid", "ex_date", "declared_date", "record_date", "pay_date", "amount"])
    )
    frame = frame.astype({"sid": "int64", "amount": "float64"})
    for column in ("ex_date", "declared_date", "record_date", "pay_date"):
        frame[column] = pd.to_datetime(frame[column])
    return frame.sort_values(["sid", "ex_date"], ignore_index=True)


def _foreign_frame(rows: list[dict[str, Any]]) -> pd.DataFrame:
    frame = pd.DataFrame(rows, columns=pd.Index(["sid", "ex_date"])).astype({"sid": "int64"})
    frame["ex_date"] = pd.to_datetime(frame["ex_date"])
    return frame


def write_foreign_dividends(connection: sqlite3.Connection, foreign: pd.DataFrame) -> None:
    """Record the foreign dividends next to Zipline's dividend_payouts.

    Zipline's payouts table has no column for it, and a run counts the
    foreign dividends it booked gross.
    """
    connection.execute(
        f"CREATE TABLE IF NOT EXISTS {FOREIGN_DIVIDEND_TABLE} (sid INTEGER NOT NULL, ex_date TEXT NOT NULL)"
    )
    connection.executemany(
        f"INSERT INTO {FOREIGN_DIVIDEND_TABLE} (sid, ex_date) VALUES (?, ?)",
        [
            (int(sid), cast(pd.Timestamp, pd.Timestamp(ex_date)).strftime("%Y-%m-%d"))
            for sid, ex_date in zip(foreign["sid"], foreign["ex_date"], strict=True)
        ],
    )
    connection.commit()


def read_foreign_dividends(connection: sqlite3.Connection) -> set[tuple[int, pd.Timestamp]] | None:
    """The (sid, ex-date) pairs of foreign dividends in a bundle, or None.

    Every bundle ingested since dividends are booked has the table, empty
    or not. None is a bundle ingested before, whose returns would leave
    every dividend out.
    """
    exists = connection.execute(
        "SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?", (FOREIGN_DIVIDEND_TABLE,)
    ).fetchone()
    if exists is None:
        return None
    rows = connection.execute(f"SELECT sid, ex_date FROM {FOREIGN_DIVIDEND_TABLE}").fetchall()
    return {(int(sid), cast(pd.Timestamp, pd.Timestamp(ex_date))) for sid, ex_date in rows}


# Zipline's adjustment writer stores a missing date (NaT) as this integer.
_SQLITE_NO_DATE = np.iinfo(np.int64).min


def _from_sqlite_date(seconds: int | None) -> pd.Timestamp | None:
    if seconds is None or int(seconds) == _SQLITE_NO_DATE:
        return None
    return cast(pd.Timestamp, pd.Timestamp(int(seconds), unit="s"))


def read_bundle_dividends(connection: sqlite3.Connection) -> BundleDividends | None:
    """The dividends an ingest booked, ready to write into a new ingest.

    None is a bundle ingested before dividends were booked: it has none
    to carry, and a run refuses it.
    """
    foreign = read_foreign_dividends(connection)
    if foreign is None:
        return None
    rows = connection.execute(
        "SELECT sid, ex_date, declared_date, record_date, pay_date, amount FROM dividend_payouts"
    ).fetchall()
    frame = _zipline_frame(
        [
            {
                "sid": int(sid),
                "ex_date": _from_sqlite_date(ex_date),
                "declared_date": _from_sqlite_date(declared),
                "record_date": _from_sqlite_date(record),
                "pay_date": _from_sqlite_date(pay_date),
                "amount": float(amount),
            }
            for sid, ex_date, declared, record, pay_date, amount in rows
        ]
    )
    return BundleDividends(frame=frame, foreign=_foreign_frame([{"sid": sid, "ex_date": ex} for sid, ex in foreign]))
