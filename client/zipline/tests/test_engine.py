import logging
import multiprocessing
import os
import sqlite3

from concurrent import futures
from datetime import date
from multiprocessing.queues import Queue
from threading import Thread
from typing import Any
from typing import cast

import grpc
import numpy as np
import pandas as pd
import pytest

from foreverbull_zipline.data_bundles.bundle_alignment import BUNDLE_VERSION_MARKER
from foreverbull_zipline.data_bundles.bundle_alignment import ensure_session_aligned_bundle
from foreverbull_zipline.data_bundles.bundle_alignment import latest_ingest_dir
from foreverbull_zipline.data_bundles.dividends import write_foreign_dividends
from foreverbull_zipline.data_bundles.foreverbull import SQLIngester
from foreverbull_zipline.engine import Engine
from foreverbull_zipline.engine import benchmark_missing_line
from foreverbull_zipline.engine import benchmark_returns
from foreverbull_zipline.engine import load_benchmark_returns
from foreverbull_zipline.engine import zero_benchmark_returns
from foreverbull_zipline.errors import PlainSentence
from foreverbull_zipline.service import BacktestService
from foreverbull_zipline.session_service import SessionServiceServicer
from zipline.data import bundles
from zipline.data.data_portal import DataPortal
from zipline.extensions import load
from zipline.finance import metrics
from zipline.finance.blotter import Blotter
from zipline.finance.trading import SimulationParameters
from zipline.utils.calendar_utils import get_calendar

from foreverbull.pb.foreverbull import common_pb
from foreverbull.pb.foreverbull.backtest.service.v1 import engine_pb as engine_service_pb
from foreverbull.pb.foreverbull.backtest.service.v1 import engine_pb_grpc as engine_service_pb_grpc
from foreverbull.pb.foreverbull.backtest.v1 import backtest_pb
from foreverbull.pb.foreverbull.backtest.v1 import execution_pb
from foreverbull.pb.foreverbull.finance.v1 import marketdata_pb
from zipline import TradingAlgorithm


def test_start_stop(fb_env, spawn_process):
    execution = Engine()
    execution.start()
    execution.is_ready.wait(3.0)
    execution.stop()
    execution.join(3.0)


def logging_thread(q: Queue):
    while True:
        record = q.get()
        if record is None:
            break
        logger = logging.getLogger(record.name)
        logger.handle(record)


@pytest.fixture(scope="session")
def ensure_ingestion(backtest_entity, fb_database):
    _, verify = fb_database
    verify(backtest_entity)
    bundles.register(backtest_entity.name, SQLIngester(backtest_entity), calendar_name="XNYS")
    try:
        bundles.load(backtest_entity.name, os.environ, None)
    except ValueError:
        service = BacktestService()
        service.ingest(engine_service_pb.IngestRequest(backtest=backtest_entity), cast(Any, None))


@pytest.fixture(scope="function")
def engine(fb_env, ensure_ingestion):
    log_queue = multiprocessing.Queue()
    log_thread = Thread(target=logging_thread, args=(log_queue,))
    log_thread.start()
    execution = Engine(logging_queue=log_queue)
    try:
        execution.start()
        execution.is_ready.wait(3.0)
        yield execution
    finally:
        execution.stop()
        execution.join(3.0)
        log_queue.put_nowait(None)
        log_thread.join(3.0)


def test_premature_stop(execution: execution_pb.Execution, engine: Engine):
    request = engine_service_pb.RunBacktestRequest(
        execution=execution,
    )
    response = engine.run_backtest(request)
    assert response.execution

    for _ in range(5):
        response = engine.get_current_period(engine_service_pb.GetCurrentPeriodRequest())
        if response.is_running is False:
            break
        assert response.portfolio
        engine.place_orders_and_continue(engine_service_pb.PlaceOrdersAndContinueRequest())


