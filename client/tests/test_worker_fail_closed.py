import os
import stat
import threading
import time

from multiprocessing import Queue
from typing import ClassVar

import pynng
import pytest

from foreverbull import exceptions
from foreverbull import worker
from foreverbull.pb.foreverbull import common_pb
from foreverbull.pb.foreverbull.service.v1 import worker_pb


HINT = "Check the worker output above for an error in your algo."


def _pool_with(surveyor: pynng.Surveyor0, workers: int) -> worker.WorkerPool:
    pool = worker.WorkerPool("unused", executors=workers)
    pool._worker_surveyor_socket = surveyor
    pool._workers = [threading.Thread() for _ in range(workers)]
    return pool


def _answer_once(address: str, ready: threading.Event, error: str | None) -> None:
    with pynng.Respondent0(dial=address, block_on_dial=True, recv_timeout=2000, send_timeout=2000) as responder:
        ready.set()
        responder.recv()
        responder.send(common_pb.Response(task="configure", error=error).to_binary())


def _broadcast(tmp_path, workers: int, answers: list[str | None], timeout_ms: int = 300) -> None:
    """Broadcast a configure request to a pool of `workers`, where only len(answers) workers answer."""
    address = f"ipc://{tmp_path}/survey.ipc"
    with pynng.Surveyor0(listen=address, send_timeout=timeout_ms, recv_timeout=timeout_ms) as surveyor:
        surveyor.survey_time = timeout_ms
        responders = []
        for error in answers:
            ready = threading.Event()
            responder = threading.Thread(target=_answer_once, args=(address, ready, error))
            responder.start()
            assert ready.wait(2)
            responders.append(responder)
        # A dialled responder can still be attaching on the surveyor's side, and a
        # survey sent before then never reaches it. Wait until every pipe is in.
        deadline = time.monotonic() + 2
        while len(surveyor.pipes) < len(answers):
            assert time.monotonic() < deadline, "responders didn't connect"
            time.sleep(0.005)
        try:
            _pool_with(surveyor, workers=workers)._broadcast(common_pb.Request(task="configure"))
        finally:
            for responder in responders:
                responder.join(timeout=2)


class TestBroadcastFailureMessage:
    def test_configure_timeout(self):
        assert worker._broadcast_failure("configure", 2, [], 1) == (
            "1 of 2 workers didn't finish setting up within 30s. Check the worker output above for an error in your algo."
        )

    def test_run_timeout(self):
        assert worker._broadcast_failure("run", 2, [], 1) == (
            "1 of 2 workers didn't start the backtest within 30s. Check the worker output above for an error in your algo."
        )

    def test_single_worker_timeout_is_singular(self):
        assert worker._broadcast_failure("run", 1, [], 1) == (
            "1 of 1 worker didn't start the backtest within 30s. Check the worker output above for an error in your algo."
        )

    def test_one_error(self):
        assert worker._broadcast_failure("configure", 3, ["ValueError: boom"], 0) == (
            "1 of 3 workers couldn't finish setting up: ValueError: boom. "
            "Check the worker output above for an error in your algo."
        )

    def test_one_error_with_a_trailing_period(self):
        assert worker._broadcast_failure("configure", 3, ["ValueError: boom."], 0) == (
            "1 of 3 workers couldn't finish setting up: ValueError: boom. "
            "Check the worker output above for an error in your algo."
        )

    def test_block_error_with_a_trailing_period(self):
        assert worker._broadcast_failure("configure", 2, ["ValueError: boom."], 1) == (
            "2 of 2 workers couldn't finish setting up:\n"
            "  A worker reported: ValueError: boom.\n"
            "  1 worker didn't answer within 30s.\n"
            "Check the worker output above for an error in your algo."
        )

    def test_two_errors(self):
        assert worker._broadcast_failure("configure", 3, ["ValueError: boom", "KeyError: 'x'"], 0) == (
            "2 of 3 workers couldn't finish setting up:\n"
            "  A worker reported: ValueError: boom.\n"
            "  A worker reported: KeyError: 'x'.\n"
            "Check the worker output above for an error in your algo."
        )

    def test_error_and_timeout(self):
        assert worker._broadcast_failure("configure", 3, ["ValueError: boom"], 1) == (
            "2 of 3 workers couldn't finish setting up:\n"
            "  A worker reported: ValueError: boom.\n"
            "  1 worker didn't answer within 30s.\n"
            "Check the worker output above for an error in your algo."
        )

    def test_run_error_and_timeouts(self):
        assert worker._broadcast_failure("run", 4, ["ConfigurationError: Worker not configured"], 2) == (
            "3 of 4 workers couldn't start the backtest:\n"
            "  A worker reported: ConfigurationError: Worker not configured.\n"
            "  2 workers didn't answer within 30s.\n"
            "Check the worker output above for an error in your algo."
        )


