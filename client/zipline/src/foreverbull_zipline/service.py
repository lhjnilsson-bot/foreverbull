import datetime
import logging
import os
import tarfile

from concurrent import futures
from contextlib import contextmanager
from dataclasses import dataclass

import grpc
import pandas as pd

from grpc_health.v1 import health_pb2
from grpc_health.v1 import health_pb2_grpc
from zipline.data import bundles
from zipline.utils.calendar_utils import get_calendar
from zipline.utils.paths import data_path
from zipline.utils.paths import data_root

from foreverbull import exceptions
from foreverbull.broker.storage import Storage
from foreverbull.broker.utils import describe_exception
from foreverbull.broker.utils import redact_text
from foreverbull.pb import pb_utils
from foreverbull.pb.foreverbull.backtest.service.v1 import engine_pb as engine_service_pb
from foreverbull.pb.foreverbull.backtest.service.v1 import engine_pb_grpc as engine_service_pb_grpc
from foreverbull.pb.foreverbull.finance.service.v1 import calendar_pb as calendar_service_pb
from foreverbull.pb.foreverbull.finance.service.v1 import calendar_pb_grpc as calendar_service_pb_grpc
from foreverbull.pb.foreverbull.finance.v1 import calendar_pb
from foreverbull_zipline.data_bundles.bundle_alignment import stamp_bundle_version
from foreverbull_zipline.data_bundles.foreverbull import MissingSessionsError
from foreverbull_zipline.data_bundles.foreverbull import SQLIngester
from foreverbull_zipline.engine import ConfigError
from foreverbull_zipline.engine import Engine
from foreverbull_zipline.errors import PlainSentence
from foreverbull_zipline.session_service import SessionServiceServicer


# The broker reads the first line of an ingest error as the reason and the
# line after this marker as the cause it shows on --verbose.
INGEST_DETAILS_MARKER = "\nDetails: "


def ingest_failure(exc: Exception) -> tuple[grpc.StatusCode, str]:
    """Status code and details for a failed ingest, with every password redacted.

    FAILED_PRECONDITION means the data service itself is broken (a missing
    driver), UNAVAILABLE that the database could not be reached, DATA_LOSS
    that a sid has sessions with no bar (MissingSessionsError), and ABORTED
    that the ingest was refused with a PlainSentence for the user. Anything
    else is INTERNAL. The text is redacted again here because a bundle writer or
    driver can quote a URL that did not pass through get_engine.
    """
    if isinstance(exc, PlainSentence):
        # The user's whole sentence, sent without details; the broker shows
        # it unchanged. Its cause, if any, was logged where it was raised.
        return grpc.StatusCode.ABORTED, redact_text(str(exc))
    cause = getattr(exc, "cause", None) or describe_exception(exc)
    match exc:
        case exceptions.DatabaseDriverError():
            reason = f"the data service is missing its database driver ({exc.driver})"
            code = grpc.StatusCode.FAILED_PRECONDITION
        case exceptions.ConnectionError():
            reason = f"the data service {_lower_first(str(exc))}"
            code = grpc.StatusCode.UNAVAILABLE
        case MissingSessionsError():
            # DATA_LOSS only for this: the broker reads the reason and names
            # the gaps in the create's missing-session sentence.
            reason = str(exc)
            code = grpc.StatusCode.DATA_LOSS
        case _:
            reason = "the data service failed while ingesting prices"
            code = grpc.StatusCode.INTERNAL
    return code, redact_text(f"{reason}{INGEST_DETAILS_MARKER}{cause}")


def _lower_first(text: str) -> str:
    return text[:1].lower() + text[1:]


@dataclass
class Session:
    id: str
    engine: Engine
    servicer: SessionServiceServicer
    server: grpc.Server


def calendar_end_reason(code: str, last_session: datetime.date) -> str:
    """The OUT_OF_RANGE status text for a read past the calendar: "the XNYS calendar ends on 2027-11-02".

    The broker reads the date from it, so the shape is fixed.
    """
    return f"the {code} calendar ends on {last_session.isoformat()}"