def test_get_result(
    execution: execution_pb.Execution,
    backtest_entity: backtest_pb.Backtest,
    engine: Engine,
):
    request = engine_service_pb.RunBacktestRequest(
        execution=execution,
        benchmark=backtest_entity.benchmark,
    )
    response = engine.run_backtest(request)
    assert response.execution

    while True:
        response = engine.get_current_period(engine_service_pb.GetCurrentPeriodRequest())
        if response.is_running is False:
            break
        assert response.portfolio
        engine.place_orders_and_continue(engine_service_pb.PlaceOrdersAndContinueRequest())

    response = engine.get_backtest_result(engine_service_pb.GetResultRequest())
    assert len(response.periods)
    assert response.periods[-1].benchmark_period_return != 0.0


def test_broker(
    backtest_entity: backtest_pb.Backtest,
    execution: execution_pb.Execution,
    engine: Engine,
):
    request = engine_service_pb.RunBacktestRequest(
        execution=execution,
        benchmark=backtest_entity.benchmark,
    )
    response = engine.run_backtest(request)
    assert response.execution

    response = engine.get_current_period(engine_service_pb.GetCurrentPeriodRequest())
    assert response.is_running
    assert response.portfolio
    engine.place_orders_and_continue(engine_service_pb.PlaceOrdersAndContinueRequest())

    response = engine.place_orders_and_continue(
        engine_service_pb.PlaceOrdersAndContinueRequest(
            orders=[
                marketdata_pb.Order(symbol=backtest_entity.periods[0].assets[0].symbol, amount=10),
            ]
        )
    )

    response = engine.get_current_period(engine_service_pb.GetCurrentPeriodRequest())
    assert response.is_running
    assert response.portfolio is not None
    assert response.portfolio.positions[0].amount == 10

    response = engine.place_orders_and_continue(
        engine_service_pb.PlaceOrdersAndContinueRequest(
            orders=[
                marketdata_pb.Order(symbol=backtest_entity.periods[0].assets[0].symbol, amount=-5),
            ]
        )
    )

    response = engine.get_current_period(engine_service_pb.GetCurrentPeriodRequest())
    assert response.is_running
    assert response.portfolio is not None
    assert response.portfolio.positions[0].amount == 5

    response = engine.place_orders_and_continue(
        engine_service_pb.PlaceOrdersAndContinueRequest(
            orders=[
                marketdata_pb.Order(symbol=backtest_entity.periods[0].assets[0].symbol, amount=-5),
            ]
        )
    )

    response = engine.get_current_period(engine_service_pb.GetCurrentPeriodRequest())
    assert response.is_running
    assert response.portfolio is not None
    assert response.portfolio.positions == []

    while True:
        response = engine.get_current_period(engine_service_pb.GetCurrentPeriodRequest())
        if response.is_running is False:
            break
        assert response.portfolio
        engine.place_orders_and_continue(engine_service_pb.PlaceOrdersAndContinueRequest())

    response = engine.get_backtest_result(engine_service_pb.GetResultRequest())
    assert len(response.periods)
    assert response.periods[-1].alpha != 0.0
    assert response.periods[-1].beta != 0.0


class _Benchmark:
    symbol = "SPY"
    sid = 0

    def __init__(self, start: str = "2022-12-30"):
        self.start_date = pd.Timestamp(start)


def _sessions(start: str, end: str):
    return get_calendar("XNYS").sessions_in_range(pd.Timestamp(start), pd.Timestamp(end))  # type: ignore[arg-type]