def test_broadcast_names_workers_that_did_not_answer(monkeypatch, tmp_path):
    monkeypatch.setattr(worker, "SURVEY_TIMEOUT_MS", 300)
    with pytest.raises(RuntimeError) as exc:
        _broadcast(tmp_path, workers=2, answers=[None])
    assert str(exc.value) == f"1 of 2 workers didn't finish setting up within 0.3s. {HINT}"


def test_broadcast_returns_when_every_worker_answers(monkeypatch, tmp_path):
    monkeypatch.setattr(worker, "SURVEY_TIMEOUT_MS", 1000)
    _broadcast(tmp_path, workers=1, answers=[None], timeout_ms=1000)


def test_broadcast_waits_for_every_answer_before_raising_an_error(monkeypatch, tmp_path):
    monkeypatch.setattr(worker, "SURVEY_TIMEOUT_MS", 1000)
    with pytest.raises(exceptions.ConfigurationError) as exc:
        _broadcast(tmp_path, workers=3, answers=["ValueError: boom", "KeyError: 'x'", None], timeout_ms=1000)
    lines = str(exc.value).splitlines()
    assert lines[0] == "2 of 3 workers couldn't finish setting up:"
    assert sorted(lines[1:3]) == ["  A worker reported: KeyError: 'x'.", "  A worker reported: ValueError: boom."]
    assert lines[3:] == [HINT]


def test_broadcast_reports_an_error_and_a_timeout_together(monkeypatch, tmp_path):
    monkeypatch.setattr(worker, "SURVEY_TIMEOUT_MS", 300)
    with pytest.raises(exceptions.ConfigurationError) as exc:
        _broadcast(tmp_path, workers=3, answers=["ValueError: boom", None])
    assert str(exc.value) == (
        "2 of 3 workers couldn't finish setting up:\n"
        "  A worker reported: ValueError: boom.\n"
        "  1 worker didn't answer within 0.3s.\n"
        f"{HINT}"
    )


def test_daemon_raises_when_surveyor_is_unreachable(tmp_path):
    path = f"{tmp_path}/nobody.ipc"
    address = f"ipc://{path}"
    running = threading.Event()
    daemon = worker.WorkerDaemon(address, Queue(), running, threading.Event(), "unused")
    with pytest.raises(exceptions.ConnectionError) as exc:
        daemon.run()
    assert str(exc.value) == (
        f"A worker couldn't connect to its worker pool at {path}: Connection refused. "
        "This is a problem in fbull, not in your algo. "
        "Please report it at https://github.com/lhjnilsson/foreverbull/issues with this message."
    )
    assert exc.value.cause == "ConnectionRefused: Connection refused"
    assert not running.is_set()


class _UnlistenedSurveyor:
    """Stands in for the pool's surveyor without listening, so every worker's dial is refused."""

    instances: ClassVar[list["_UnlistenedSurveyor"]] = []

    def __init__(self, **_kwargs):
        self.survey_time = 0
        self.closed = False
        _UnlistenedSurveyor.instances.append(self)

    def close(self) -> None:
        self.closed = True