class CalendarService(calendar_service_pb_grpc.CalendarServiceSync):
    def __init__(self):
        self.logger = logging.getLogger(__name__)

    def list_calendar_dates(self, request: calendar_service_pb.ListCalendarDatesRequest, context):
        # Zipline's get_calendar, as the ingester uses: XNYS from 1990, not
        # the last 20 years only. The broker checks a create's bars against
        # these sessions, so both must agree on every day.
        calendar = get_calendar(request.code)
        start = pb_utils.from_proto_date_to_pydate(request.start)
        end = pb_utils.from_proto_date_to_pydate(request.end)
        last = calendar.last_session.date()
        if start > last or end > last:
            # The calendar is loaded about a year ahead. A read past its last
            # session is answered with that session under its own code, so
            # the broker can name it, rather than with exchange_calendars'
            # DateOutOfBounds text as an unknown error.
            self.logger.warning(
                "%s sessions from %s to %s are past the calendar's last session %s", request.code, start, end, last
            )
            context.abort(grpc.StatusCode.OUT_OF_RANGE, calendar_end_reason(request.code, last))
        dates = calendar.sessions_in_range(start, end)
        rsp = calendar_service_pb.ListCalendarDatesResponse()
        for date in dates:
            rsp.dates.append(
                calendar_pb.CalendarDate(
                    date=pb_utils.from_pydate_to_proto_date(date),
                )
            )
        return rsp


class BacktestService(engine_service_pb_grpc.EngineServiceSync, health_pb2_grpc.HealthServicer):
    def __init__(self):
        self.logger = logging.getLogger(__name__)
        self.sessions: list[Session] = []

    def download_ingestion(self, request: engine_service_pb.DownloadIngestionRequest, context):
        storage = Storage.from_environment()
        storage.download_object(request.bucket, request.object, "/tmp/ingestion.tar.gz")
        with tarfile.open("/tmp/ingestion.tar.gz", "r:gz") as tar:
            tar.extractall(data_root())
        return engine_service_pb.DownloadIngestionResponse()

    def ingest(self, request: engine_service_pb.IngestRequest, context: grpc.ServicerContext | None):
        backtest = request.backtest
        if backtest is None:
            raise ValueError("ingest request has no backtest")
        try:
            ingester = SQLIngester(backtest)
            bundles.register(backtest.name, ingester, calendar_name="XNYS")
            bundles.ingest(backtest.name, os.environ, pd.Timestamp.utcnow(), [], True)
        except Exception as exc:
            code, details = ingest_failure(exc)
            self.logger.error("Ingesting %s failed: %s", backtest.name, details)
            if context is None:
                # Called in-process (tests, local tools) with no gRPC context: keep the real error.
                raise
            context.abort(code, details)
        stamp_bundle_version(backtest.name, os.environ)
        self.bundle = bundles.load(backtest.name, os.environ, None)
        self.logger.debug("ingestion completed")
        if request.has_field("bucket") and request.has_field("object"):
            self.logger.debug("Uploading ingestion to: %s/%s", request.bucket, request.object)
            with tarfile.open(f"/tmp/{backtest.name}.tar.gz", "w:gz") as tar:
                tar.add(data_path([backtest.name]), arcname=backtest.name)
            storage = Storage.from_environment()
            storage.upload_object(request.bucket, request.object, f"/tmp/{backtest.name}.tar.gz")
            self.logger.debug("Ingestion uploaded")
        return engine_service_pb.IngestResponse()

    def new_session(self, request: engine_service_pb.NewSessionRequest, context):
        engine = Engine(
            socket_file_path=f"/tmp/fb_{request.id}.sock",
        )
        engine.start()
        if not engine.is_ready.wait(5.0):
            raise ConfigError("Timeout when creating engine")
        server = grpc.server(thread_pool=futures.ThreadPoolExecutor())
        servicer = SessionServiceServicer(engine)
        engine_service_pb_grpc.EngineSessionServiceSync.add_to_server(servicer, server)
        port = server.add_insecure_port("[::]:0")
        server.start()
        self.sessions.append(Session(request.id, servicer.engine, servicer, server))
        return engine_service_pb.NewSessionResponse(port=port)

    def Check(self, request, context):
        return health_pb2.HealthCheckResponse(status=health_pb2.HealthCheckResponse.SERVING)

    def Watch(self, request, context):
        return self.Check(request, context)

    def stop(self):
        for session in self.sessions:
            session.server.stop(None)
            session.engine.stop()
            session.engine.join()


@contextmanager
def grpc_server(port=50055):
    server = grpc.server(thread_pool=futures.ThreadPoolExecutor())
    bs = BacktestService()
    cs = CalendarService()
    engine_service_pb_grpc.EngineServiceSync.add_to_server(bs, server)
    calendar_service_pb_grpc.CalendarServiceSync.add_to_server(cs, server)
    health_pb2_grpc.add_HealthServicer_to_server(bs, server)
    server.add_insecure_port(f"[::]:{port}")
    server.start()
    yield server
    server.stop(None)