def test_benchmark_returns_start_on_the_first_session():
    # 2023-01-03 is the first XNYS session of 2023 and SPY's first bar in
    # a 2023 bundle. The series Zipline reindexes must start there, at the
    # session label, not at the 14:30 bar stamp or a calendar date.
    sessions = _sessions("2023-01-03", "2023-01-10")
    returns = benchmark_returns(
        sessions,
        sessions,
        opens=[99.0, 100.0, 102.0, 101.0, 105.0, 106.0],
        closes=[100.0, 102.0, 101.0, 105.0, 106.0, 110.0],
        dividends=pd.Series(dtype=float),
    ).price

    assert sessions[0] == pd.Timestamp("2023-01-03")
    assert returns.index[0] == sessions[0]
    assert list(returns.index) == list(sessions)
    assert returns.iloc[0] == pytest.approx((100.0 - 99.0) / 99.0)
    assert returns.iloc[1] == pytest.approx(102.0 / 100.0 - 1.0)
    assert (1.0 + returns).prod() - 1.0 == pytest.approx(110.0 / 99.0 - 1.0)


def test_benchmark_returns_use_the_previous_close_when_there_is_one():
    sessions = _sessions("2023-01-04", "2023-01-10")
    window = _sessions("2023-01-03", "2023-01-10")
    returns = benchmark_returns(
        window,
        sessions,
        opens=[99.0, 100.0, 102.0, 101.0, 105.0, 106.0],
        closes=[100.0, 102.0, 101.0, 105.0, 106.0, 110.0],
        dividends=pd.Series(dtype=float),
    ).price

    assert returns.index[0] == sessions[0]
    assert returns.iloc[0] == pytest.approx(102.0 / 100.0 - 1.0)


def test_benchmark_total_return_adds_the_dividend_on_its_ex_date():
    # A $1.00 dividend on 01-05: r = (101 + 1) / 102 - 1 against the price return 101 / 102 - 1.
    sessions = _sessions("2023-01-04", "2023-01-10")
    window = _sessions("2023-01-03", "2023-01-10")
    returns = benchmark_returns(
        window,
        sessions,
        opens=[99.0, 100.0, 102.0, 101.0, 105.0, 106.0],
        closes=[100.0, 102.0, 101.0, 105.0, 106.0, 110.0],
        dividends=pd.Series({pd.Timestamp("2023-01-05"): 1.0}),
    )

    assert returns.price[pd.Timestamp("2023-01-05")] == pytest.approx(101.0 / 102.0 - 1.0)
    assert returns.total[pd.Timestamp("2023-01-05")] == pytest.approx(102.0 / 102.0 - 1.0)
    other_days = returns.total.index != pd.Timestamp("2023-01-05")
    assert list(returns.total[other_days]) == list(returns.price[other_days])
    assert (1.0 + returns.total).prod() - 1.0 == pytest.approx((110.0 / 100.0) * (102.0 / 101.0) - 1.0)


def test_load_benchmark_returns_reads_the_benchmark_dividends():
    calendar = get_calendar("XNYS")
    sessions = _sessions("2023-01-03", "2023-01-06")
    closes = {session: 100.0 for session in _sessions("2022-12-30", "2023-01-06")}
    bundle = _fake_bundle(calendar, closes, {"2023-01-05": 2.0, "2022-06-01": 9.0})

    returns = load_benchmark_returns("oef-2024q1", bundle, _Benchmark(), calendar, sessions)
    assert list(returns.price) == [0.0, 0.0, 0.0, 0.0]
    assert list(returns.total) == pytest.approx([0.0, 0.0, 0.02, 0.0])


class _FakeDailyReader:
    def __init__(self, calendar, closes_by_session):
        self._calendar = calendar
        self._closes = closes_by_session
        self.sessions = pd.DatetimeIndex(sorted(closes_by_session))

    def load_raw_arrays(self, columns, start, end, assets):
        assert columns == ["open", "close"]
        window = self._calendar.sessions_in_range(start, end)
        closes = np.array([[self._closes[session]] for session in window], dtype=float)
        return [closes.copy(), closes.copy()]