# In thread mode the failed worker's ConnectionError surfaces as an unhandled thread exception, as intended.
@pytest.mark.filterwarnings("ignore::pytest.PytestUnhandledThreadExceptionWarning")
@pytest.mark.parametrize("threaded", [True, False], ids=["thread", "process"])
def test_pool_names_why_a_worker_could_not_start(monkeypatch, tmp_path, threaded):
    if threaded:
        monkeypatch.setenv("THREADED_EXECUTION", "1")
    else:
        monkeypatch.delenv("THREADED_EXECUTION", raising=False)
    monkeypatch.setattr(worker.models.Algorithm, "from_file_path", lambda _path: None)
    monkeypatch.setattr(worker.pynng, "Surveyor0", _UnlistenedSurveyor)
    _UnlistenedSurveyor.instances.clear()
    pool = worker.WorkerPool("unused", executors=2)
    started = time.monotonic()
    with pytest.raises(exceptions.ConfigurationError) as exc:
        pool.__enter__()
    # The pool saw the worker fail; it didn't sit out the start timeout.
    assert time.monotonic() - started < worker.WORKER_START_TIMEOUT_SECONDS
    socket_path = pool._worker_surveyor_address.removeprefix("ipc://")
    assert str(exc.value) == (
        f"A worker couldn't connect to its worker pool at {socket_path}: Connection refused. "
        "This is a problem in fbull, not in your algo. "
        "Please report it at https://github.com/lhjnilsson/foreverbull/issues with this message."
    )
    # The second worker never started, the first one is stopped, and the socket is closed.
    assert len(pool._workers) == 1
    assert not pool._workers[0].is_alive()
    assert [s.closed for s in _UnlistenedSurveyor.instances] == [True]
    assert not os.path.exists(os.path.dirname(socket_path))


def test_pool_keeps_a_survey_open_for_the_whole_timeout(monkeypatch, tmp_path):
    # nng closes a survey after 1s by default; a worker that answers later must still count.
    monkeypatch.setenv("THREADED_EXECUTION", "1")
    monkeypatch.setattr(worker.models.Algorithm, "from_file_path", lambda _path: None)

    def slow_configure(self, req):
        time.sleep(1.5)

    monkeypatch.setattr(worker.WorkerDaemon, "configure_execution", slow_configure)
    pool = worker.WorkerPool("unused", executors=1)
    with pool:
        assert pool._worker_surveyor_socket.survey_time == worker.SURVEY_TIMEOUT_MS
        pool.configure_execution(worker_pb.ExecutionConfiguration())


def test_daemon_reports_an_error_as_type_and_message(monkeypatch, tmp_path):
    monkeypatch.setattr(worker, "SURVEY_TIMEOUT_MS", 2000)
    address = f"ipc://{tmp_path}/survey.ipc"
    stop = threading.Event()
    running = threading.Event()

    def boom(self, req):
        raise ValueError("boom")

    monkeypatch.setattr(worker.WorkerDaemon, "configure_execution", boom)
    with pynng.Surveyor0(listen=address, send_timeout=2000, recv_timeout=2000) as surveyor:
        surveyor.survey_time = 2000
        daemon = worker.WorkerDaemon(address, Queue(), running, stop, "unused")
        thread = threading.Thread(target=daemon.run)
        thread.start()
        try:
            assert running.wait(2)
            pool = _pool_with(surveyor, workers=1)
            with pytest.raises(exceptions.ConfigurationError) as exc:
                pool._broadcast(
                    common_pb.Request(task="configure", data=worker_pb.ExecutionConfiguration().to_binary())
                )
        finally:
            stop.set()
            thread.join(timeout=5)
    assert str(exc.value) == (
        "1 of 1 worker couldn't finish setting up: ValueError: boom. "
        "Check the worker output above for an error in your algo."
    )


def _threaded_pool(monkeypatch) -> worker.WorkerPool:
    monkeypatch.setenv("THREADED_EXECUTION", "1")
    monkeypatch.setattr(worker.models.Algorithm, "from_file_path", lambda _path: None)
    monkeypatch.setattr(worker.WorkerDaemon, "configure_execution", lambda self, req: None)
    return worker.WorkerPool("unused", executors=1)


def test_pool_socket_is_in_a_private_directory_removed_on_exit(monkeypatch):
    pool = _threaded_pool(monkeypatch)
    with pool:
        socket_path = pool._worker_surveyor_address.removeprefix("ipc://")
        directory = os.path.dirname(socket_path)
        assert os.path.basename(socket_path) == worker.SOCKET_NAME
        assert stat.S_IMODE(os.stat(directory).st_mode) == 0o700
        assert os.stat(directory).st_uid == os.getuid()
    assert not os.path.exists(directory)
    assert pool._socket_dir is None


