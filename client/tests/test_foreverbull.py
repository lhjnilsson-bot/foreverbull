import time

from concurrent import futures
from contextlib import suppress
from datetime import date
from unittest.mock import MagicMock

import grpc
import pytest

from foreverbull.foreverbull import BacktestSession
from foreverbull.foreverbull import Foreverbull
from foreverbull.foreverbull import TradingSession
from foreverbull.pb import pb_utils
from foreverbull.pb.foreverbull.backtest.service.v1 import backtest_pb_grpc as backtest_service_pb_grpc
from foreverbull.pb.foreverbull.backtest.service.v1 import session_pb as backtest_session_service_pb
from foreverbull.pb.foreverbull.backtest.service.v1 import session_pb_grpc as backtest_session_service_pb_grpc
from foreverbull.pb.foreverbull.backtest.v1 import backtest_pb
from foreverbull.pb.foreverbull.backtest.v1 import execution_pb
from foreverbull.pb.foreverbull.backtest.v1 import session_pb
from foreverbull.pb.foreverbull.finance.service.v1 import trading_pb as trading_service_pb
from foreverbull.pb.foreverbull.finance.service.v1 import trading_pb_grpc as trading_service_pb_grpc
from foreverbull.pb.foreverbull.finance.v1 import trading_pb


class TestBacktestSession:
    @pytest.fixture
    def start_grpc_server(self):
        grpc_server = grpc.server(thread_pool=futures.ThreadPoolExecutor(max_workers=1))

        def _add_servicer(backtest_session_servicer):
            backtest_session_service_pb_grpc.SessionServicerServiceSync.add_to_server(
                backtest_session_servicer, grpc_server
            )
            server_port = grpc_server.add_insecure_port("[::]:7877")
            grpc_server.start()
            time.sleep(0.5)
            stub = backtest_session_service_pb_grpc.SessionServicerClientSync(
                grpc.insecure_channel(f"localhost:{server_port}")
            )
            return stub

        try:
            yield _add_servicer
        finally:
            with suppress(Exception):
                grpc_server.stop(None)

    def test_run_execution(
        self, fb_env, fb_database, parallel_algo_file, namespace_server, start_grpc_server, monkeypatch
    ):
        monkeypatch.setenv("THREADED_EXECUTION", "1")
        model, configuration, _ = parallel_algo_file
        backtest = backtest_pb.Backtest()
        session = session_pb.Session()
        mocked_session_servicer = MagicMock(spec=backtest_session_service_pb_grpc.SessionServicerServiceSync)
        mocked_session_servicer.create_execution.return_value = backtest_session_service_pb.CreateExecutionResponse(
            execution=execution_pb.Execution(),
            configuration=configuration,
        )
        mocked_session_servicer.store_result.return_value = backtest_session_service_pb.StoreExecutionResultResponse()
        mocked_session_servicer.stop_server.return_value = backtest_session_service_pb.StopServerResponse()

        def runner(req, ctx):
            for _ in range(10):
                yield backtest_session_service_pb.RunExecutionResponse()

        mocked_session_servicer.run_execution = runner

        stub = start_grpc_server(mocked_session_servicer)
        uut = BacktestSession(model, backtest, session, stub)
        periods = uut.run_execution(
            pb_utils.from_pydate_to_proto_date(date.today()),
            pb_utils.from_pydate_to_proto_date(date.today()),
        )
        assert periods is not None
        assert len(list(periods)) == 10


class TestTradingSession:
    @pytest.fixture
    def start_grpc_server(self):
        grpc_server = grpc.server(thread_pool=futures.ThreadPoolExecutor(max_workers=1))

        def _add_servicer(trading_session_servicer):
            trading_service_pb_grpc.SessionServicerServiceSync.add_to_server(trading_session_servicer, grpc_server)
            server_port = grpc_server.add_insecure_port("[::]:7877")
            grpc_server.start()
            time.sleep(0.5)
            stub = trading_service_pb_grpc.SessionServicerClientSync(grpc.insecure_channel(f"localhost:{server_port}"))
            return stub

        try:
            yield _add_servicer
        finally:
            with suppress(Exception):
                grpc_server.stop(None)

    def test_run_trading(
        self, fb_env, fb_database, parallel_algo_file, namespace_server, start_grpc_server, monkeypatch
    ):
        monkeypatch.setenv("THREADED_EXECUTION", "1")
        model, configuration, _ = parallel_algo_file
        mocked_session_servicer = MagicMock(spec=trading_service_pb_grpc.SessionServicerServiceSync)
        mocked_session_servicer.configure.return_value = trading_service_pb.ConfigureResponse(
            configuration=configuration,
        )
        mocked_session_servicer.run.return_value = trading_service_pb.RunResponse(
            orders=[],
        )

        stub = start_grpc_server(mocked_session_servicer)
        uut = TradingSession(model, stub)
        orders, _ = uut.run_trading([])
        assert orders == []


