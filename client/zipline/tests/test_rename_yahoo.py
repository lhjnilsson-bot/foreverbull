"""FB then META with the bars Yahoo actually serves.

Yahoo has no bars under FB for any date: its chart endpoint answers "Data
doesn't exist" for FB, and serves the whole history, before the rename
too, under META. So the broker stores no FB rows in ohlc. The META and
OEF files under testdata are Yahoo's daily chart response for
2021-10-01 to 2021-11-30, stamped at the session open in UTC as Yahoo
returns them.

asset_symbol still says FB up to the rename and META from it, and that
is the ticker the universe hands user code on each date.
"""

import logging
import os

from datetime import date
from pathlib import Path

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


TESTDATA = Path(__file__).parent / "testdata"
META_ISIN = "US30303M1027"
BUNDLE = "fb_meta_rename_yahoo"
RENAME = date(2021, 10, 31)
SHARES = 10


def _yahoo_bars(symbol: str) -> pd.DataFrame:
    frame = pd.read_csv(TESTDATA / f"yahoo_{symbol.lower()}_2021-10-01_2021-11-30.csv")
    frame["time"] = pd.to_datetime(frame["time"], utc=True)
    return frame


class YahooOHLC:
    """The ohlc rows the broker stores from Yahoo: none under FB."""

    def __init__(self):
        empty = _yahoo_bars("META").iloc[0:0]
        self.frames = {"FB": empty, "META": _yahoo_bars("META"), "OEF": _yahoo_bars("OEF")}

    def get_data(self, symbol, from_date, to_date):
        frame = self.frames[symbol]
        days = frame["time"].dt.date
        return frame[(days >= from_date) & (days <= to_date)].copy()


ALIASES = pd.DataFrame(
    {
        "isin": [META_ISIN, META_ISIN],
        "symbol": ["FB", "META"],
        "from_date": [date(2021, 7, 1), RENAME],
        "to_date": [RENAME, None],
        "member_to": [None, None],
    }
)


def universe_ticker(day: date) -> str:
    """The ticker asset_symbol gives the ISIN on day."""
    return "FB" if day < RENAME else "META"


def _backtest() -> backtest_pb.Backtest:
    return backtest_pb.Backtest(
        name=BUNDLE,
        benchmark="OEF",
        periods=[
            backtest_pb.Backtest.Period(
                start_date=common_pb.Date(year=2021, month=10, day=1),
                end_date=common_pb.Date(year=2021, month=11, day=30),
                assets=[
                    # The FB download from Yahoo fails, so the broker disables it.
                    backtest_pb.Backtest.Period.Asset(symbol="FB", enabled=False),
                    backtest_pb.Backtest.Period.Asset(symbol="META", enabled=True),
                ],
            )
        ],
    )


@pytest.fixture
def yahoo_bundle(tmp_path, monkeypatch):
    monkeypatch.setenv("ZIPLINE_ROOT", str(tmp_path))
    sessions = get_calendar("XNYS").sessions_in_range(
        pd.Timestamp("2021-10-01"),  # type: ignore[arg-type]
        pd.Timestamp("2021-11-30"),  # type: ignore[arg-type]
    )
    bundles.register(
        BUNDLE,
        SQLIngester(
            _backtest(),
            engine=YahooOHLC(),
            aliases=ALIASES,
            events=pd.DataFrame(columns=pd.Index(["symbol", "kind", "on_date", "evidence"])),
        ),
        calendar_name="XNYS",
    )
    try:
        bundles.ingest(BUNDLE, os.environ, pd.Timestamp("2021-12-01", tz="UTC"), [], False)
        yield bundles.load(BUNDLE, os.environ, None), sessions
    finally:
        bundles.unregister(BUNDLE)


def _run(bundle, sessions, on_session):
    """Run a TradingAlgorithm on the bundle. on_session(engine, context, data, day) is user code."""
    calendar = get_calendar("XNYS")
    engine = Engine(socket_file_path="/tmp/foreverbull_zipline_rename_yahoo_test.sock")
    engine.log = logging.getLogger(__name__)

    def handle_data(context, data):
        on_session(engine, context, data, context.get_datetime().tz_localize(None).normalize())

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
    return algorithm.run()


def _order(engine: Engine, context, symbol: str, amount: int) -> None:
    order = marketdata_pb.Order(symbol=symbol, amount=amount)
    engine._place_orders(engine_service_pb.PlaceOrdersAndContinueRequest(orders=[order]).to_binary(), context)


def _positions(engine: Engine, context) -> list[tuple[str, int]]:
    period = engine_service_pb.GetCurrentPeriodResponse()
    merge_from_binary(period, engine._get_current_period(b"", context))
    assert period.portfolio is not None
    return [(position.symbol, position.amount) for position in period.portfolio.positions]