class _Payouts:
    """An adjustments database with the benchmark's dividends, by default none."""

    def __init__(self, dividends: dict[str, float] | None = None, sid: int = 0):
        self.conn = sqlite3.connect(":memory:")
        self.conn.execute("CREATE TABLE dividend_payouts (sid INTEGER, ex_date INTEGER, amount REAL)")
        self.conn.executemany(
            "INSERT INTO dividend_payouts VALUES (?, ?, ?)",
            [
                (sid, int(cast(pd.Timestamp, pd.Timestamp(day)).timestamp()), amount)
                for day, amount in (dividends or {}).items()
            ],
        )


def _fake_bundle(calendar, closes, dividends: dict[str, float] | None = None):
    class _FakeBundle:
        equity_daily_bar_reader = _FakeDailyReader(calendar, closes)
        adjustment_reader = _Payouts(dividends)

    return _FakeBundle()


def _gap_line(count: str, days: str) -> str:
    return (
        f"Couldn't run backtest 'oef-2024q1': the benchmark SPY has no prices on {count}: {days}. "
        "Nothing was run. Run `fbull backtest create` again to reload its prices, or choose another benchmark."
    )


def test_load_benchmark_returns_aligns_the_reader_to_sessions():
    calendar = get_calendar("XNYS")
    sessions = _sessions("2023-01-03", "2023-01-06")
    closes = {session: 100.0 + index for index, session in enumerate(_sessions("2022-12-30", "2023-01-06"))}

    returns = load_benchmark_returns(
        "oef-2024q1", _fake_bundle(calendar, closes), _Benchmark(), calendar, sessions
    ).price
    assert returns.index[0] == sessions[0] == pd.Timestamp("2023-01-03")
    assert list(returns.index) == list(sessions)
    assert returns.iloc[0] == pytest.approx(101.0 / 100.0 - 1.0)
    assert returns.iloc[-1] == pytest.approx(104.0 / 103.0 - 1.0)
    assert bool(np.isfinite(returns.to_numpy()).all())


def test_load_benchmark_returns_on_the_first_bundle_session_uses_open_to_close():
    # The run starts on the benchmark's first session, so there is no
    # previous close and no window day: the first return is the session's open to close, the documented
    # first-day rule, computed and not filled.
    calendar = get_calendar("XNYS")
    sessions = _sessions("2023-01-03", "2023-01-06")
    closes = {session: 100.0 + index for index, session in enumerate(sessions)}

    class _OpensBelowCloses(_FakeDailyReader):
        def load_raw_arrays(self, columns, start, end, assets):
            closes, _ = super().load_raw_arrays(columns, start, end, assets)
            return [closes - 1.0, closes]

    class _FakeBundle:
        equity_daily_bar_reader = _OpensBelowCloses(calendar, closes)
        adjustment_reader = _Payouts({"2023-01-03": 0.5})

    returns = load_benchmark_returns("oef-2024q1", _FakeBundle(), _Benchmark("2023-01-03"), calendar, sessions)
    assert list(returns.price.index) == list(sessions)
    assert returns.price.iloc[0] == pytest.approx((100.0 - 99.0) / 99.0)
    assert returns.price.iloc[1] == pytest.approx(101.0 / 100.0 - 1.0)
    # No close before the first session to have held the share at, so its
    # 0.50 dividend is not in the total return either.
    assert returns.total.iloc[0] == pytest.approx((100.0 - 99.0) / 99.0)
    assert list(returns.total.iloc[1:]) == list(returns.price.iloc[1:])


def test_load_benchmark_returns_fails_on_sessions_the_bundle_lacks():
    calendar = get_calendar("XNYS")
    sessions = _sessions("2023-01-03", "2023-01-10")
    closes = {session: 100.0 for session in _sessions("2022-12-30", "2023-01-05")}

    with pytest.raises(PlainSentence) as error:
        load_benchmark_returns("oef-2024q1", _fake_bundle(calendar, closes), _Benchmark(), calendar, sessions)
    assert str(error.value) == _gap_line("3 trading days", "2023-01-06 to 2023-01-10")


