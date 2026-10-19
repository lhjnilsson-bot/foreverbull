import functools
import logging
import logging.handlers
import math
import multiprocessing
import multiprocessing.queues
import os

import pandas as pd
import pynng
import six

from protobuf import merge_from_binary
from zipline.data import bundles
from zipline.data.bar_reader import NoDataOnDate
from zipline.data.data_portal import DataPortal
from zipline.extensions import load
from zipline.finance import metrics
from zipline.finance.blotter import Blotter
from zipline.finance.trading import SimulationParameters
from zipline.protocol import BarData
from zipline.protocol import Portfolio
from zipline.utils.calendar_utils import get_calendar

from foreverbull.broker.storage import Storage
from foreverbull.pb import pb_utils
from foreverbull.pb.foreverbull import common_pb
from foreverbull.pb.foreverbull.backtest.service.v1 import engine_pb as engine_service_pb
from foreverbull.pb.foreverbull.backtest.v1 import execution_pb
from foreverbull.pb.foreverbull.finance.v1 import marketdata_pb
from zipline import TradingAlgorithm


class ConfigError(Exception):
    pass


class StopExcecution(Exception):
    pass


def zero_benchmark_returns(sessions: pd.DatetimeIndex) -> pd.Series:
    """Flat benchmark on the simulation sessions, not on calendar dates."""
    return pd.Series(index=sessions, data=0.0)


def benchmark_returns(window_sessions, sessions, opens, closes) -> pd.Series:
    """Benchmark daily returns on Zipline session labels.

    ``window_sessions`` may hold the session before ``sessions`` so the
    first return is that close against the previous close. Without a
    usable previous close the first return is the first session's open
    to close, as Zipline does for an asset that starts on that session.
    Passing the benchmark as a sid instead lets Zipline compare the
    asset start (the first bar, stamped at the session open) against
    the session label and refuse the run, so the series is built here.
    """
    session_index = pd.DatetimeIndex(sessions)
    if len(session_index) == 0:
        return zero_benchmark_returns(session_index)
    window_index = pd.DatetimeIndex(window_sessions)
    close_series = pd.Series(data=[float(value) for value in closes], index=window_index, dtype=float)
    open_series = pd.Series(data=[float(value) for value in opens], index=window_index, dtype=float)
    returns = close_series.pct_change().reindex(session_index)
    first_return = float(returns.iloc[0])
    if not math.isfinite(first_return):
        first_open = float(open_series[session_index[0]])
        first_close = float(close_series[session_index[0]])
        returns.iloc[0] = (first_close - first_open) / first_open if first_open > 0 else 0.0
    returns = returns.fillna(0.0)
    return returns.replace([float("inf"), float("-inf")], 0.0)


def load_benchmark_returns(bundle, benchmark_asset, trading_calendar, sessions) -> pd.Series:
    """Read the benchmark's daily bars from the bundle on session labels."""
    if len(sessions) == 0:
        return zero_benchmark_returns(sessions)
    reader = bundle.equity_daily_bar_reader
    window_start = sessions[0]
    if sessions[0] > trading_calendar.first_session:
        window_start = trading_calendar.previous_session(sessions[0])
    window_sessions = trading_calendar.sessions_in_range(window_start, sessions[-1])
    if len(window_sessions) == 0 or window_sessions[0] > sessions[0]:
        window_sessions = sessions
    try:
        opens, closes = reader.load_raw_arrays(
            ["open", "close"], window_sessions[0], window_sessions[-1], [benchmark_asset]
        )
    except NoDataOnDate:
        opens, closes = reader.load_raw_arrays(["open", "close"], sessions[0], sessions[-1], [benchmark_asset])
        window_sessions = sessions
    return benchmark_returns(window_sessions, sessions, opens[:, 0], closes[:, 0])


