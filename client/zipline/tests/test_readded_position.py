"""Golden run: a held member is dropped from the ETF and added back.

universe_member keeps one half-open row per membership interval, so a
member that is dropped and re-added has two rows. Zipline has one
auto_close_date per sid. It must follow the member's final interval: a
member that is back in the ETF at the end of the run is never
auto-closed, and one dropped for good closes on its final drop, not on
its first one.
"""

import os

from datetime import date
from datetime import datetime
from typing import cast

import pandas as pd
import pytest

from foreverbull_zipline.data_bundles.foreverbull import SQLIngester
from foreverbull_zipline.engine import zero_benchmark_returns
from zipline.data import bundles
from zipline.data.data_portal import DataPortal
from zipline.extensions import load
from zipline.finance import commission
from zipline.finance import metrics
from zipline.finance import slippage
from zipline.finance.blotter import Blotter
from zipline.finance.trading import SimulationParameters
from zipline.utils.calendar_utils import get_calendar

from foreverbull.pb.foreverbull import common_pb
from foreverbull.pb.foreverbull.backtest.v1 import backtest_pb
from zipline import TradingAlgorithm


BUNDLE = "readded_member_golden"
AAPL_ISIN = "US0378331005"
XYZ_ISIN = "US9999999999"
START = pd.Timestamp("2023-01-03")
END = pd.Timestamp("2023-03-31")
# The first interval ends at the January month-end list; the February
# month-end list adds XYZ back.
FIRST_DROP = date(2023, 1, 31)
READDED = date(2023, 2, 28)
# The final drop when XYZ leaves again inside the run.
FINAL_DROP = date(2023, 3, 15)
BUY_SESSION = pd.Timestamp("2023-01-04")
XYZ_SHARES = 100
AAPL_SHARES = 10


class _FakeEngine:
    def __init__(self, frames: dict[str, pd.DataFrame]):
        self.frames = frames

    def get_data(self, symbol, from_date, to_date):
        return self.frames[symbol].copy()


def _sessions() -> pd.DatetimeIndex:
    return get_calendar("XNYS").sessions_in_range(START, END)  # type: ignore[arg-type]


def _closes(sessions: pd.DatetimeIndex, first: float, step: float) -> pd.Series:
    return pd.Series([first + step * index for index in range(len(sessions))], index=sessions)


def _bars(closes: pd.Series) -> pd.DataFrame:
    days = pd.DatetimeIndex(closes.index)
    return pd.DataFrame(
        {
            "open": closes.to_list(),
            "high": closes.to_list(),
            "low": closes.to_list(),
            "close": closes.to_list(),
            "volume": [10_000_000] * len(closes),
            "time": [datetime(day.year, day.month, day.day) for day in days],
        }
    )


def _aliases(xyz_intervals: list[tuple[date, date | None]]) -> pd.DataFrame:
    """The load_aliases rows: one per asset_symbol row and membership interval.

    The query orders by ISIN and ticker start, not by membership, so the
    intervals of one ISIN can come back in any order.
    """
    rows = [(AAPL_ISIN, "AAPL", None)] + [(XYZ_ISIN, "XYZ", member_to) for _, member_to in xyz_intervals]
    return pd.DataFrame(
        {
            "isin": [isin for isin, _, _ in rows],
            "symbol": [symbol for _, symbol, _ in rows],
            "from_date": [date(2022, 12, 31)] * len(rows),
            "to_date": [None] * len(rows),
            "member_to": [member_to for _, _, member_to in rows],
        }
    )


def _ingester(sessions: pd.DatetimeIndex, xyz_intervals: list[tuple[date, date | None]]) -> SQLIngester:
    """AAPL a member throughout, XYZ a member in xyz_intervals; both trade on every session."""
    engine = _FakeEngine(
        {
            "AAPL": _bars(_closes(sessions, 50.0, 0.5)),
            "XYZ": _bars(_closes(sessions, 20.0, 0.25)),
            "OEF": _bars(_closes(sessions, 100.0, 1.0)),
        }
    )
    backtest = backtest_pb.Backtest(
        name="sp100",
        benchmark="OEF",
        periods=[
            backtest_pb.Backtest.Period(
                start_date=common_pb.Date(year=2023, month=1, day=3),
                end_date=common_pb.Date(year=2023, month=3, day=31),
                assets=[
                    backtest_pb.Backtest.Period.Asset(symbol="AAPL", enabled=True),
                    backtest_pb.Backtest.Period.Asset(symbol="XYZ", enabled=True),
                ],
            )
        ],
    )
    return SQLIngester(backtest, engine=engine, aliases=_aliases(xyz_intervals))


def _run(sessions: pd.DatetimeIndex) -> pd.DataFrame:
    """Buy AAPL and XYZ on the first session, then hold."""
    bundle = bundles.load(BUNDLE, os.environ, None)
    calendar = get_calendar("XNYS")
    data_portal = DataPortal(
        bundle.asset_finder,
        trading_calendar=calendar,
        first_trading_day=bundle.equity_minute_bar_reader.first_trading_day,
        equity_minute_reader=bundle.equity_minute_bar_reader,
        equity_daily_reader=bundle.equity_daily_bar_reader,
        adjustment_reader=bundle.adjustment_reader,
    )

    def initialize(context):
        context.set_slippage(slippage.FixedSlippage(spread=0.0))
        context.set_commission(commission.NoCommission())
        context.ordered = False

    def handle_data(context, data):
        if not context.ordered:
            context.order(context.symbol("AAPL"), AAPL_SHARES)
            context.order(context.symbol("XYZ"), XYZ_SHARES)
            context.ordered = True

    algorithm = TradingAlgorithm(
        namespace={},
        data_portal=data_portal,
        trading_calendar=calendar,
        sim_params=SimulationParameters(
            start_session=sessions[0],
            end_session=sessions[-1],
            trading_calendar=calendar,
            data_frequency="daily",
        ),
        metrics_set=metrics.load("default"),
        blotter=load(Blotter, "default"),
        benchmark_returns=zero_benchmark_returns(sessions),
        benchmark_sid=None,
        initialize=initialize,
        handle_data=handle_data,
    )
    result = algorithm.run()
    # Daily results are stamped at the UTC market close; key them by session.
    result.index = pd.DatetimeIndex([pd.Timestamp(stamp.date()) for stamp in pd.DatetimeIndex(result.index)])
    return result


