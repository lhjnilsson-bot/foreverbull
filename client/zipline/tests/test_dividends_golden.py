"""Worked examples of dividends as cash, to check by hand.

KO bars are Yahoo's daily chart for 2024-02-28 to 2024-07-31, recorded on
2026-11-04: split-adjusted only, so a dividend is not inside the prices.
KO paid two dividends in the window:

    ex-date      pay-date     per share
    2024-03-14   2024-04-01   $0.485
    2024-06-14   2024-07-01   $0.485

Each is credited to cash at the open of its ex-date; the pay date is not
used.

The strategy starts with $6,002.00, buys 100 KO on 2024-02-28 and the
order fills at the 2024-02-29 close of $60.02, with no slippage or
commission, so all the cash is in KO and V_0 = 6,002.00. Each dividend is
100 x 0.485 = $48.50.

Closes used below: 03-13 61.12, 03-14 60.50, 06-13 62.99, 06-14 62.55,
06-28 63.65, 07-31 66.74.
"""

import logging
import os

from pathlib import Path
from typing import Any
from typing import cast

import pandas as pd
import pytest

from foreverbull_zipline.data_bundles.dividends import DIVIDEND_COLUMNS
from foreverbull_zipline.data_bundles.dividends import read_foreign_dividends
from foreverbull_zipline.data_bundles.foreverbull import SQLIngester
from foreverbull_zipline.engine import Engine
from foreverbull_zipline.engine import zero_benchmark
from foreverbull_zipline.engine import zero_benchmark_returns
from foreverbull_zipline.returns import dividend_metrics
from foreverbull_zipline.returns import total_return_frame
from protobuf import merge_from_binary
from zipline.api import set_commission
from zipline.api import set_slippage
from zipline.data import bundles
from zipline.data.data_portal import DataPortal
from zipline.extensions import load
from zipline.finance.blotter import Blotter
from zipline.finance.commission import PerShare
from zipline.finance.slippage import FixedSlippage
from zipline.finance.trading import SimulationParameters
from zipline.utils.calendar_utils import get_calendar

from foreverbull.pb.foreverbull import common_pb
from foreverbull.pb.foreverbull.backtest.service.v1 import engine_pb as engine_service_pb
from foreverbull.pb.foreverbull.backtest.v1 import backtest_pb
from zipline import TradingAlgorithm


TESTDATA = Path(__file__).parent / "testdata"
START_CASH = 6_002.00
SHARES = 100
EX_1 = cast(pd.Timestamp, pd.Timestamp("2024-03-14"))
PAY_1 = cast(pd.Timestamp, pd.Timestamp("2024-04-01"))
EX_2 = cast(pd.Timestamp, pd.Timestamp("2024-06-14"))
PAY_2 = cast(pd.Timestamp, pd.Timestamp("2024-07-01"))


class OHLCRows:
    def __init__(self, frames: dict[str, pd.DataFrame]):
        self.frames = frames

    def get_data(self, symbol, from_date, to_date):
        frame = self.frames[symbol]
        days = frame["time"].dt.date
        return frame[(days >= from_date) & (days <= to_date)].copy()


def _ko_bars() -> pd.DataFrame:
    frame = pd.read_csv(TESTDATA / "yahoo_ko_2024-02-28_2024-07-31.csv")
    frame["time"] = pd.to_datetime(frame["time"], utc=True)
    return frame


def _ko_dividends(*, pay_dates: bool = True, foreign: bool = False) -> pd.DataFrame:
    return pd.DataFrame(
        [
            {
                "symbol": "KO",
                "ex_date": ex_date,
                "pay_date": pay_date if pay_dates else None,
                "record_date": None,
                "declared_date": None,
                "amount": 0.485,
                "currency": "USD",
                "is_foreign": foreign,
            }
            for ex_date, pay_date in ((EX_1, PAY_1), (EX_2, PAY_2))
        ],
        columns=pd.Index(DIVIDEND_COLUMNS),
    )


