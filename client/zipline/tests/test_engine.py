import logging
import multiprocessing
import os

from multiprocessing.queues import Queue
from threading import Thread
from typing import Any
from typing import cast

import numpy as np
import pandas as pd
import pytest

from foreverbull_zipline.data_bundles.foreverbull import SQLIngester
from foreverbull_zipline.engine import Engine
from foreverbull_zipline.engine import benchmark_returns
from foreverbull_zipline.engine import load_benchmark_returns
from foreverbull_zipline.engine import zero_benchmark_returns
from foreverbull_zipline.service import BacktestService
from zipline.data import bundles
from zipline.utils.calendar_utils import get_calendar

from foreverbull.pb.foreverbull.backtest.service.v1 import engine_pb as engine_service_pb
from foreverbull.pb.foreverbull.backtest.v1 import backtest_pb
from foreverbull.pb.foreverbull.backtest.v1 import execution_pb
from foreverbull.pb.foreverbull.finance.v1 import marketdata_pb


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
    )

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
    )

    assert returns.index[0] == sessions[0]
    assert returns.iloc[0] == pytest.approx(102.0 / 100.0 - 1.0)


class _FakeDailyReader:
    def __init__(self, calendar, closes_by_session):
        self._calendar = calendar
        self._closes = closes_by_session

    def load_raw_arrays(self, columns, start, end, assets):
        assert columns == ["open", "close"]
        window = self._calendar.sessions_in_range(start, end)
        closes = np.array([[self._closes[session]] for session in window], dtype=float)
        return [closes.copy(), closes.copy()]


def test_load_benchmark_returns_aligns_the_reader_to_sessions():
    calendar = get_calendar("XNYS")
    sessions = _sessions("2023-01-03", "2023-01-06")
    closes = {session: 100.0 + index for index, session in enumerate(_sessions("2022-12-30", "2023-01-06"))}

    class _FakeBundle:
        equity_daily_bar_reader = _FakeDailyReader(calendar, closes)

    returns = load_benchmark_returns(_FakeBundle(), object(), calendar, sessions)
    assert returns.index[0] == sessions[0] == pd.Timestamp("2023-01-03")
    assert list(returns.index) == list(sessions)
    assert returns.iloc[0] == pytest.approx(101.0 / 100.0 - 1.0)


def test_zero_benchmark_returns_stay_on_sessions():
    sessions = _sessions("2023-01-03", "2023-01-10")
    returns = zero_benchmark_returns(sessions)

    assert list(returns.index) == list(sessions)
    assert pd.Timestamp("2023-01-07") not in returns.index
    assert bool((returns == 0.0).all())
