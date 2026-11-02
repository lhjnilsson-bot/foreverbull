import sys
import time

from concurrent import futures
from datetime import date
from datetime import timedelta

import grpc
import pandas as pd
import pytest

from foreverbull_zipline import service
from foreverbull_zipline.data_bundles.foreverbull import SQLIngester
from sqlalchemy import create_engine
from zipline.data import bundles
from zipline.utils.calendar_utils import get_calendar

from foreverbull import exceptions
from foreverbull.pb import pb_utils
from foreverbull.pb.foreverbull import common_pb
from foreverbull.pb.foreverbull.backtest.service.v1 import engine_pb as engine_service_pb
from foreverbull.pb.foreverbull.backtest.service.v1 import engine_pb_grpc as engine_service_pb_grpc
from foreverbull.pb.foreverbull.backtest.v1 import backtest_pb
from foreverbull.pb.foreverbull.finance.service.v1 import calendar_pb as calendar_service_pb
from foreverbull.pb.foreverbull.finance.service.v1 import calendar_pb_grpc as calendar_service_pb_grpc


class TestCalendarService:
    @pytest.fixture
    def servicer(self, fb_env):
        server = grpc.server(thread_pool=futures.ThreadPoolExecutor())
        servicer = service.CalendarService()
        calendar_service_pb_grpc.CalendarServiceSync.add_to_server(servicer, server)
        server.add_insecure_port("[::]:60066")
        server.start()
        time.sleep(1)
        try:
            yield server
        finally:
            server.stop(None)

    @pytest.fixture
    def stub(self, servicer):
        return calendar_service_pb_grpc.CalendarClientSync(grpc.insecure_channel("localhost:60066"))

    def test_list_calendar_dates(self, stub: calendar_service_pb_grpc.CalendarClientSync):
        req = calendar_service_pb.ListCalendarDatesRequest(
            code="XNYS",
            start=pb_utils.from_pydate_to_proto_date(date(2020, 1, 1)),
            end=pb_utils.from_pydate_to_proto_date(date(2020, 12, 31)),
        )
        response = stub.list_calendar_dates(req)
        assert response

    def test_list_calendar_dates_leaves_out_good_friday(self, stub: calendar_service_pb_grpc.CalendarClientSync):
        # The broker reads these sessions to find missing prices, so a
        # holiday must not be a session.
        req = calendar_service_pb.ListCalendarDatesRequest(
            code="XNYS",
            start=pb_utils.from_pydate_to_proto_date(date(2024, 3, 27)),
            end=pb_utils.from_pydate_to_proto_date(date(2024, 4, 2)),
        )
        response = stub.list_calendar_dates(req)
        days = [pb_utils.from_proto_date_to_pydate(day.date) for day in response.dates]
        assert days == [date(2024, 3, 27), date(2024, 3, 28), date(2024, 4, 1), date(2024, 4, 2)]

    def test_list_calendar_dates_reaches_back_like_the_ingester(
        self, stub: calendar_service_pb_grpc.CalendarClientSync
    ):
        # exchange_calendars alone starts 20 years ago; the ingester's
        # calendar starts in 1990, and a create must not fail before that.
        req = calendar_service_pb.ListCalendarDatesRequest(
            code="XNYS",
            start=pb_utils.from_pydate_to_proto_date(date(1995, 1, 3)),
            end=pb_utils.from_pydate_to_proto_date(date(1995, 1, 6)),
        )
        response = stub.list_calendar_dates(req)
        assert len(response.dates) == 4

    @pytest.mark.parametrize("past", ["start", "end"])
    def test_list_calendar_dates_past_the_last_session_is_out_of_range(
        self, stub: calendar_service_pb_grpc.CalendarClientSync, past: str
    ):
        # Past the last loaded session, the status is OUT_OF_RANGE and names
        # that session, whichever of start and end is past it; the library's
        # own text is not sent.
        last = get_calendar("XNYS").last_session.date()
        start = last + timedelta(days=45) if past == "start" else last - timedelta(days=5)
        req = calendar_service_pb.ListCalendarDatesRequest(
            code="XNYS",
            start=pb_utils.from_pydate_to_proto_date(start),
            end=pb_utils.from_pydate_to_proto_date(last + timedelta(days=59)),
        )
        with pytest.raises(grpc.RpcError) as raised:
            stub.list_calendar_dates(req)
        assert raised.value.code() == grpc.StatusCode.OUT_OF_RANGE
        assert raised.value.details() == f"the XNYS calendar ends on {last.isoformat()}"

    def test_list_calendar_dates_through_the_last_session(self, stub: calendar_service_pb_grpc.CalendarClientSync):
        last = get_calendar("XNYS").last_session.date()
        req = calendar_service_pb.ListCalendarDatesRequest(
            code="XNYS",
            start=pb_utils.from_pydate_to_proto_date(last - timedelta(days=14)),
            end=pb_utils.from_pydate_to_proto_date(last),
        )
        response = stub.list_calendar_dates(req)
        assert pb_utils.from_proto_date_to_pydate(response.dates[-1].date) == last