def _backtest(name: str) -> backtest_pb.Backtest:
    return backtest_pb.Backtest(
        name=name,
        provider="yahoo",
        periods=[
            backtest_pb.Backtest.Period(
                start_date=common_pb.Date(year=2024, month=2, day=28),
                end_date=common_pb.Date(year=2024, month=7, day=31),
                assets=[backtest_pb.Backtest.Period.Asset(symbol="KO", enabled=True)],
            )
        ],
    )


def _ingest(name: str, dividends: pd.DataFrame):
    ingester = SQLIngester(
        _backtest(name),
        engine=OHLCRows({"KO": _ko_bars()}),
        aliases=pd.DataFrame(),
        events=pd.DataFrame(columns=pd.Index(["symbol", "kind", "on_date", "evidence"])),
        dividends=dividends,
    )
    bundles.register(name, ingester, calendar_name="XNYS")
    try:
        bundles.ingest(name, os.environ, pd.Timestamp("2024-08-01", tz="UTC"), [], False)
        return bundles.load(name, os.environ, None)
    finally:
        bundles.unregister(name)


def _run(
    bundle,
    shares: int = SHARES,
    end: str = "2024-07-31",
    commission: float = 0.0,
    min_commission: float = 0.0,
    spread: float = 0.0,
) -> tuple[pd.DataFrame, pd.DataFrame, dict[pd.Timestamp, Any], Any]:
    """Run buy and hold; return Zipline's result, the reported frame, what the strategy saw, and the book."""
    calendar = get_calendar("XNYS")
    sessions = calendar.sessions_in_range(pd.Timestamp("2024-02-28"), pd.Timestamp(end))  # type: ignore[arg-type]
    foreign = read_foreign_dividends(bundle.adjustment_reader.conn)
    assert foreign is not None
    metrics_set, book = dividend_metrics(foreign)
    engine = Engine(socket_file_path="/tmp/foreverbull_zipline_dividend_test.sock")
    engine._dividend_book = book
    seen: dict[pd.Timestamp, Any] = {}

    def initialize(context):
        set_slippage(us_equities=FixedSlippage(spread=spread))
        set_commission(us_equities=PerShare(cost=commission, min_trade_cost=min_commission))

    def handle_data(context, data):
        today = context.get_datetime().tz_localize(None).normalize()
        if not seen:
            context.order(context.symbol("KO"), shares)
        period = engine_service_pb.GetCurrentPeriodResponse()
        merge_from_binary(period, engine._get_current_period(b"", context))
        seen[today] = period.portfolio

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
            capital_base=START_CASH,
        ),
        metrics_set=metrics_set,
        blotter=load(Blotter, "default"),
        benchmark_returns=zero_benchmark_returns(sessions),
        benchmark_sid=None,
        initialize=initialize,
        handle_data=handle_data,
    )
    logging.getLogger("zipline").setLevel(logging.ERROR)
    result = algorithm.run()
    result.index = pd.DatetimeIndex([pd.Timestamp(day.date()) for day in result.index])
    reported = total_return_frame(result, START_CASH, zero_benchmark(sessions))
    return result, reported, seen, book


def _day(frame: pd.DataFrame, day: str, column: str) -> float:
    return float(frame.loc[pd.Timestamp(day), column])


@pytest.fixture
def zipline_root(tmp_path, monkeypatch):
    monkeypatch.setenv("ZIPLINE_ROOT", str(tmp_path))