class Engine(multiprocessing.Process):
    def __init__(
        self,
        socket_file_path: str = "/tmp/foreverbull_zipline.sock",
        logging_queue: multiprocessing.queues.Queue | None = None,
    ):
        self._socket_file_path = socket_file_path
        self._logging_queue = logging_queue
        self.logger = logging.getLogger().getChild(__name__)
        self.is_ready = multiprocessing.Event()
        super().__init__()

    def run_backtest(self, backtest: engine_service_pb.RunBacktestRequest) -> engine_service_pb.RunBacktestResponse:
        with pynng.Req0(
            dial=f"ipc://{self._socket_file_path}",
            block_on_dial=False,
            recv_timeout=10_000,
            send_timeout=10_000,
        ) as socket:
            data = backtest.to_binary()
            request = common_pb.Request(task="run", data=data)
            socket.send(request.to_binary())
            response = common_pb.Response()
            merge_from_binary(response, socket.recv())
            if response.has_field("error"):
                raise SystemError(response.error)
            b = engine_service_pb.RunBacktestResponse()
            merge_from_binary(b, response.data)
            return b

    def get_current_period(
        self, req: engine_service_pb.GetCurrentPeriodRequest
    ) -> engine_service_pb.GetCurrentPeriodResponse:
        with pynng.Req0(
            dial=f"ipc://{self._socket_file_path}",
            block_on_dial=False,
            recv_timeout=10_000,
            send_timeout=10_000,
        ) as socket:
            data = req.to_binary()
            request = common_pb.Request(task="get_current_period", data=data)
            socket.send(request.to_binary())
            response = common_pb.Response()
            merge_from_binary(response, socket.recv())
            if response.has_field("error"):
                raise SystemError(response.error)
            p = engine_service_pb.GetCurrentPeriodResponse()
            merge_from_binary(p, response.data)
            return p

    def place_orders_and_continue(
        self, req: engine_service_pb.PlaceOrdersAndContinueRequest
    ) -> engine_service_pb.PlaceOrdersAndContinueResponse:
        with pynng.Req0(
            dial=f"ipc://{self._socket_file_path}",
            block_on_dial=False,
            recv_timeout=10_000,
            send_timeout=10_000,
        ) as socket:
            data = req.to_binary()
            request = common_pb.Request(task="place_orders_and_continue", data=data)
            socket.send(request.to_binary())
            response = common_pb.Response()
            merge_from_binary(response, socket.recv())
            if response.has_field("error"):
                raise SystemError(response.error)
            p = engine_service_pb.PlaceOrdersAndContinueResponse()
            merge_from_binary(p, response.data)
            return p

    def get_backtest_result(self, req: engine_service_pb.GetResultRequest) -> engine_service_pb.GetResultResponse:
        with pynng.Req0(
            dial=f"ipc://{self._socket_file_path}",
            block_on_dial=False,
            recv_timeout=10_000,
            send_timeout=10_000,
        ) as socket:
            request = common_pb.Request(task="get_result", data=req.to_binary())
            socket.send(request.to_binary())
            response = common_pb.Response()
            merge_from_binary(response, socket.recv())
            if response.has_field("error"):
                raise SystemError(response.error)
            result = engine_service_pb.GetResultResponse()
            merge_from_binary(result, response.data)
            return result

    def stop(self):
        with pynng.Req0(
            dial=f"ipc://{self._socket_file_path}",
            block_on_dial=False,
            recv_timeout=10_000,
            send_timeout=10_000,
        ) as socket:
            request = common_pb.Request(task="stop")
            socket.send(request.to_binary())

    def run(self):
        if self._logging_queue is not None:
            handler = logging.handlers.QueueHandler(self._logging_queue)
            logging.basicConfig(handlers=[handler], level=logging.DEBUG)
        self.log = logging.getLogger().getChild(__name__)
        self.log.info("Starting Execution Process")
        self._trading_algorithm: TradingAlgorithm | None = None
        self.logger = logging.getLogger().getChild(__name__)

        if os.path.exists(self._socket_file_path):
            print("Removing old socket file", self._socket_file_path)
            os.remove(self._socket_file_path)
        with pynng.Rep0(
            listen=f"ipc://{self._socket_file_path}",
            block_on_dial=False,
            send_timeout=1_000,
        ) as socket:
            self.is_ready.set()
            while True:
                try:
                    self._process_request(None, None, socket)
                except (StopExcecution, KeyboardInterrupt):
                    break

    def analyze(self, _, result):
        self.result = result

    def _run(self, data: bytes, socket: pynng.Socket) -> tuple[TradingAlgorithm, bytes]:
        req = engine_service_pb.RunBacktestRequest()
        merge_from_binary(req, data)
        execution = req.execution
        if execution is None:
            raise ValueError("missing execution")
        start = execution.start_date
        end = execution.end_date
        if start is None or end is None:
            raise ValueError("missing execution dates")
        bundles.register(execution.backtest, None, calendar_name="XNYS")
        bundle = bundles.load(execution.backtest, os.environ, None)

        start_date = pd.Timestamp(
            start.year,
            start.month,
            start.day,
        )
        end_date = pd.Timestamp(
            end.year,
            end.month,
            end.day,
        )

        trading_calendar = get_calendar("XNYS")
        sessions = trading_calendar.sessions_in_range(start_date, end_date)  # type: ignore[arg-type]

        benchmark_sid = None
        if req.benchmark:
            benchmark_asset = bundle.asset_finder.lookup_symbol(req.benchmark, as_of_date=None)
            stored_benchmark_returns = load_benchmark_returns(bundle, benchmark_asset, trading_calendar, sessions)
        else:
            stored_benchmark_returns = zero_benchmark_returns(sessions)

        data_portal = DataPortal(
            bundle.asset_finder,
            trading_calendar=trading_calendar,
            first_trading_day=bundle.equity_minute_bar_reader.first_trading_day,
            equity_minute_reader=bundle.equity_minute_bar_reader,
            equity_daily_reader=bundle.equity_daily_bar_reader,
            adjustment_reader=bundle.adjustment_reader,
        )
        sim_params = SimulationParameters(
            start_session=start_date,
            end_session=end_date,
            trading_calendar=trading_calendar,
            data_frequency="daily",
        )
        num_periods = int(sessions.size)  # type: ignore
        metrics_set = "default"
        blotter = "default"
        if isinstance(metrics_set, six.string_types):
            try:
                metrics_set = metrics.load(metrics_set)
            except ValueError as e:
                self.logger.error("Error configuring metrics: %s", repr(e))
                raise ConfigError(repr(e))

        if isinstance(blotter, six.string_types):
            try:
                blotter = load(Blotter, blotter)
            except ValueError as e:
                self.logger.error("Error configuring blotter: %s", repr(e))
                raise ConfigError(repr(e))

        handle_data = functools.partial(self._process_request, socket=socket)
        trading_algorithm = TradingAlgorithm(
            namespace={},
            data_portal=data_portal,
            trading_calendar=trading_calendar,
            sim_params=sim_params,
            metrics_set=metrics_set,
            blotter=blotter,
            benchmark_returns=stored_benchmark_returns,
            benchmark_sid=benchmark_sid,
            handle_data=handle_data,
            analyze=self.analyze,
        )
        return (
            trading_algorithm,
            engine_service_pb.RunBacktestResponse(
                execution=req.execution,
                num_periods=num_periods,
            ).to_binary(),
        )

    def _place_orders(self, data: bytes, trading_algorithm: TradingAlgorithm) -> bytes:
        req = engine_service_pb.PlaceOrdersAndContinueRequest()
        merge_from_binary(req, data)
        for order in req.orders:
            self.log.info("Placing order: %s", order)
            asset = trading_algorithm.symbol(order.symbol)
            order_id = trading_algorithm.order(asset=asset, amount=order.amount)
            self.log.debug("Placed order: %s", order_id)

        return engine_service_pb.PlaceOrdersAndContinueResponse().to_binary()

    def _get_current_period(self, data: bytes, trading_algorithm: TradingAlgorithm) -> bytes:
        req = engine_service_pb.GetCurrentPeriodRequest()
        merge_from_binary(req, data)
        p: Portfolio = trading_algorithm.portfolio
        return engine_service_pb.GetCurrentPeriodResponse(
            is_running=True,
            portfolio=marketdata_pb.Portfolio(
                timestamp=pb_utils.to_proto_timestamp(trading_algorithm.datetime),
                cash_flow=p.cash_flow,  # type: ignore
                starting_cash=p.starting_cash,  # type: ignore
                portfolio_value=p.portfolio_value,  # type: ignore
                pnl=p.pnl,  # type: ignore
                returns=p.returns,  # type: ignore
                cash=p.cash,  # type: ignore
                positions_value=p.positions_value,  # type: ignore
                positions_exposure=p.positions_exposure,  # type: ignore
                positions=[
                    marketdata_pb.Position(
                        symbol=p.sid.symbol,
                        amount=int(p.amount),
                        cost_basis=p.cost_basis,
                        last_sale_price=p.last_sale_price,
                        last_sale_date=pb_utils.to_proto_timestamp(p.last_sale_date),
                    )
                    for _, p in p.positions.items()  # type: ignore
                ],
            ),
        ).to_binary()

    def _get_execution_result(self, data: bytes) -> bytes:
        req = engine_service_pb.GetResultRequest()
        merge_from_binary(req, data)
        assert self.result is not None, "no result"
        assert type(self.result) is pd.DataFrame, "result is not a dataframe"
        rsp = engine_service_pb.GetResultResponse()
        for row in self.result.index:
            period = self.result.loc[row]
            rsp.periods.append(
                execution_pb.Period(
                    date=pb_utils.from_pydate_to_proto_date(period["period_close"].to_pydatetime().replace()),
                    PNL=period["pnl"],
                    returns=period["returns"],
                    portfolio_value=period["portfolio_value"],
                    longs_count=int(period["longs_count"]),
                    shorts_count=int(period["shorts_count"]),
                    long_value=period["long_value"],
                    short_value=period["short_value"],
                    starting_exposure=period["starting_exposure"],
                    ending_exposure=period["ending_exposure"],
                    long_exposure=period["long_exposure"],
                    short_exposure=period["short_exposure"],
                    capital_used=period["capital_used"],
                    gross_leverage=period["gross_leverage"],
                    net_leverage=period["net_leverage"],
                    starting_value=period["starting_value"],
                    ending_value=period["ending_value"],
                    starting_cash=period["starting_cash"],
                    ending_cash=period["ending_cash"],
                    max_drawdown=period["max_drawdown"],
                    max_leverage=period["max_leverage"],
                    excess_return=period["excess_return"],
                    treasury_period_return=period["treasury_period_return"],
                    algorithm_period_return=period["algorithm_period_return"],
                    algo_volatility=(None if pd.isnull(period["algo_volatility"]) else period["algo_volatility"]),
                    sharpe=None if pd.isnull(period["sharpe"]) else period["sharpe"],
                    sortino=None if pd.isnull(period["sortino"]) else period["sortino"],
                    benchmark_period_return=(
                        None if pd.isnull(period["benchmark_period_return"]) else period["benchmark_period_return"]
                    ),
                    benchmark_volatility=(
                        None if pd.isnull(period["benchmark_volatility"]) else period["benchmark_volatility"]
                    ),
                    alpha=(None if period["alpha"] is None or pd.isnull(period["alpha"]) else period["alpha"]),
                    beta=(None if period["beta"] is None or pd.isnull(period["beta"]) else period["beta"]),
                )
            )
        upload = req.upload
        if upload is not None:
            storage = Storage.from_environment()
            self.result.to_pickle("/tmp/result.pkl")
            storage.upload_object(upload.bucket, upload.object, "/tmp/result.pkl")
        return rsp.to_binary()

    def _process_request(
        self,
        trading_algorithm: TradingAlgorithm | None,
        bar_data: BarData | None,
        socket: pynng.Socket,
    ):
        while True:
            with socket.new_context() as context_socket:
                req = common_pb.Request()
                merge_from_binary(req, context_socket.recv())
                self.log.debug(f"Received request {req.task}, {id(trading_algorithm)}")
                rsp = common_pb.Response(task=req.task)
                data: bytes | None = None
                try:
                    match req.task:
                        case "run":
                            if trading_algorithm is not None:
                                rsp.error = "Execution already running"
                                context_socket.send(rsp.to_binary())
                            else:
                                ta, data = self._run(req.data, socket)
                                rsp.data = data
                                context_socket.send(rsp.to_binary())
                                ta.run()
                                return
                        case "get_current_period":
                            if trading_algorithm is None:
                                rsp.data = engine_service_pb.GetCurrentPeriodResponse(is_running=False).to_binary()
                                context_socket.send(rsp.to_binary())
                            else:
                                rsp.data = self._get_current_period(req.data, trading_algorithm)
                                context_socket.send(rsp.to_binary())
                        case "place_orders_and_continue":
                            if trading_algorithm is None:
                                rsp.error = "Execution not running"
                                context_socket.send(rsp.to_binary())
                            else:
                                rsp.data = self._place_orders(req.data, trading_algorithm)
                                context_socket.send(rsp.to_binary())
                                return
                        case "stop":
                            context_socket.send(rsp.to_binary())
                            raise StopExcecution()
                        case "get_result":
                            rsp.data = self._get_execution_result(req.data)
                            context_socket.send(rsp.to_binary())
                except StopExcecution:
                    raise
                except Exception as e:
                    self.log.exception("Error processing request %s", req.task)
                    rsp.error = str(e)
                    context_socket.send(rsp.to_binary())