class TestForeverbull:
    @pytest.fixture
    def backtest_grpc_server(self):
        grpc_server = grpc.server(thread_pool=futures.ThreadPoolExecutor(max_workers=1))

        def _add_servicer(backtest_servicer, backtest_session_servicer):
            backtest_service_pb_grpc.BacktestServicerServiceSync.add_to_server(backtest_servicer, grpc_server)
            if backtest_session_servicer:
                backtest_session_service_pb_grpc.SessionServicerServiceSync.add_to_server(
                    backtest_session_servicer, grpc_server
                )
            server_port = grpc_server.add_insecure_port("[::]:7877")
            grpc_server.start()
            time.sleep(1)
            return server_port

        try:
            yield _add_servicer
        finally:
            with suppress(Exception):
                grpc_server.stop(None)

    @pytest.fixture
    def trading_grpc_server(self):
        grpc_server = grpc.server(thread_pool=futures.ThreadPoolExecutor(max_workers=1))

        def _add_servicer(trading_servicer, trading_session_servicer):
            trading_service_pb_grpc.TradingServicerServiceSync.add_to_server(trading_servicer, grpc_server)
            if trading_session_servicer:
                trading_service_pb_grpc.SessionServicerServiceSync.add_to_server(trading_session_servicer, grpc_server)
            server_port = grpc_server.add_insecure_port("[::]:7877")
            grpc_server.start()
            time.sleep(1)
            return server_port

        try:
            yield _add_servicer
        finally:
            with suppress(Exception):
                grpc_server.stop(None)

    def test_backtest_session(self, parallel_algo_file, backtest_grpc_server):
        mock_backtest_session_servicer = MagicMock(spec=backtest_session_service_pb_grpc.SessionServicerServiceSync)
        mock_backtest_session_servicer.stop_server.return_value = backtest_session_service_pb.StopServerResponse()
        mock_backtest_servicer = MagicMock(spec=backtest_service_pb_grpc.BacktestServicerServiceSync)
        model, _, _ = parallel_algo_file
        algorithm = Foreverbull(model)
        port = backtest_grpc_server(mock_backtest_servicer, mock_backtest_session_servicer)
        backtest = backtest_pb.Backtest()
        backtest_session = session_pb.Session(
            port=port,
        )

        with algorithm.backtest_session(backtest, backtest_session) as algo:
            assert type(algo) is BacktestSession, "algo is not of type BacktestSession"

    def test_trading_session(self, parallel_algo_file, trading_grpc_server):
        mock_trading_session_servicer = MagicMock(spec=trading_service_pb_grpc.SessionServicerServiceSync)
        mock_trading_session_servicer.stop_server.return_value = trading_service_pb.StopServerResponse()
        mock_trading_servicer = MagicMock(spec=trading_service_pb_grpc.TradingServicerServiceSync)
        model, _, _ = parallel_algo_file
        algorithm = Foreverbull(model)
        port = trading_grpc_server(mock_trading_servicer, mock_trading_session_servicer)
        trading_session = trading_pb.Session(
            port=port,
        )
        with algorithm.trading_session(trading_session) as algo:
            assert type(algo) is TradingSession, "algo is not of type TradingSession"

    def test_get_execution_uses_resolved_broker_address(self, monkeypatch, tmp_path):
        seen: list[str] = []
        stub = MagicMock()
        stub.get_execution.return_value = MagicMock(execution=MagicMock(), periods=[])
        monkeypatch.setenv("HOME", str(tmp_path))
        monkeypatch.setenv("FOREVERBULL_BROKER_ADDRESS", "remote.example:1234")
        monkeypatch.delenv("FOREVERBULL_TOKEN", raising=False)
        from foreverbull.broker.credentials import set_cli_token

        set_cli_token(None)
        monkeypatch.setattr(
            "foreverbull.broker.credentials.grpc.secure_channel",
            lambda addr, creds: seen.append(addr) or MagicMock(),
        )
        monkeypatch.setattr(
            "foreverbull.foreverbull.backtest_service_pb_grpc.BacktestServicerClientSync",
            lambda channel: stub,
        )
        algorithm = Foreverbull(None)
        execution, frame = algorithm.get_execution("exec-1")
        assert execution is stub.get_execution.return_value.execution
        assert frame.empty
        assert seen == ["remote.example:1234"]