@pytest.mark.usefixtures("zipline_root")
def test_cash_is_credited_at_the_ex_date_open():
    result, reported, seen, book = _run(_ingest("ko_paid", _ko_dividends()))

    # All the cash buys KO at the 02-29 close: V_0 = 100 x 60.02 = 6,002.00.
    assert _day(result, "2024-02-29", "ending_cash") == pytest.approx(0.0, abs=1e-6)

    # Cash steps up by 100 x 0.485 = 48.50 at each ex-date open, not on the pay dates.
    for day, cash in [
        ("2024-03-13", 0.0),
        ("2024-03-14", 48.50),
        ("2024-03-28", 48.50),
        ("2024-04-01", 48.50),
        ("2024-06-13", 48.50),
        ("2024-06-14", 97.00),
        ("2024-07-01", 97.00),
        ("2024-07-31", 97.00),
    ]:
        assert _day(result, day, "ending_cash") == pytest.approx(cash), day

    # The strategy can spend it from the ex-date session on.
    assert seen[cast(pd.Timestamp, pd.Timestamp("2024-03-13"))].cash == pytest.approx(0.0, abs=1e-6)
    assert seen[EX_1].cash == pytest.approx(48.50)
    assert seen[EX_2].cash == pytest.approx(97.00)

    # V_t = cash + KO. 03-14: 48.50 + 6,050.00; 06-14: 97.00 + 6,255.00.
    assert _day(reported, "2024-03-14", "portfolio_value") == pytest.approx(6_098.50)
    assert _day(reported, "2024-06-14", "portfolio_value") == pytest.approx(6_352.00)

    # Ex-day total return: 6,098.50 / 6,112.00 - 1 and 6,352.00 / 6,347.50 - 1.
    assert _day(reported, "2024-03-14", "returns") == pytest.approx(6_098.50 / 6_112.00 - 1, abs=1e-12)
    assert _day(reported, "2024-06-14", "returns") == pytest.approx(6_352.00 / 6_347.50 - 1, abs=1e-12)
    # Ex-day price return: the price drop, 6,050 / 6,112 - 1 and 6,255 / 6,299 - 1.
    excluding = reported["portfolio_value"] - reported["dividends_earned"]
    assert excluding[EX_1] / excluding[cast(pd.Timestamp, pd.Timestamp("2024-03-13"))] - 1 == pytest.approx(
        6_050 / 6_112 - 1, abs=1e-12
    )
    assert excluding[EX_2] / excluding[cast(pd.Timestamp, pd.Timestamp("2024-06-13"))] - 1 == pytest.approx(
        6_255 / 6_299 - 1, abs=1e-12
    )

    # End: V_T = 6,674.00 + 97.00 = 6,771.00, E_T = V_T - Dcum_T = 6,674.00.
    end = reported.iloc[-1]
    assert end["portfolio_value"] == pytest.approx(6_771.00, abs=1e-9)
    assert end["algorithm_period_return"] == pytest.approx(6_771 / 6_002 - 1, abs=1e-12)  # 12.812%
    assert end["price_return"] == pytest.approx(6_674 / 6_002 - 1, abs=1e-12)  # 11.196%
    assert end["algorithm_period_return"] - end["price_return"] == pytest.approx(97 / 6_002, abs=1e-12)
    # V_T - E_T, with E_T read back from the reported price return.
    assert end["portfolio_value"] - (end["price_return"] + 1.0) * START_CASH == pytest.approx(97.00, abs=1e-9)
    assert end["dividends_earned"] == pytest.approx(97.00, abs=1e-9)
    # Nothing is owed, so Zipline's own total return is the same number.
    assert result.iloc[-1]["algorithm_period_return"] == pytest.approx(end["algorithm_period_return"])
    assert book.foreign_booked == 0


@pytest.mark.usefixtures("zipline_root")
def test_a_dividend_paid_after_the_run_ends_is_still_cash():
    # The run ends 06-28; the 06-14 dividend really pays on 07-01.
    result, reported, _, _ = _run(_ingest("ko_ends_early", _ko_dividends()), end="2024-06-28")

    assert _day(result, "2024-06-28", "ending_cash") == pytest.approx(97.00)
    end = reported.iloc[-1]
    assert end["algorithm_period_return"] - end["price_return"] == pytest.approx(97.0 / 6_002.0)