def test_load_benchmark_returns_fails_on_a_nan_close_mid_run():
    calendar = get_calendar("XNYS")
    sessions = _sessions("2023-01-03", "2023-01-10")
    closes = {session: 100.0 for session in _sessions("2022-12-30", "2023-01-10")}
    closes[pd.Timestamp("2023-01-05")] = np.nan

    with pytest.raises(PlainSentence) as error:
        load_benchmark_returns("oef-2024q1", _fake_bundle(calendar, closes), _Benchmark(), calendar, sessions)
    assert str(error.value) == _gap_line("1 trading day", "2023-01-05")


def test_load_benchmark_returns_fails_on_a_zero_close():
    # A 0 close would make the next return infinite; the day is named
    # instead.
    calendar = get_calendar("XNYS")
    sessions = _sessions("2023-01-03", "2023-01-10")
    closes = {session: 100.0 for session in _sessions("2022-12-30", "2023-01-10")}
    closes[pd.Timestamp("2023-01-04")] = 0.0

    with pytest.raises(PlainSentence) as error:
        load_benchmark_returns("oef-2024q1", _fake_bundle(calendar, closes), _Benchmark(), calendar, sessions)
    assert str(error.value) == _gap_line("1 trading day", "2023-01-04")


def test_load_benchmark_returns_fails_on_a_nan_close_the_day_before_the_start():
    # A previous close exists in the bundle calendar but isn't a price: the
    # window day is named, not replaced with the first day's open to close.
    calendar = get_calendar("XNYS")
    sessions = _sessions("2023-01-03", "2023-01-10")
    closes = {session: 100.0 for session in _sessions("2022-12-30", "2023-01-10")}
    closes[pd.Timestamp("2022-12-30")] = np.nan

    with pytest.raises(PlainSentence) as error:
        load_benchmark_returns("oef-2024q1", _fake_bundle(calendar, closes), _Benchmark(), calendar, sessions)
    assert str(error.value) == _gap_line("1 trading day", "2022-12-30")


def test_load_benchmark_returns_names_every_bad_day_in_ranges():
    # The window day, a run of NaN closes over a weekend, a 0 close and
    # days past the bundle's end, all in one error.
    calendar = get_calendar("XNYS")
    sessions = _sessions("2023-01-03", "2023-01-20")
    closes = {session: 100.0 for session in _sessions("2022-12-30", "2023-01-18")}
    for day in ("2022-12-30", "2023-01-06", "2023-01-09", "2023-01-10"):
        closes[pd.Timestamp(day)] = np.nan
    closes[pd.Timestamp("2023-01-12")] = 0.0

    with pytest.raises(PlainSentence) as error:
        load_benchmark_returns("oef-2024q1", _fake_bundle(calendar, closes), _Benchmark(), calendar, sessions)
    assert str(error.value) == _gap_line(
        "7 trading days", "2022-12-30, 2023-01-06 to 2023-01-10, 2023-01-12, 2023-01-19 to 2023-01-20"
    )


def test_benchmark_missing_line_merges_sessions_in_a_row():
    window = _sessions("2024-01-02", "2024-03-28")
    missing = pd.DatetimeIndex(["2024-02-14", "2024-02-20", "2024-02-21"])
    assert benchmark_missing_line("oef-2024q1", "SPY", missing, window) == (
        "Couldn't run backtest 'oef-2024q1': the benchmark SPY has no prices on 3 trading days: "
        "2024-02-14, 2024-02-20 to 2024-02-21. Nothing was run. "
        "Run `fbull backtest create` again to reload its prices, or choose another benchmark."
    )


