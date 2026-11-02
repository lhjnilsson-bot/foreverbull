"""A held position survives a ticker rename (FB then META, one ISIN).

The bundle is built by SQLIngester from the asset_symbol rows of a backtest
over the rename, and a real TradingAlgorithm runs on it. Orders and the
portfolio go through the same Engine methods the broker session calls, so
the symbols asserted here are the ones user code sends and receives.
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


META_ISIN = "US30303M1027"
BUNDLE = "fb_meta_rename"
# iShares lists META from the 2021-10-31 as-of. The last FB bar is the
# session before it, the first META bar the session after it.
RENAME = pd.Timestamp("2021-10-31")
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
                    backtest_pb.Backtest.Period.Asset(symbol="FB", enabled=True),
                    backtest_pb.Backtest.Period.Asset(symbol="META", enabled=True),
                ],
            )
        ],
    )


@pytest.fixture
def rename_bundle(tmp_path, monkeypatch):
    monkeypatch.setenv("ZIPLINE_ROOT", str(tmp_path))
    sessions = get_calendar("XNYS").sessions_in_range(
        pd.Timestamp("2021-10-01"),  # type: ignore[arg-type]
        pd.Timestamp("2021-11-30"),  # type: ignore[arg-type]
    )
    days = list(sessions)
    closes = [100.0 + index for index in range(len(days))]
    fb = [(day, close) for day, close in zip(days, closes, strict=True) if day < RENAME]
    meta = [(day, close) for day, close in zip(days, closes, strict=True) if day >= RENAME]
    engine = FakeEngine(
        {
            "FB": _bars([day for day, _ in fb], [close for _, close in fb]),
            "META": _bars([day for day, _ in meta], [close for _, close in meta]),
            "OEF": _bars(days, [50.0] * len(days)),
        }
    )
    # asset_symbol after the fix: FB up to the rename, META from it.
    aliases = pd.DataFrame(
        {
            "isin": [META_ISIN, META_ISIN],
            "symbol": ["FB", "META"],
            "from_date": [date(2021, 7, 1), date(2021, 10, 31)],
            "to_date": [date(2021, 10, 31), None],
            "member_to": [None, None],
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
        yield sessions, closes
    finally:
        bundles.unregister(BUNDLE)


def test_held_position_survives_ticker_rename(rename_bundle):
    sessions, closes = rename_bundle
    bundle = bundles.load(BUNDLE, os.environ, None)
    calendar = get_calendar("XNYS")
    engine = Engine(socket_file_path="/tmp/foreverbull_zipline_rename_test.sock")
    engine.log = logging.getLogger(__name__)

    sids = {bundle.asset_finder.lookup_symbol(symbol, as_of_date=None).sid for symbol in ("FB", "META")}
    assert len(sids) == 1, "FB and META are one Zipline asset"
    sid = sids.pop()

    seen: list[dict] = []

    def handle_data(context, data):
        today = context.get_datetime().tz_localize(None).normalize()
        if not seen:
            # User code buys FB, the ticker the universe lists before the rename.
            order = marketdata_pb.Order(symbol="FB", amount=SHARES)
            engine._place_orders(engine_service_pb.PlaceOrdersAndContinueRequest(orders=[order]).to_binary(), context)
        period = engine_service_pb.GetCurrentPeriodResponse()
        merge_from_binary(period, engine._get_current_period(b"", context))
        assert period.portfolio is not None
        asset = context.sid(sid)
        seen.append(
            {
                "day": today,
                "positions": [(p.symbol, p.amount, p.cost_basis) for p in period.portfolio.positions],
                "sids": sorted(position.sid for position in context.portfolio.positions),
                "price": float(data.current(asset, "price")),
                "can_trade": bool(data.can_trade(asset)),
            }
        )

    data_portal = DataPortal(
        bundle.asset_finder,
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
    result = algorithm.run()

    assert [row["day"] for row in seen] == list(sessions)
    held = seen[1:]  # the order on the first session fills on the next one
    cost_basis = held[0]["positions"][0][2]
    assert cost_basis == pytest.approx(closes[1], rel=0.01)  # next close plus slippage
    for row in held:
        assert row["sids"] == [sid], f"one asset on {row['day']:%Y-%m-%d}"
        assert len(row["positions"]) == 1
        _, amount, basis = row["positions"][0]
        assert amount == SHARES, f"shares on {row['day']:%Y-%m-%d}"
        assert basis == pytest.approx(cost_basis), f"cost basis on {row['day']:%Y-%m-%d}"
        assert row["can_trade"], f"tradable on {row['day']:%Y-%m-%d}"

    # One continuous price history across the rename: no gap, no jump.
    assert [row["price"] for row in seen] == pytest.approx(closes)
    window = data_portal.get_history_window(
        [bundle.asset_finder.retrieve_asset(sid)], sessions[-1], len(sessions), "1d", "close", "daily"
    )
    assert window.iloc[:, 0].tolist() == pytest.approx(closes)

    # The position is not closed at the rename: no sell, one buy in total.
    transactions = [txn for day in result["transactions"] for txn in day]
    assert [txn["amount"] for txn in transactions] == [SHARES]

    # User code sees the position under the universe's ticker of the day:
    # FB before the rename, META from it.
    before = [row for row in held if row["day"] < RENAME]
    after = [row for row in held if row["day"] >= RENAME]
    assert before and after
    assert {row["positions"][0][0] for row in before} == {"FB"}
    assert {row["positions"][0][0] for row in after} == {"META"}
