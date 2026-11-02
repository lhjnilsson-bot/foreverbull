"""An order resolves a ticker as of the session it is placed in.

Ticker X belongs to ISIN A until a reuse date and to ISIN B afterwards
(A renamed away from X; B took the freed ticker). An order for X placed
before that date must fill on A. TradingAlgorithm.symbol resolves as of
the backtest's last session, so on main the same order fills on B.
"""

import logging
import os

from datetime import date

import pandas as pd
import pytest

from foreverbull_zipline.data_bundles.foreverbull import SQLIngester
from foreverbull_zipline.engine import Engine
from foreverbull_zipline.engine import zero_benchmark_returns
from protobuf import merge_from_binary
from zipline.data import bundles
from zipline.data.data_portal import DataPortal
from zipline.extensions import load
from zipline.finance import metrics
from zipline.finance.blotter import Blotter
from zipline.finance.trading import SimulationParameters
from zipline.utils.calendar_utils import get_calendar

from foreverbull.pb.foreverbull import common_pb
from foreverbull.pb.foreverbull.backtest.service.v1 import engine_pb as engine_service_pb
from foreverbull.pb.foreverbull.backtest.v1 import backtest_pb
from foreverbull.pb.foreverbull.finance.v1 import marketdata_pb
from zipline import TradingAlgorithm


ISIN_A = "US0000000001"
ISIN_B = "US0000000002"
BUNDLE = "ticker_reuse_order"
# A frees X on this date; B takes it. Half-open asset_symbol.to_date.
REUSE = date(2021, 10, 31)
SHARES = 10


class FakeEngine:
    def __init__(self, frames: dict[str, pd.DataFrame]):
        self.frames = frames

    def get_data(self, symbol, from_date, to_date):
        return self.frames[symbol].copy()


def _bars(days: list[pd.Timestamp], closes: list[float]) -> pd.DataFrame:
    return pd.DataFrame(
        {
            "open": closes,
            "high": [close + 1.0 for close in closes],
            "low": [close - 1.0 for close in closes],
            "close": closes,
            "volume": [1_000_000] * len(closes),
            "time": [day.to_pydatetime() for day in days],
        }
    )


def _backtest() -> backtest_pb.Backtest:
    return backtest_pb.Backtest(
        name=BUNDLE,
        benchmark="OEF",
        periods=[
            backtest_pb.Backtest.Period(
                start_date=common_pb.Date(year=2021, month=10, day=1),
                end_date=common_pb.Date(year=2021, month=11, day=30),
                assets=[
                    backtest_pb.Backtest.Period.Asset(symbol="X", enabled=True),
                    backtest_pb.Backtest.Period.Asset(symbol="Y", enabled=True),
                    backtest_pb.Backtest.Period.Asset(symbol="Z", enabled=True),
                ],
            )
        ],
    )


@pytest.fixture
def reuse_bundle(tmp_path, monkeypatch):
    monkeypatch.setenv("ZIPLINE_ROOT", str(tmp_path))
    sessions = get_calendar("XNYS").sessions_in_range(
        pd.Timestamp("2021-10-01"),  # type: ignore[arg-type]
        pd.Timestamp("2021-11-30"),  # type: ignore[arg-type]
    )
    days = list(sessions)
    before = [day for day in days if day.date() < REUSE]
    after = [day for day in days if day.date() >= REUSE]
    assert before and after

    # Distinct closes so a wrong sid is visible in the fill price.
    engine = FakeEngine(
        {
            "X": _bars(
                before + after,
                [100.0 + i for i in range(len(before))] + [400.0 + i for i in range(len(after))],
            ),
            "Y": _bars(after, [200.0 + i for i in range(len(after))]),
            "Z": _bars(before, [300.0 + i for i in range(len(before))]),
            "OEF": _bars(days, [50.0] * len(days)),
        }
    )
    # A: X then Y. B: Z then X (reuses the freed ticker).
    aliases = pd.DataFrame(
        {
            "isin": [ISIN_A, ISIN_A, ISIN_B, ISIN_B],
            "symbol": ["X", "Y", "Z", "X"],
            "from_date": [date(2021, 7, 1), REUSE, date(2021, 7, 1), REUSE],
            "to_date": [REUSE, None, REUSE, None],
            "member_to": [None, None, None, None],
        }
    )
    bundles.register(
        BUNDLE,
        SQLIngester(
            _backtest(),
            engine=engine,
            aliases=aliases,
            events=pd.DataFrame(columns=pd.Index(["symbol", "kind", "on_date", "evidence"])),
        ),
        calendar_name="XNYS",
    )
    try:
        bundles.ingest(BUNDLE, os.environ, pd.Timestamp("2021-12-01", tz="UTC"), [], False)
        yield sessions, before, after
    finally:
        bundles.unregister(BUNDLE)


def test_order_for_reused_ticker_fills_on_prior_isin(reuse_bundle):
    sessions, before, after = reuse_bundle
    bundle = bundles.load(BUNDLE, os.environ, None)
    calendar = get_calendar("XNYS")
    finder = bundle.asset_finder
    engine = Engine(socket_file_path="/tmp/foreverbull_zipline_reuse_order_test.sock")
    engine.log = logging.getLogger(__name__)

    sid_a = finder.lookup_symbol("X", as_of_date=before[0]).sid
    sid_b = finder.lookup_symbol("X", as_of_date=after[0]).sid
    assert sid_a != sid_b, "X maps to different ISINs before and after the reuse"
    assert finder.lookup_symbol("Y", as_of_date=after[0]).sid == sid_a
    assert finder.lookup_symbol("Z", as_of_date=before[0]).sid == sid_b
    # Last-session lookup is what TradingAlgorithm.symbol uses on main.
    assert finder.lookup_symbol("X", as_of_date=sessions[-1]).sid == sid_b

    filled_sid: list[int] = []

    def handle_data(context, data):
        today = context.get_datetime().tz_localize(None).normalize()
        if today == before[0]:
            order = marketdata_pb.Order(symbol="X", amount=SHARES)
            engine._place_orders(
                engine_service_pb.PlaceOrdersAndContinueRequest(orders=[order]).to_binary(),
                context,
            )
        if today == before[1] and not filled_sid:
            period = engine_service_pb.GetCurrentPeriodResponse()
            merge_from_binary(period, engine._get_current_period(b"", context))
            assert period.portfolio is not None
            assert len(period.portfolio.positions) == 1
            assert period.portfolio.positions[0].amount == SHARES
            held = sorted(int(position.sid) for position in context.portfolio.positions)
            assert len(held) == 1
            filled_sid.append(held[0])

    data_portal = DataPortal(
        finder,
        trading_calendar=calendar,
        first_trading_day=bundle.equity_minute_bar_reader.first_trading_day,
        equity_minute_reader=bundle.equity_minute_bar_reader,
        equity_daily_reader=bundle.equity_daily_bar_reader,
        adjustment_reader=bundle.adjustment_reader,
    )
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
        handle_data=handle_data,
    )
    algorithm.run()

    assert filled_sid == [sid_a], (
        f"order for X before reuse must fill on A ({sid_a}), not B ({sid_b}); got {filled_sid}"
    )
