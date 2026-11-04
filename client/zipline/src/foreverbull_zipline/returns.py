"""Total and price return with dividends as cash.

A dividend is credited to cash at the open of its ex-date session: the
shares held at the previous close times the amount, negative for a short.
The bundle writes the ex-date as Zipline's pay date, so there is never a
dividend owed and not yet paid.

For every session close t:

    V_t    = Zipline's portfolio value (cash + positions)
    Dcum_t = every dividend credited so far, signed
    E_t    = V_t - Dcum_t
    total return  TR_t = V_t / V_0 - 1, daily V_t / V_{t-1} - 1
    price return  PR_t = E_t / V_0 - 1

V_0 is the starting cash; there are no deposits or withdrawals. Volatility,
Sharpe, Sortino, drawdown, alpha and beta come from the daily total return
against the benchmark's daily total return, never from the price return.
"""

import math

from dataclasses import dataclass
from typing import Any
from typing import cast

import empyrical
import numpy as np
import pandas as pd

from zipline.finance import metrics


@dataclass(frozen=True)
class BenchmarkReturns:
    """The benchmark's daily total return (dividends paid out) and price return."""

    total: pd.Series
    price: pd.Series


def session_label(value) -> pd.Timestamp:
    """A timestamp as a naive session label at midnight."""
    stamp = cast(pd.Timestamp, pd.Timestamp(value))
    if stamp.tzinfo is not None:
        stamp = stamp.tz_convert("UTC").tz_localize(None)
    return stamp.normalize()


# The run's failure when Zipline still holds a dividend unpaid at a close. It
# fails the same way on every run, so the step is a report, not a retry.
UNPAID_DIVIDEND_LINE = (
    "The backtest run stopped: foreverbull didn't pay a dividend on its ex-date. "
    "This is a problem in foreverbull, not in your algo. "
    "Please report it at https://github.com/lhjnilsson/foreverbull/issues with this message."
)


class UnpaidDividendError(RuntimeError):
    """Zipline still holds a dividend unpaid at a session close.

    The text names the amount and the session for the log. The user sees
    UNPAID_DIVIDEND_LINE instead.
    """


class DividendBook:
    """A Zipline metric: dividend cash credited by each session close.

    It runs after the ledger has credited the session's dividends at the
    open. Every dividend is paid on its ex-date, so a dividend Zipline
    still holds as unpaid at a close fails the run.
    """

    def __init__(self, foreign: set[tuple[int, pd.Timestamp]]):
        self._foreign = foreign
        self.earned = 0.0
        self.foreign_booked = 0

    def start_of_simulation(self, ledger, emission_rate, trading_calendar, sessions, benchmark_source):
        self.earned = 0.0
        self.foreign_booked = 0

    def start_of_session(self, ledger, session, data_portal):
        day = session_label(session)
        positions = ledger.position_tracker.positions
        if positions and data_portal.adjustment_reader is not None:
            for dividend in data_portal.adjustment_reader.get_dividends_with_ex_date(
                set(positions), session, data_portal.asset_finder
            ):
                self.earned += float(positions[dividend.asset].amount) * float(dividend.amount)
                if (int(dividend.asset.sid), day) in self._foreign:
                    self.foreign_booked += 1

    def end_of_session(self, packet, ledger, session, session_ix, data_portal):
        unpaid = sum(
            float(payment["amount"])
            for payments in ledger.position_tracker._unpaid_dividends.values()
            for payment in payments
        )
        if unpaid != 0.0:
            raise UnpaidDividendError(
                f"dividends of {unpaid:.6f} were not paid on their ex-date by {session:%Y-%m-%d}: "
                "the bundle's pay date must be the ex-date"
            )
        packet["daily_perf"]["dividends_earned"] = self.earned


def dividend_metrics(foreign: set[tuple[int, pd.Timestamp]]) -> tuple[set[Any], DividendBook]:
    """Zipline's default metrics with the dividend book, and the book."""
    book = DividendBook(foreign)
    metrics_set = metrics.load("default")
    metrics_set.add(book)
    return metrics_set, book


def _finite(value) -> float | None:
    if value is None:
        return None
    number = float(value)
    return number if math.isfinite(number) else None


def total_return_frame(result: pd.DataFrame, capital_base: float, benchmark: BenchmarkReturns) -> pd.DataFrame:
    """Per session close: value, total and price return and the risk metrics from them.

    ``result`` is Zipline's run result with the DividendBook columns, and
    ``benchmark`` is on the same sessions.
    """
    if len(benchmark.total) != len(result):
        raise ValueError(f"benchmark has {len(benchmark.total)} sessions and the run has {len(result)}")
    earned = result["dividends_earned"].to_numpy(dtype=float)
    value = result["portfolio_value"].to_numpy(dtype=float)
    excluding_dividends = value - earned
    previous = np.concatenate([[capital_base], value[:-1]])
    daily = value / previous - 1.0
    bench_total = benchmark.total.to_numpy(dtype=float)
    bench_price = benchmark.price.to_numpy(dtype=float)

    rows: list[dict[str, Any]] = []
    for i in range(len(result)):
        returns = daily[: i + 1]
        alpha, beta = empyrical.alpha_beta_aligned(returns, bench_total[: i + 1])
        bench_window = bench_total[: i + 1]
        rows.append(
            {
                "portfolio_value": value[i],
                "pnl": value[i] - previous[i],
                "returns": daily[i],
                "algorithm_period_return": value[i] / capital_base - 1.0,
                "price_return": excluding_dividends[i] / capital_base - 1.0,
                "dividends_earned": earned[i],
                "algo_volatility": _finite(empyrical.annual_volatility(returns)),
                "sharpe": _finite(empyrical.sharpe_ratio(returns)),
                "sortino": _finite(empyrical.sortino_ratio(returns)),
                "max_drawdown": _finite(empyrical.max_drawdown(returns)),
                "alpha": _finite(alpha),
                "beta": None if np.isnan(beta) else float(beta),
                "benchmark_period_return": float(np.prod(1.0 + bench_window) - 1.0),
                "benchmark_price_return": float(np.prod(1.0 + bench_price[: i + 1]) - 1.0),
                "benchmark_volatility": (_finite(np.std(bench_window, ddof=1) * np.sqrt(252)) if i >= 1 else None),
            }
        )
    return pd.DataFrame(rows, index=result.index)