def test_two_pools_run_side_by_side(monkeypatch):
    first, second = _threaded_pool(monkeypatch), _threaded_pool(monkeypatch)
    with first, second:
        assert first._worker_surveyor_address != second._worker_surveyor_address
        first.configure_execution(worker_pb.ExecutionConfiguration())
        second.configure_execution(worker_pb.ExecutionConfiguration())


def test_pool_removes_its_directory_when_listen_fails(monkeypatch):
    monkeypatch.setattr(worker.models.Algorithm, "from_file_path", lambda _path: None)
    created: list[str] = []
    real_mkdtemp = worker.tempfile.mkdtemp

    def mkdtemp(**kwargs):
        created.append(real_mkdtemp(**kwargs))
        return created[-1]

    def refuse(**_kwargs):
        raise pynng.exceptions.AddressInUse("Address in use", 10)

    monkeypatch.setattr(worker.tempfile, "mkdtemp", mkdtemp)
    monkeypatch.setattr(worker.pynng, "Surveyor0", refuse)
    with pytest.raises(pynng.exceptions.AddressInUse):
        worker.WorkerPool("unused", executors=1).__enter__()
    assert len(created) == 1
    assert not os.path.exists(created[0])


# macOS caps a unix socket path (sun_path) at 104 bytes, and its $TMPDIR is about 49 characters,
# e.g. /var/folders/xx/xxxxxxxxxxxxxxxxxxxxxxxxxxxx/T/.
UNIX_SOCKET_PATH_LIMIT = 104
MACOS_TMPDIR_LENGTH = 49


def test_pool_socket_path_fits_a_long_macos_tmpdir(monkeypatch, tmp_path):
    tmpdir = tmp_path / "T"
    tmpdir.mkdir()
    monkeypatch.setattr(worker.tempfile, "tempdir", str(tmpdir))
    pool = _threaded_pool(monkeypatch)
    with pool:
        socket_path = pool._worker_surveyor_address.removeprefix("ipc://")
        assert os.path.dirname(os.path.dirname(socket_path)) == str(tmpdir)
        # What the pool adds after $TMPDIR: "/fbull-XXXXXXXX/s.ipc".
        added = len(socket_path.encode()) - len(str(tmpdir).encode())
    assert MACOS_TMPDIR_LENGTH + added < UNIX_SOCKET_PATH_LIMIT


def test_pool_socket_path_is_under_the_unix_socket_limit(monkeypatch):
    pool = _threaded_pool(monkeypatch)
    with pool:
        socket_path = pool._worker_surveyor_address.removeprefix("ipc://")
        assert len(socket_path.encode()) < UNIX_SOCKET_PATH_LIMIT


def test_pool_keeps_the_listen_error_when_cleanup_also_fails(monkeypatch):
    monkeypatch.setattr(worker.models.Algorithm, "from_file_path", lambda _path: None)
    created: list[str] = []
    real_mkdtemp = worker.tempfile.mkdtemp

    def mkdtemp(**kwargs):
        created.append(real_mkdtemp(**kwargs))
        return created[-1]

    def refuse(**_kwargs):
        raise pynng.exceptions.AddressInUse("Address in use", 10)

    def fail_rmtree(path):
        raise PermissionError(13, "Permission denied", path)

    monkeypatch.setattr(worker.tempfile, "mkdtemp", mkdtemp)
    monkeypatch.setattr(worker.pynng, "Surveyor0", refuse)
    monkeypatch.setattr(worker.shutil, "rmtree", fail_rmtree)
    try:
        with pytest.raises(pynng.exceptions.AddressInUse) as exc:
            worker.WorkerPool("unused", executors=1).__enter__()
        assert exc.value.__notes__ == [
            f"Also couldn't remove the worker pool's socket directory: [Errno 13] Permission denied: '{created[0]}'"
        ]
    finally:
        for path in created:
            os.rmdir(path)