class TestBacktestService:
    @pytest.fixture
    def servicer(self):
        server = grpc.server(thread_pool=futures.ThreadPoolExecutor())
        servicer = service.BacktestService()
        engine_service_pb_grpc.EngineServiceSync.add_to_server(servicer, server)
        server.add_insecure_port("[::]:60066")
        server.start()
        time.sleep(1)
        try:
            yield server
        finally:
            servicer.stop()
            server.stop(None)

    @pytest.fixture
    def stub(self, servicer):
        return engine_service_pb_grpc.EngineClientSync(grpc.insecure_channel("localhost:60066"))

    def test_ingest(
        self,
        stub: engine_service_pb_grpc.EngineClientSync,
        backtest_entity: backtest_pb.Backtest,
        fb_database,
    ):
        _, verify = fb_database
        verify(backtest_entity)
        response = stub.ingest(
            engine_service_pb.IngestRequest(
                backtest=backtest_entity,
            )
        )
        assert response

    def test_ingest_missing_driver_reports_the_driver_without_the_password(
        self,
        stub: engine_service_pb_grpc.EngineClientSync,
        monkeypatch,
        caplog,
    ):
        secret = "n0t-the-default-Pa55"
        monkeypatch.setenv("DATABASE_URL", f"postgresql+psycopg://foreverbull:{secret}@127.0.0.1:1/foreverbull")
        # sys.modules[...] = None makes the import fail the way a missing package does.
        monkeypatch.setitem(sys.modules, "psycopg", None)

        with pytest.raises(grpc.RpcError) as raised:
            stub.ingest(engine_service_pb.IngestRequest(backtest=backtest_pb.Backtest(name="missing-driver")))

        assert raised.value.code() == grpc.StatusCode.FAILED_PRECONDITION
        message = raised.value.details() or ""
        reason, _, details = message.partition(service.INGEST_DETAILS_MARKER)
        assert reason == "the data service is missing its database driver (psycopg)"
        assert details.startswith("ModuleNotFoundError:")
        assert secret not in message
        assert secret not in caplog.text
        assert "Could not connect" not in message

    def test_new_session(
        self,
        stub: engine_service_pb_grpc.EngineClientSync,
        fb_env,
    ):
        response = stub.new_session(engine_service_pb.NewSessionRequest(id="test123"))
        assert response.port


class TestIngestFailure:
    def test_missing_driver(self):
        exc = exceptions.DatabaseDriverError(
            "Database driver psycopg is not installed",
            driver="psycopg",
            cause="ModuleNotFoundError: No module named 'psycopg'",
        )
        code, details = service.ingest_failure(exc)
        assert code == grpc.StatusCode.FAILED_PRECONDITION
        assert details == (
            "the data service is missing its database driver (psycopg)"
            "\nDetails: ModuleNotFoundError: No module named 'psycopg'"
        )

    def test_connection_error_is_still_a_connection_error(self):
        exc = exceptions.ConnectionError(
            "Could not connect to database at postgresql://foreverbull:***@db.internal:5432/foreverbull",
            cause="OperationalError: connection refused",
        )
        code, details = service.ingest_failure(exc)
        assert code == grpc.StatusCode.UNAVAILABLE
        assert details == (
            "the data service could not connect to database at "
            "postgresql://foreverbull:***@db.internal:5432/foreverbull"
            "\nDetails: OperationalError: connection refused"
        )

    def test_missing_sessions_are_data_loss_with_every_gap(self):
        # The broker reads this reason and names the gaps in the create's
        # missing-session sentence, so its shape is fixed.
        import pandas as pd

        from foreverbull_zipline.data_bundles.foreverbull import MissingSessionsError

        exc = MissingSessionsError(
            {
                "AAA": list(pd.DatetimeIndex(["2024-02-14"])),
                "BBB": list(pd.DatetimeIndex(["2024-02-20", "2024-02-21", "2024-03-05"])),
            }
        )
        code, details = service.ingest_failure(exc)
        assert code == grpc.StatusCode.DATA_LOSS
        assert details.split("\nDetails: ")[0] == (
            "missing prices on trading days: AAA 2024-02-14; BBB 2024-02-20,2024-02-21,2024-03-05"
        )

    def test_other_error_is_redacted(self):
        code, details = service.ingest_failure(ValueError("bad bundle at postgresql://u:hunter22@h/db"))
        assert code == grpc.StatusCode.INTERNAL
        assert details == (
            "the data service failed while ingesting prices\nDetails: ValueError: bad bundle at postgresql://u:***@h/db"
        )