def _ingest_and_run(xyz_intervals: list[tuple[date, date | None]]) -> tuple[pd.DataFrame, pd.DataFrame]:
    sessions = _sessions()
    ingester = _ingester(sessions, xyz_intervals)
    bundles.register(BUNDLE, ingester, calendar_name="XNYS")
    try:
        bundles.ingest(BUNDLE, os.environ, pd.Timestamp("2023-04-03", tz="UTC"), [], False)
        return pd.DataFrame(ingester._df_metadata), _run(sessions)
    finally:
        bundles.unregister(BUNDLE)


def _held(row) -> dict[str, int]:
    return {position["sid"].symbol: position["amount"] for position in row["positions"]}


def _xyz_fills(result: pd.DataFrame) -> list[tuple[pd.Timestamp, int, float]]:
    return [
        (cast(pd.Timestamp, day), fill["amount"], fill["price"])
        for day, fills in result["transactions"].items()
        for fill in fills
        if fill["sid"].symbol == "XYZ"
    ]


READDED_AND_HELD = [(date(2022, 12, 31), FIRST_DROP), (READDED, None)]


@pytest.mark.parametrize(
    "xyz_intervals",
    [READDED_AND_HELD, list(reversed(READDED_AND_HELD))],
    ids=["drop-first", "readd-first"],
)
def test_readded_member_is_not_auto_closed_at_its_first_drop(tmp_path, monkeypatch, xyz_intervals):
    monkeypatch.setenv("ZIPLINE_ROOT", str(tmp_path))
    metadata, result = _ingest_and_run(xyz_intervals)
    sessions = _sessions()

    rows = metadata.loc[metadata["symbol"] == "XYZ"]
    # One sid row, not one per membership interval.
    assert len(rows) == 1
    # Back in the ETF at the end: no auto-close inside the run.
    assert pd.Timestamp(rows.iloc[0]["auto_close_date"]) == END + pd.Timedelta(days=1)

    # The only XYZ fill is the buy: nothing sells it at the first drop.
    xyz_closes = _closes(sessions, 20.0, 0.25)
    assert _xyz_fills(result) == [(BUY_SESSION, XYZ_SHARES, pytest.approx(xyz_closes[BUY_SESSION]))]

    after_first_drop = sessions[sessions >= pd.Timestamp(FIRST_DROP)]
    for day in after_first_drop:
        assert _held(result.loc[day]) == {"AAPL": AAPL_SHARES, "XYZ": XYZ_SHARES}, day

    end = result.iloc[-1]
    aapl_closes = _closes(sessions, 50.0, 0.5)
    held_value = XYZ_SHARES * xyz_closes.iloc[-1] + AAPL_SHARES * aapl_closes.iloc[-1]
    assert end["portfolio_value"] == pytest.approx(end["ending_cash"] + held_value)


READDED_AND_DROPPED = [(date(2022, 12, 31), FIRST_DROP), (READDED, FINAL_DROP)]


@pytest.mark.parametrize(
    "xyz_intervals",
    [READDED_AND_DROPPED, list(reversed(READDED_AND_DROPPED))],
    ids=["drop-first", "readd-first"],
)
def test_readded_member_dropped_again_closes_at_its_final_drop(tmp_path, monkeypatch, xyz_intervals):
    monkeypatch.setenv("ZIPLINE_ROOT", str(tmp_path))
    metadata, result = _ingest_and_run(xyz_intervals)
    sessions = _sessions()

    rows = metadata.loc[metadata["symbol"] == "XYZ"]
    assert len(rows) == 1
    assert pd.Timestamp(rows.iloc[0]["auto_close_date"]) == pd.Timestamp(FINAL_DROP)

    held_through = sessions[(sessions >= pd.Timestamp(FIRST_DROP)) & (sessions < pd.Timestamp(FINAL_DROP))]
    for day in held_through:
        assert _held(result.loc[day]) == {"AAPL": AAPL_SHARES, "XYZ": XYZ_SHARES}, day
    assert _held(result.iloc[-1]) == {"AAPL": AAPL_SHARES}

    # Bought once. Zipline closes it at the end of the final-drop session,
    # the first one the broker leaves XYZ out of, at that session's close.
    xyz_closes = _closes(sessions, 20.0, 0.25)
    closed_on = pd.Timestamp(FINAL_DROP)
    assert _xyz_fills(result) == [
        (BUY_SESSION, XYZ_SHARES, pytest.approx(xyz_closes[BUY_SESSION])),
        (closed_on, -XYZ_SHARES, pytest.approx(xyz_closes[closed_on])),
    ]
    assert _held(result.loc[closed_on]) == {"AAPL": AAPL_SHARES}
    before = sessions[sessions < closed_on][-1]
    credited = result.loc[closed_on]["ending_cash"] - result.loc[before]["ending_cash"]
    assert credited == pytest.approx(XYZ_SHARES * xyz_closes[closed_on])