def test_benchmark_missing_line_runs_over_a_holiday_and_has_no_cap():
    window = _sessions("2024-01-02", "2024-03-28")
    # 2024-02-19 is Presidents Day, so Friday the 16th and Tuesday the 20th
    # are sessions in a row.
    assert benchmark_missing_line("oef-2024q1", "SPY", pd.DatetimeIndex(["2024-02-16", "2024-02-20"]), window) == (
        "Couldn't run backtest 'oef-2024q1': the benchmark SPY has no prices on 2 trading days: "
        "2024-02-16 to 2024-02-20. Nothing was run. "
        "Run `fbull backtest create` again to reload its prices, or choose another benchmark."
    )
    every_other = pd.DatetimeIndex(window[:24:2])
    days = ", ".join(f"{day:%Y-%m-%d}" for day in every_other)
    assert benchmark_missing_line("oef-2024q1", "SPY", every_other, window) == (
        f"Couldn't run backtest 'oef-2024q1': the benchmark SPY has no prices on 12 trading days: {days}. "
        "Nothing was run. Run `fbull backtest create` again to reload its prices, or choose another benchmark."
    )


def test_zero_benchmark_returns_stay_on_sessions():
    sessions = _sessions("2023-01-03", "2023-01-10")
    returns = zero_benchmark_returns(sessions)

    assert list(returns.index) == list(sessions)
    assert pd.Timestamp("2023-01-07") not in returns.index
    assert bool((returns == 0.0).all())


def _write_e2e_bundle(
    bundle_name: str,
    sessions,
    *,
    stale: bool,
    spy_closes: list[float] | None = None,
    before_dividends: bool = False,
) -> None:
    if spy_closes is None:
        spy_closes = [100.0 + index for index in range(len(sessions))]
    aapl_closes = [50.0 + 0.5 * index for index in range(len(sessions))]
    index = sessions + pd.Timedelta(hours=14, minutes=30) if stale else sessions

    def _frame(closes: list[float]) -> pd.DataFrame:
        return pd.DataFrame(
            {
                "open": [max(close - 1.0, 0.0) for close in closes],
                "high": [close + 1.0 for close in closes],
                "low": [max(close - 2.0, 0.0) for close in closes],
                "close": closes,
                "volume": [1000] * len(closes),
            },
            index=index,
        )

    metadata = pd.DataFrame(
        {
            "sid": [0, 1],
            "start_date": [index[0], index[0]],
            "end_date": [index[-1], index[-1]],
            "auto_close_date": [index[-1] + pd.Timedelta(days=1)] * 2,
            "symbol": ["AAPL", "SPY"],
            "exchange": ["NASDAQ", "NASDAQ"],
        }
    )

    def ingest_bundle(
        _environ,
        asset_db_writer,
        _minute_bar_writer,
        daily_bar_writer,
        adjustment_writer,
        _calendar,
        _start_session,
        _end_session,
        _cache,
        _show_progress,
        _output_dir,
    ):
        daily_bar_writer.write([(0, _frame(aapl_closes)), (1, _frame(spy_closes))])
        asset_db_writer.write(equities=metadata)
        adjustment_writer.write()
        if not before_dividends:
            write_foreign_dividends(adjustment_writer.conn, pd.DataFrame({"sid": [], "ex_date": []}))

    bundles.register(bundle_name, ingest_bundle, calendar_name="XNYS")
    bundles.ingest(bundle_name, os.environ, pd.Timestamp("2023-02-01", tz="UTC"), [], False)


def _run_synthetic_algorithm(bundle_name: str, sessions):
    bundle = bundles.load(bundle_name, os.environ, None)
    calendar = get_calendar("XNYS")
    benchmark_asset = bundle.asset_finder.lookup_symbol("SPY", as_of_date=None)
    stored_benchmark_returns = load_benchmark_returns(bundle_name, bundle, benchmark_asset, calendar, sessions)
    data_portal = DataPortal(
        bundle.asset_finder,
        trading_calendar=calendar,
        first_trading_day=bundle.equity_minute_bar_reader.first_trading_day,
        equity_minute_reader=bundle.equity_minute_bar_reader,
        equity_daily_reader=bundle.equity_daily_bar_reader,
        adjustment_reader=bundle.adjustment_reader,
    )

    def initialize(context):
        context.asset = context.symbol("AAPL")
        context.ordered = False

    def handle_data(context, data):
        if not context.ordered and data.can_trade(context.asset):
            context.order(context.asset, 10)
            context.ordered = True

    trading_algorithm = TradingAlgorithm(
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
        benchmark_returns=stored_benchmark_returns.total,
        benchmark_sid=None,
        initialize=initialize,
        handle_data=handle_data,
    )
    return trading_algorithm.run(), benchmark_asset