def test_yahoo_bars_cover_every_session_and_an_fb_order_fills(yahoo_bundle):
    bundle, sessions = yahoo_bundle
    finder = bundle.asset_finder
    before = pd.Timestamp("2021-10-01")
    sid = finder.lookup_symbol("META", as_of_date=pd.Timestamp("2021-11-30")).sid
    assert finder.lookup_symbol("FB", as_of_date=before).sid == sid, "FB before the rename is the META asset"

    meta = _yahoo_bars("META")
    closes = dict(zip(pd.DatetimeIndex(meta["time"].dt.date), meta["close"], strict=True))
    seen: list[tuple[pd.Timestamp, float, bool]] = []

    def on_session(engine, context, data, day):
        if not seen:
            # User code orders the ticker the universe lists on this date.
            _order(engine, context, universe_ticker(day.date()), SHARES)
        asset = context.sid(sid)
        seen.append((day, float(data.current(asset, "close")), bool(data.can_trade(asset))))

    result = _run(bundle, sessions, on_session)

    assert [day for day, _, _ in seen] == list(sessions)
    for day, close, can_trade in seen:
        assert close == pytest.approx(closes[day]), f"Yahoo close on {day:%Y-%m-%d}"
        assert can_trade, f"tradable on {day:%Y-%m-%d}"

    transactions = [txn for day in result["transactions"] for txn in day]
    assert [(txn["sid"].sid, txn["amount"]) for txn in transactions] == [(sid, SHARES)]
    assert transactions[0]["dt"].normalize().tz_localize(None) == sessions[1]


def test_held_position_is_named_by_the_universe_ticker_of_the_day(yahoo_bundle):
    bundle, sessions = yahoo_bundle
    names: list[tuple[date, list[tuple[str, int]]]] = []

    def on_session(engine, context, data, day):
        if not names:
            _order(engine, context, universe_ticker(day.date()), SHARES)
        names.append((day.date(), _positions(engine, context)))

    _run(bundle, sessions, on_session)

    held = names[1:]  # the order on the first session fills on the next one
    assert any(day < RENAME for day, _ in held) and any(day >= RENAME for day, _ in held)
    for day, positions in held:
        assert positions == [(universe_ticker(day), SHARES)], f"position name on {day:%Y-%m-%d}"


# --- pre-rename window (internal #43) -----------------------------------------
# A backtest that ends before 2021-10-31 only lists FB. Yahoo still has no FB
# bars; the history lives under META. After download records META as a later
# alias of the same ISIN, SQLIngester must fill FB's sessions from META.


PRE_RENAME_BUNDLE = "fb_meta_pre_rename_yahoo"
PRE_RENAME_END = date(2021, 10, 29)


def _pre_rename_backtest() -> backtest_pb.Backtest:
    return backtest_pb.Backtest(
        name=PRE_RENAME_BUNDLE,
        benchmark="OEF",
        periods=[
            backtest_pb.Backtest.Period(
                start_date=common_pb.Date(year=2021, month=10, day=1),
                end_date=common_pb.Date(year=2021, month=10, day=29),
                assets=[
                    backtest_pb.Backtest.Period.Asset(symbol="FB", enabled=True),
                ],
            )
        ],
    )


# FB covers the whole pre-rename window; META is only a later alias so the
# ingester can read the bars Yahoo stored under that ticker.
PRE_RENAME_ALIASES = pd.DataFrame(
    {
        "isin": [META_ISIN, META_ISIN],
        "symbol": ["FB", "META"],
        "from_date": [date(2021, 7, 1), date(2021, 10, 30)],
        "to_date": [date(2021, 10, 30), None],
        "member_to": [None, None],
    }
)


@pytest.fixture
def pre_rename_yahoo_bundle(tmp_path, monkeypatch):
    monkeypatch.setenv("ZIPLINE_ROOT", str(tmp_path))
    sessions = get_calendar("XNYS").sessions_in_range(
        pd.Timestamp("2021-10-01"),  # type: ignore[arg-type]
        pd.Timestamp("2021-10-29"),  # type: ignore[arg-type]
    )
    bundles.register(
        PRE_RENAME_BUNDLE,
        SQLIngester(
            _pre_rename_backtest(),
            engine=YahooOHLC(),
            aliases=PRE_RENAME_ALIASES,
            events=pd.DataFrame(columns=pd.Index(["symbol", "kind", "on_date", "evidence"])),
        ),
        calendar_name="XNYS",
    )
    try:
        bundles.ingest(PRE_RENAME_BUNDLE, os.environ, pd.Timestamp("2021-11-01", tz="UTC"), [], False)
        yield bundles.load(PRE_RENAME_BUNDLE, os.environ, None), sessions
    finally:
        bundles.unregister(PRE_RENAME_BUNDLE)


def test_pre_rename_backtest_gets_fb_bars_from_meta_only_source(pre_rename_yahoo_bundle):
    """ETF window ending before the rename still trades FB on Yahoo's META bars."""
    bundle, sessions = pre_rename_yahoo_bundle
    finder = bundle.asset_finder
    as_of = pd.Timestamp("2021-10-15")
    sid = finder.lookup_symbol("FB", as_of_date=as_of).sid

    meta = _yahoo_bars("META")
    closes = {
        day.date() if hasattr(day, "date") else day: c
        for day, c in zip(meta["time"].dt.tz_convert("UTC").dt.normalize(), meta["close"], strict=True)
        if day.date() <= PRE_RENAME_END
    }
    seen: list[tuple[pd.Timestamp, float, bool]] = []

    def on_session(engine, context, data, day):
        if not seen:
            _order(engine, context, "FB", SHARES)
        asset = context.sid(sid)
        seen.append((day, float(data.current(asset, "close")), bool(data.can_trade(asset))))

    result = _run(bundle, sessions, on_session)

    assert [day for day, _, _ in seen] == list(sessions)
    for day, close, can_trade in seen:
        assert close == pytest.approx(closes[day.date() if hasattr(day, "date") else day]), (
            f"Yahoo close on {day:%Y-%m-%d}"
        )
        assert can_trade, f"tradable on {day:%Y-%m-%d}"

    transactions = [txn for day in result["transactions"] for txn in day]
    assert [(txn["sid"].sid, txn["amount"]) for txn in transactions] == [(sid, SHARES)]