class TestIngestWithoutContext:
    def test_failed_ingest_raises_the_original_error(self, monkeypatch):
        """In-process callers pass no gRPC context; they must see the ingest error, not None.abort."""

        def failing_ingest(*args, **kwargs):
            raise RuntimeError("no bars for BRKB")

        monkeypatch.setattr(service.bundles, "ingest", failing_ingest)
        backtest = backtest_pb.Backtest(name="no-context")

        with pytest.raises(RuntimeError, match="no bars for BRKB"):
            service.BacktestService().ingest(engine_service_pb.IngestRequest(backtest=backtest), None)


class _NoBars:
    def __init__(self, provider: str = ""):
        self.provider = provider

    def get_data(self, symbol, from_date, to_date):
        return pd.DataFrame({column: [] for column in ("open", "high", "low", "close", "volume", "time")})


class _NoTickerHistory:
    def __init__(self):
        self.provider = ""
        self._engine = create_engine("sqlite://")


def _quarter_backtest() -> backtest_pb.Backtest:
    return backtest_pb.Backtest(
        name="oef-2024q1",
        benchmark="OEF",
        periods=[
            backtest_pb.Backtest.Period(
                start_date=common_pb.Date(year=2024, month=1, day=1),
                end_date=common_pb.Date(year=2024, month=3, day=31),
                assets=[backtest_pb.Backtest.Period.Asset(symbol="META", enabled=True)],
            )
        ],
    )


class TestIngestRefusalReachesTheBrokerAsWritten:
    """A refused ingest's sentence crosses gRPC unchanged, with ABORTED and no details."""

    @pytest.fixture
    def ingest_stub(self, tmp_path, monkeypatch):
        monkeypatch.setenv("ZIPLINE_ROOT", str(tmp_path))
        server = grpc.server(thread_pool=futures.ThreadPoolExecutor())
        engine_service_pb_grpc.EngineServiceSync.add_to_server(service.BacktestService(), server)
        port = server.add_insecure_port("localhost:0")
        server.start()
        try:
            yield engine_service_pb_grpc.EngineClientSync(grpc.insecure_channel(f"localhost:{port}"))
        finally:
            server.stop(None)
            if "oef-2024q1" in bundles.bundles:
                bundles.unregister("oef-2024q1")

    def test_symbols_without_bars(self, ingest_stub, monkeypatch):
        rename = pd.DataFrame(
            {
                "isin": ["US30303M1027", "US30303M1027"],
                "symbol": ["FB", "META"],
                "from_date": [date(2012, 5, 18), date(2022, 6, 9)],
                "to_date": [date(2022, 6, 9), None],
                "member_to": [None, None],
            }
        )
        monkeypatch.setattr(
            service, "SQLIngester", lambda backtest: SQLIngester(backtest, engine=_NoBars(), aliases=rename)
        )

        with pytest.raises(grpc.RpcError) as error:
            ingest_stub.ingest(engine_service_pb.IngestRequest(backtest=_quarter_backtest()))

        assert error.value.code() == grpc.StatusCode.ABORTED
        assert error.value.details() == (
            "Couldn't load data for backtest 'oef-2024q1': 2 symbols have no stored prices. The backtest wasn't saved.\n"
            "  META (earlier FB): no prices between 2024-01-02 and 2024-03-28.\n"
            "  OEF: no prices between 2024-01-02 and 2024-03-28.\n"
            "Try another provider with --provider, then run `fbull backtest create` again."
        )

    def test_ticker_history_cant_be_read(self, ingest_stub, monkeypatch):
        monkeypatch.setattr(service, "SQLIngester", lambda backtest: SQLIngester(backtest, engine=_NoTickerHistory()))

        with pytest.raises(grpc.RpcError) as error:
            ingest_stub.ingest(engine_service_pb.IngestRequest(backtest=_quarter_backtest()))

        assert error.value.code() == grpc.StatusCode.ABORTED
        assert error.value.details() == (
            "Couldn't load data for backtest 'oef-2024q1': foreverbull couldn't read its record of ticker changes, "
            "so renamed symbols can't be followed. The backtest wasn't saved. "
            "Run `fbenv status` to check that the environment is running, then run `fbull backtest create` again."
        )
        assert "Details:" not in (error.value.details() or "")
