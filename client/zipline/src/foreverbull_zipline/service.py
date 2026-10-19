import logging
import os
import tarfile

from concurrent import futures
from contextlib import contextmanager
from dataclasses import dataclass

import exchange_calendars
import grpc
import pandas as pd

from grpc_health.v1 import health_pb2
from grpc_health.v1 import health_pb2_grpc
from zipline.data import bundles
from zipline.utils.paths import data_path
from zipline.utils.paths import data_root

from foreverbull.broker.storage import Storage
from foreverbull.pb import pb_utils
from foreverbull.pb.foreverbull.backtest.service.v1 import engine_pb as engine_service_pb
from foreverbull.pb.foreverbull.backtest.service.v1 import engine_pb_grpc as engine_service_pb_grpc
from foreverbull.pb.foreverbull.finance.service.v1 import calendar_pb as calendar_service_pb
from foreverbull.pb.foreverbull.finance.service.v1 import calendar_pb_grpc as calendar_service_pb_grpc
from foreverbull.pb.foreverbull.finance.v1 import calendar_pb
from foreverbull_zipline.data_bundles.bundle_alignment import stamp_bundle_version
from foreverbull_zipline.data_bundles.foreverbull import SQLIngester
from foreverbull_zipline.engine import ConfigError
from foreverbull_zipline.engine import Engine
from foreverbull_zipline.session_service import SessionServiceServicer


@dataclass
class Session:
    id: str
    engine: Engine
    servicer: SessionServiceServicer
    server: grpc.Server


class CalendarService(calendar_service_pb_grpc.CalendarServiceSync):
    def __init__(self):
        self.logger = logging.getLogger(__name__)

    def list_calendar_dates(self, request: calendar_service_pb.ListCalendarDatesRequest, context):
        calendar = exchange_calendars.get_calendar(request.code)
        start = pb_utils.from_proto_date_to_pydate(request.start)
        end = pb_utils.from_proto_date_to_pydate(request.end)
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

    def ingest(self, request: engine_service_pb.IngestRequest, context):
        backtest = request.backtest
        if backtest is None:
            raise ValueError("ingest request has no backtest")
        bundles.register(backtest.name, SQLIngester(backtest), calendar_name="XNYS")
        bundles.ingest(backtest.name, os.environ, pd.Timestamp.utcnow(), [], True)
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