@pytest.mark.usefixtures("zipline_root")
def test_a_dividend_without_a_pay_date_is_credited_the_same():
    # Yahoo sends no pay dates; removing them changes nothing.
    _, with_pay_dates, _, _ = _run(_ingest("ko_with_pay", _ko_dividends()))
    result, without_pay_dates, _, _ = _run(_ingest("ko_without_pay", _ko_dividends(pay_dates=False)))

    pd.testing.assert_frame_equal(without_pay_dates, with_pay_dates)
    assert _day(result, "2024-03-14", "ending_cash") == pytest.approx(48.50)
    assert without_pay_dates.iloc[-1]["algorithm_period_return"] == pytest.approx(0.128124, abs=5e-7)
    assert without_pay_dates.iloc[-1]["price_return"] == pytest.approx(0.111963, abs=5e-7)


@pytest.mark.usefixtures("zipline_root")
def test_a_foreign_dividend_is_booked_gross():
    # The same dividends flagged as a foreign issuer's: no withholding is
    # taken, so each credits the full 100 x 0.485 = 48.50, and the run
    # counts both for the "booked gross" label.
    bundle = _ingest("ko_foreign", _ko_dividends(foreign=True))
    sid = int(bundle.asset_finder.lookup_symbol("KO", as_of_date=None).sid)
    assert read_foreign_dividends(bundle.adjustment_reader.conn) == {(sid, EX_1), (sid, EX_2)}
    result, reported, _, book = _run(bundle)

    assert _day(result, "2024-03-14", "ending_cash") == pytest.approx(48.50)
    assert _day(result, "2024-07-31", "ending_cash") == pytest.approx(97.00)
    assert reported.iloc[-1]["algorithm_period_return"] == pytest.approx(0.128124, abs=5e-7)
    assert book.foreign_booked == 2


@pytest.mark.usefixtures("zipline_root")
def test_a_short_position_pays_the_dividend():
    # Short 100 KO: the account pays 48.50 at each ex-date open.
    result, reported, _, _ = _run(_ingest("ko_short", _ko_dividends()), shares=-SHARES)

    proceeds = _day(result, "2024-03-13", "ending_cash")
    assert _day(result, "2024-03-14", "ending_cash") == pytest.approx(proceeds - 48.50)
    assert reported.iloc[-1]["dividends_earned"] == pytest.approx(-97.00)


@pytest.mark.usefixtures("zipline_root")
def test_costs_change_the_fill_not_the_dividend_cash():
    # The same buy and hold with $0.01 a share commission ($1 minimum) and a
    # $0.04 spread: the fill and V_0's cash change, the dividends don't.
    result, reported, _, _ = _run(
        _ingest("ko_costs", _ko_dividends()), commission=0.01, min_commission=1.0, spread=0.04
    )

    # 100 KO at 60.02 + 0.02 half spread, plus max(100 x 0.01, 1.00) = 1.00.
    bought = _day(result, "2024-02-29", "ending_cash")
    assert bought == pytest.approx(START_CASH - 100 * 60.04 - 1.00, abs=1e-9)
    # 48.50 lands at each ex-date open and nothing else moves the cash.
    assert _day(result, "2024-03-14", "ending_cash") - _day(result, "2024-03-13", "ending_cash") == pytest.approx(
        48.50, abs=1e-9
    )
    assert _day(result, "2024-06-14", "ending_cash") - _day(result, "2024-06-13", "ending_cash") == pytest.approx(
        48.50, abs=1e-9
    )
    assert _day(result, "2024-07-31", "ending_cash") == pytest.approx(bought + 97.00, abs=1e-9)
    # V_T - E_T is the dividend cash whatever the fills and costs.
    end = reported.iloc[-1]
    assert end["dividends_earned"] == pytest.approx(97.00, abs=1e-9)
    assert end["portfolio_value"] - (end["price_return"] + 1.0) * START_CASH == pytest.approx(97.00, abs=1e-9)
    assert end["algorithm_period_return"] - end["price_return"] == pytest.approx(97 / 6_002, abs=1e-12)