@pytest.mark.parametrize("stale", [False, True])
def test_trading_algorithm_cumulative_benchmark_end_to_end(tmp_path, monkeypatch, stale):
    # Real TradingAlgorithm over a synthetic bundle, no database: SPY
    # closes rise 100 -> 119 with the first open at 99, so the cumulative
    # benchmark return the run reports must be 119/99 - 1, not flat, and
    # the AAPL order placed on the first session must be in the result.
    # The stale variant is a pre-alignment bundle (14:30 stamps, no
    # marker); the run-path check rebuilds it before the algorithm runs,
    # exactly as Engine._run does, so it must reach the same numbers.
    monkeypatch.setenv("ZIPLINE_ROOT", str(tmp_path))
    bundle_name = "e2e_stale_bundle" if stale else "e2e_fresh_bundle"
    sessions = _sessions("2023-01-03", "2023-01-31")
    _write_e2e_bundle(bundle_name, sessions, stale=stale)
    try:
        rebuilt = ensure_session_aligned_bundle(bundle_name, os.environ)
        assert rebuilt is stale

        result, benchmark_asset = _run_synthetic_algorithm(bundle_name, sessions)

        assert benchmark_asset.start_date == sessions[0]
        assert len(result) == len(sessions)
        assert result["benchmark_period_return"].iloc[-1] == pytest.approx(119.0 / 99.0 - 1.0)
        assert result["algorithm_period_return"].iloc[-1] > 0.0
        assert result["portfolio_value"].iloc[-1] > 100_000.0
    finally:
        bundles.unregister(bundle_name)


def test_load_benchmark_returns_fails_on_a_stored_zero_close_in_a_real_bundle(tmp_path, monkeypatch):
    # bcolz stores a 0 close and reads it back as NaN, and reader.sessions
    # is the bundle calendar, so the stored prices are what is checked.
    monkeypatch.setenv("ZIPLINE_ROOT", str(tmp_path))
    sessions = _sessions("2023-01-03", "2023-01-31")
    spy_closes = [100.0 + index for index in range(len(sessions))]
    spy_closes[5] = 0.0
    _write_e2e_bundle("e2e_zero_close_bundle", sessions, stale=False, spy_closes=spy_closes)
    try:
        bundle = bundles.load("e2e_zero_close_bundle", os.environ, None)
        benchmark_asset = bundle.asset_finder.lookup_symbol("SPY", as_of_date=None)
        calendar = get_calendar("XNYS")

        with pytest.raises(PlainSentence) as error:
            load_benchmark_returns("oef-2024q1", bundle, benchmark_asset, calendar, sessions[2:])
        assert str(error.value) == _gap_line("1 trading day", f"{sessions[5]:%Y-%m-%d}")

        # A later start measures from the previous stored close.
        returns = load_benchmark_returns("oef-2024q1", bundle, benchmark_asset, calendar, sessions[1:5]).price
        assert returns.iloc[0] == pytest.approx(101.0 / 100.0 - 1.0)
    finally:
        bundles.unregister("e2e_zero_close_bundle")


def _marker(bundle_name: str):
    ingest_dir = latest_ingest_dir(bundle_name, os.environ)
    assert ingest_dir is not None
    return ingest_dir / BUNDLE_VERSION_MARKER


@pytest.fixture
def run_stub(tmp_path, monkeypatch, spawn_process):
    """A real engine process behind the session service, over gRPC."""
    monkeypatch.setenv("ZIPLINE_ROOT", str(tmp_path))
    engine = Engine(socket_file_path=str(tmp_path / "engine.sock"))
    engine.start()
    assert engine.is_ready.wait(10.0)
    server = grpc.server(thread_pool=futures.ThreadPoolExecutor())
    engine_service_pb_grpc.EngineSessionServiceSync.add_to_server(SessionServiceServicer(engine), server)
    port = server.add_insecure_port("localhost:0")
    server.start()
    try:
        yield engine_service_pb_grpc.EngineSessionClientSync(grpc.insecure_channel(f"localhost:{port}"))
    finally:
        server.stop(None)
        engine.stop()
        engine.join(5.0)
        if "oef-2024q1" in bundles.bundles:
            bundles.unregister("oef-2024q1")


def _run_request(bundle_name: str, start: str, end: str) -> engine_service_pb.RunBacktestRequest:
    first, last = date.fromisoformat(start), date.fromisoformat(end)
    return engine_service_pb.RunBacktestRequest(
        execution=execution_pb.Execution(
            id="plain-sentence",
            backtest=bundle_name,
            start_date=common_pb.Date(year=first.year, month=first.month, day=first.day),
            end_date=common_pb.Date(year=last.year, month=last.month, day=last.day),
        ),
        benchmark="SPY",
    )


def test_a_benchmark_gap_reaches_the_session_client_as_written(run_stub):
    # The engine process refuses the run; the session service sends the
    # sentence with FAILED_PRECONDITION, which the broker passes to fbull.
    sessions = _sessions("2023-01-03", "2023-01-31")
    spy_closes = [100.0 + index for index in range(len(sessions))]
    spy_closes[5] = 0.0
    _write_e2e_bundle("oef-2024q1", sessions, stale=False, spy_closes=spy_closes)

    with pytest.raises(grpc.RpcError) as error:
        run_stub.run_backtest(_run_request("oef-2024q1", "2023-01-04", "2023-01-20"))

    assert error.value.code() == grpc.StatusCode.FAILED_PRECONDITION
    assert error.value.details() == _gap_line("1 trading day", f"{sessions[5]:%Y-%m-%d}")


def test_a_damaged_version_marker_reaches_the_session_client_as_written(run_stub):
    sessions = _sessions("2023-01-03", "2023-01-31")
    _write_e2e_bundle("oef-2024q1", sessions, stale=False)
    _marker("oef-2024q1").write_text("one\n", encoding="utf-8")

    with pytest.raises(grpc.RpcError) as error:
        run_stub.run_backtest(_run_request("oef-2024q1", "2023-01-04", "2023-01-20"))

    assert error.value.code() == grpc.StatusCode.FAILED_PRECONDITION
    assert error.value.details() == (
        "The price data for backtest 'oef-2024q1' can't be read: its version marker is damaged. "
        "Run `fbull backtest create` again to rebuild it."
    )


def test_a_bundle_from_before_dividends_is_refused(run_stub):
    # Its returns would leave every dividend out and still be labelled a total return.
    sessions = _sessions("2023-01-03", "2023-01-31")
    _write_e2e_bundle("oef-2024q1", sessions, stale=False, before_dividends=True)

    with pytest.raises(grpc.RpcError) as error:
        run_stub.run_backtest(_run_request("oef-2024q1", "2023-01-04", "2023-01-20"))

    assert error.value.code() == grpc.StatusCode.FAILED_PRECONDITION
    assert error.value.details() == (
        "Couldn't run backtest 'oef-2024q1': its data was loaded before dividends were counted, "
        "so its returns would leave them out. Nothing was run. "
        "Run `fbull backtest delete oef-2024q1`, then `fbull backtest create` again."
    )
