import os
import tempfile
import time
import traceback

from concurrent import futures
from datetime import date
from datetime import timedelta
from pathlib import Path
from unittest.mock import MagicMock
from unittest.mock import patch

import click
import grpc
import pytest
import typer
import yaml

from foreverbull_cli.bull.backtest import backtest
from foreverbull_cli.bull.cli import cli
from foreverbull_cli.bull.summary import format_drawdown
from foreverbull_cli.bull.summary import format_percent
from foreverbull_cli.output import execution_link
from typer.testing import CliRunner

from foreverbull.foreverbull import BacktestSession
from foreverbull.pb.foreverbull import common_pb
from foreverbull.pb.foreverbull.backtest.service.v1 import backtest_pb as backtest_service_pb
from foreverbull.pb.foreverbull.backtest.service.v1 import backtest_pb_grpc as backtest_service_pb_grpc
from foreverbull.pb.foreverbull.backtest.service.v1 import session_pb as session_service_pb
from foreverbull.pb.foreverbull.backtest.service.v1 import session_pb_grpc as session_service_pb_grpc
from foreverbull.pb.foreverbull.backtest.v1 import backtest_pb
from foreverbull.pb.foreverbull.backtest.v1 import execution_pb
from foreverbull.pb.foreverbull.backtest.v1 import session_pb
from foreverbull.pb.pb_utils import from_pydate_to_proto_date


pytestmark = pytest.mark.usefixtures("fb_env")

START = date(2020, 1, 1)
END = date(2020, 1, 31)


def _sample_backtest(name="test_name", benchmark="SPY", provider="") -> backtest_pb.Backtest:
    return backtest_pb.Backtest(
        name=name,
        benchmark=benchmark,
        provider=provider,
        periods=[
            backtest_pb.Backtest.Period(
                start_date=from_pydate_to_proto_date(START),
                end_date=from_pydate_to_proto_date(END),
                assets=[
                    backtest_pb.Backtest.Period.Asset(symbol="AAPL", enabled=True),
                    backtest_pb.Backtest.Period.Asset(symbol="MSFT", enabled=True),
                    backtest_pb.Backtest.Period.Asset(symbol="IBM", enabled=False),
                ],
            )
        ],
    )


def _sample_execution(execution_id="exec-1") -> execution_pb.Execution:
    return execution_pb.Execution(
        id=execution_id,
        backtest="test_name",
        session="sess-1",
        start_date=from_pydate_to_proto_date(START),
        end_date=from_pydate_to_proto_date(END),
    )


def test_backtest_list(runner):
    with patch("foreverbull.broker.backtest.list_backtests") as mock_list:
        mock_list.return_value = [_sample_backtest()]
        result = runner(backtest, ["list"])

        if result.exit_code != 0 and result.exc_info:
            traceback.print_exception(*result.exc_info)
        assert "test_name" in result.stdout
        assert "SPY" in result.stdout
        assert "2020-01-01" in result.stdout
        assert "2020-01-31" in result.stdout


def test_backtest_create(runner):
    with patch("foreverbull.broker.backtest.create") as mock_create:
        mock_create.return_value = iter(
            [
                backtest_service_pb.CreateBacktestResponse(
                    CREATING=common_pb.Step(state=common_pb.State.COMPLETED),
                    DOWNLOADING=common_pb.Step(state=common_pb.State.COMPLETED),
                    INGESTING=common_pb.Step(state=common_pb.State.COMPLETED),
                    STORING=common_pb.Step(state=common_pb.State.COMPLETED),
                    backtest=_sample_backtest(),
                )
            ]
        )
        result = runner(
            backtest,
            [
                "create",
            ],
            input="demo\niShares S&P 100\n2020-01-01\n2020-01-31\n",
        )

        if result.exit_code != 0:
            traceback.print_exception(*result.exc_info)  # type: ignore
        assert "test_name" in result.stdout
        assert "2020-01-01" in result.stdout
        assert "2020-01-31" in result.stdout
        assert "AAPL" in result.stdout
        assert "MSFT" in result.stdout
        assert "SPY" in result.stdout


_STOOQ_BLOCKED = (
    "Stooq blocked the download for AAPL (it returned a bot-check page instead of data). Retry with --provider yahoo."
)
_STOOQ_REFUSED = "Stooq blocked the download for AAPL (it refused the request instead of returning data). Retry with --provider yahoo."
_YAHOO_BLOCKED = (
    "Yahoo blocked the download for AAPL (it returned a bot-check page instead of data). Retry with --provider stooq."
)


def _blocked_stream(message: str):
    def stream(**_kwargs):
        yield backtest_service_pb.CreateBacktestResponse(
            CREATING=common_pb.Step(state=common_pb.State.UNSPECIFIED),
            DOWNLOADING=common_pb.Step(state=common_pb.State.UNSPECIFIED),
            INGESTING=common_pb.Step(state=common_pb.State.UNSPECIFIED),
            STORING=common_pb.Step(state=common_pb.State.UNSPECIFIED),
        )
        yield backtest_service_pb.CreateBacktestResponse(
            CREATING=common_pb.Step(state=common_pb.State.COMPLETED),
            DOWNLOADING=common_pb.Step(state=common_pb.State.RUNNING),
        )
        # The orchestrator copies the download command's error into the step.
        yield backtest_service_pb.CreateBacktestResponse(
            CREATING=common_pb.Step(state=common_pb.State.COMPLETED),
            DOWNLOADING=common_pb.Step(state=common_pb.State.FAILED, message=message),
        )

    return stream


@pytest.mark.parametrize(
    ("provider", "message"),
    [("stooq", _STOOQ_BLOCKED), ("stooq", _STOOQ_REFUSED), ("yahoo", _YAHOO_BLOCKED)],
)
def test_backtest_create_blocked_provider_prints_one_error_line(provider, message, monkeypatch):
    # A pipe, not a terminal: FORCE_COLOR in some shells would make Rich treat
    # the captured output as a dumb terminal and add a blank line.
    monkeypatch.setenv("TTY_COMPATIBLE", "0")
    with (
        patch("foreverbull.broker.backtest.create", side_effect=_blocked_stream(message)),
        tempfile.NamedTemporaryFile("w", suffix=".yaml", delete=False) as cfg_file,
    ):
        yaml.safe_dump(_file_payload(), cfg_file)
        cfg_file.flush()
        result = CliRunner().invoke(backtest, ["create", "--file", cfg_file.name, "--provider", provider])
    Path(cfg_file.name).unlink(missing_ok=True)

    if os.environ.get("ECHO"):
        print(result.output, end="")
    assert result.exit_code == 1
    assert result.output == f"Using provider: {provider} (from --provider)\n✖ {message}\n"


def test_backtest_create_grpc_error_prints_one_error_line(monkeypatch):
    monkeypatch.setenv("TTY_COMPATIBLE", "0")

    class BlockedRpc(grpc.RpcError):
        def details(self):
            return _STOOQ_BLOCKED

    def stream(**_kwargs):
        raise BlockedRpc()
        yield  # pragma: no cover

    with (
        patch("foreverbull.broker.backtest.create", side_effect=stream),
        tempfile.NamedTemporaryFile("w", suffix=".yaml", delete=False) as cfg_file,
    ):
        yaml.safe_dump(_file_payload(), cfg_file)
        cfg_file.flush()
        result = CliRunner().invoke(backtest, ["create", "--file", cfg_file.name, "--provider", "stooq"])
    Path(cfg_file.name).unlink(missing_ok=True)

    assert result.exit_code == 1
    assert result.output == f"Using provider: stooq (from --provider)\n✖ {_STOOQ_BLOCKED}\n"


class _BrokenStream(grpc.RpcError):
    def __init__(self, details: str, code: grpc.StatusCode = grpc.StatusCode.UNAVAILABLE):
        self._details = details
        self._code = code

    def details(self):
        return self._details

    def code(self):
        return self._code


def _stream_breaking_after_creating(**_kwargs):
    yield backtest_service_pb.CreateBacktestResponse(
        CREATING=common_pb.Step(state=common_pb.State.COMPLETED),
        DOWNLOADING=common_pb.Step(state=common_pb.State.RUNNING),
    )
    raise _BrokenStream("Socket closed")


def _create_from_file(provider: str = "yahoo"):
    with tempfile.NamedTemporaryFile("w", suffix=".yaml", delete=False) as cfg_file:
        yaml.safe_dump(_file_payload(), cfg_file)
        cfg_file.flush()
        result = CliRunner().invoke(backtest, ["create", "--file", cfg_file.name, "--provider", provider])
    Path(cfg_file.name).unlink(missing_ok=True)
    return result


def test_backtest_create_broken_stream_removes_the_backtest(monkeypatch):
    monkeypatch.setenv("TTY_COMPATIBLE", "0")
    with (
        patch("foreverbull.broker.backtest.create", side_effect=_stream_breaking_after_creating),
        patch("foreverbull.broker.backtest.delete") as mock_delete,
    ):
        result = _create_from_file()

    mock_delete.assert_called_once_with(_file_payload()["name"])
    assert result.exit_code == 1
    assert result.output == (
        "Using provider: yahoo (from --provider)\n"
        "✖ Creating the backtest failed: socket closed. Nothing was changed. Run the same create again.\n"
    )


def test_backtest_create_broken_stream_already_removed(monkeypatch):
    monkeypatch.setenv("TTY_COMPATIBLE", "0")
    with (
        patch("foreverbull.broker.backtest.create", side_effect=_stream_breaking_after_creating),
        patch(
            "foreverbull.broker.backtest.delete",
            side_effect=_BrokenStream("backtest not found", grpc.StatusCode.NOT_FOUND),
        ),
    ):
        result = _create_from_file()

    assert result.exit_code == 1
    assert result.output == (
        "Using provider: yahoo (from --provider)\n"
        "✖ Creating the backtest failed: socket closed. Nothing was changed. Run the same create again.\n"
    )


def test_backtest_create_broken_stream_delete_fails(monkeypatch):
    monkeypatch.setenv("TTY_COMPATIBLE", "0")
    name = _file_payload()["name"]
    with (
        patch("foreverbull.broker.backtest.create", side_effect=_stream_breaking_after_creating),
        patch("foreverbull.broker.backtest.delete", side_effect=_BrokenStream("connection refused")),
    ):
        result = _create_from_file()

    assert result.exit_code == 1
    assert "Nothing was changed" not in result.output
    assert f"delete it with `fbull backtest delete {name}` before you retry." in " ".join(result.output.split())


def test_backtest_create_broken_stream_before_creating_keeps_existing(monkeypatch):
    # The name may belong to an older backtest; this create never stored it.
    monkeypatch.setenv("TTY_COMPATIBLE", "0")

    def stream(**_kwargs):
        yield backtest_service_pb.CreateBacktestResponse(
            CREATING=common_pb.Step(state=common_pb.State.RUNNING),
        )
        raise _BrokenStream("Socket closed")

    with (
        patch("foreverbull.broker.backtest.create", side_effect=stream),
        patch("foreverbull.broker.backtest.delete") as mock_delete,
    ):
        result = _create_from_file()

    mock_delete.assert_not_called()
    assert result.exit_code == 1
    assert result.output == "Using provider: yahoo (from --provider)\n✖ Socket closed\n"


def test_backtest_create_reprompts_invalid_dates(runner):
    with patch("foreverbull.broker.backtest.create") as mock_create:
        mock_create.return_value = iter(
            [
                backtest_service_pb.CreateBacktestResponse(
                    CREATING=common_pb.Step(state=common_pb.State.COMPLETED),
                    DOWNLOADING=common_pb.Step(state=common_pb.State.COMPLETED),
                    INGESTING=common_pb.Step(state=common_pb.State.COMPLETED),
                    STORING=common_pb.Step(state=common_pb.State.COMPLETED),
                    backtest=_sample_backtest(),
                )
            ]
        )
        result = runner(
            backtest,
            ["create"],
            input="\ndemo\niShares S&P 100\nnot-a-date\n1999-01-01\n2020-01-01\n2019-12-31\n2020-01-31\n",
        )

    assert result.exit_code == 0
    assert "Enter a name" in result.stdout
    assert "not a date" in result.stdout
    assert "started on 2000-10-23" in result.stdout
    assert "before start date" in result.stdout
    request = mock_create.call_args.kwargs["entity"]
    assert request.name == "demo"
    assert request.provider == "yahoo"
    assert request.start_date.year == 2020
    assert request.end_date.day == 31


def test_backtest_etfs(runner):
    result = runner(backtest, ["etfs"])
    assert result.exit_code == 0
    assert "iShares S&P 100" in result.stdout
    assert "OEF" in result.stdout
    assert "2000-10-23" in result.stdout


def _created_backtest_response():
    return iter(
        [
            backtest_service_pb.CreateBacktestResponse(
                CREATING=common_pb.Step(state=common_pb.State.COMPLETED),
                DOWNLOADING=common_pb.Step(state=common_pb.State.COMPLETED),
                INGESTING=common_pb.Step(state=common_pb.State.COMPLETED),
                STORING=common_pb.Step(state=common_pb.State.COMPLETED),
                backtest=_sample_backtest(),
            )
        ]
    )


def test_backtest_create_unknown_etf_does_not_call_broker(runner):
    with patch("foreverbull.broker.backtest.create") as mock_create:
        result = runner(backtest, ["create"], input="demo\nnot-a-fund\n")

    assert result.exit_code == 0
    mock_create.assert_not_called()
    assert "Unknown ETF 'not-a-fund'. Known funds: iShares S&P 100." in result.stdout
    assert "Creating" not in result.stdout
    assert "Traceback" not in result.stdout


def test_backtest_create_reprompts_unknown_etf(runner):
    with patch("foreverbull.broker.backtest.create") as mock_create:
        mock_create.return_value = _created_backtest_response()
        result = runner(
            backtest,
            ["create"],
            input="demo\nnot-a-fund\niShares S&P 100\n2020-01-01\n2020-01-31\n",
        )

    assert result.exit_code == 0
    assert "Unknown ETF 'not-a-fund'. Known funds: iShares S&P 100." in result.stdout
    request = mock_create.call_args.kwargs["entity"]
    assert request.base_etf == "iShares S&P 100"
    assert request.name == "demo"


def test_backtest_create_reprompts_end_after_today(runner):
    future = date.today() + timedelta(days=1)
    with patch("foreverbull.broker.backtest.create") as mock_create:
        mock_create.return_value = _created_backtest_response()
        result = runner(
            backtest,
            ["create"],
            input=f"demo\niShares S&P 100\n2020-01-01\n{future.isoformat()}\n2020-01-31\n",
        )

    assert result.exit_code == 0
    assert f"End date {future.isoformat()} is after today ({date.today().isoformat()})." in result.stdout
    request = mock_create.call_args.kwargs["entity"]
    assert request.end_date.year == 2020
    assert request.end_date.month == 1
    assert request.end_date.day == 31
    assert mock_create.call_count == 1


def test_backtest_create_reprompts_start_after_today(runner):
    future = date.today() + timedelta(days=1)
    with patch("foreverbull.broker.backtest.create") as mock_create:
        mock_create.return_value = _created_backtest_response()
        result = runner(
            backtest,
            ["create"],
            input=f"demo\niShares S&P 100\n{future.isoformat()}\n2020-01-01\n2020-01-31\n",
        )

    assert result.exit_code == 0
    assert f"{future.isoformat()} is after today ({date.today().isoformat()})." in result.stdout
    request = mock_create.call_args.kwargs["entity"]
    assert request.start_date.year == 2020
    assert request.start_date.day == 1


def test_backtest_create_reprompts_non_iso_date(runner):
    with patch("foreverbull.broker.backtest.create") as mock_create:
        mock_create.return_value = _created_backtest_response()
        result = runner(
            backtest,
            ["create"],
            input="demo\niShares S&P 100\n20200101\n2020-02-31\n2020-01-01\n2020-01-31\n",
        )

    assert result.exit_code == 0
    assert "'20200101' is not a date. Use YYYY-MM-DD, for example 2024-01-31." in result.stdout
    assert "'2020-02-31' is not a date. Use YYYY-MM-DD, for example 2024-01-31." in result.stdout
    request = mock_create.call_args.kwargs["entity"]
    assert request.start_date.year == 2020
    assert request.start_date.month == 1
    assert request.start_date.day == 1


def test_backtest_create_accepts_long_range(runner):
    with patch("foreverbull.broker.backtest.create") as mock_create:
        mock_create.return_value = _created_backtest_response()
        result = runner(
            backtest,
            ["create"],
            input="long\niShares S&P 100\n2020-01-01\n2025-12-31\n",
        )

    assert result.exit_code == 0
    assert "is not a date" not in result.stdout
    assert "after today" not in result.stdout
    assert "before start date" not in result.stdout
    assert "started on" not in result.stdout
    request = mock_create.call_args.kwargs["entity"]
    assert request.name == "long"
    assert request.base_etf == "iShares S&P 100"
    assert (request.start_date.year, request.start_date.month, request.start_date.day) == (2020, 1, 1)
    assert (request.end_date.year, request.end_date.month, request.end_date.day) == (2025, 12, 31)


def test_backtest_create_from_file(runner):
    payload = {
        "name": "from_file",
        "benchmark": "QQQ",
        "periods": [
            {
                "start_date": "2021-01-01",
                "end_date": "2021-01-31",
                "assets": [
                    {"symbol": "AAPL", "enabled": True},
                    {"symbol": "MSFT", "enabled": True},
                ],
            }
        ],
    }
    created = backtest_pb.Backtest(
        name="from_file",
        benchmark="QQQ",
        periods=[
            backtest_pb.Backtest.Period(
                start_date=from_pydate_to_proto_date(date(2021, 1, 1)),
                end_date=from_pydate_to_proto_date(date(2021, 1, 31)),
                assets=[
                    backtest_pb.Backtest.Period.Asset(symbol="AAPL", enabled=True),
                    backtest_pb.Backtest.Period.Asset(symbol="MSFT", enabled=True),
                ],
            )
        ],
    )
    with (
        patch("foreverbull.broker.backtest.create") as mock_create,
        tempfile.NamedTemporaryFile("w", suffix=".yaml", delete=False) as cfg_file,
    ):
        yaml.safe_dump(payload, cfg_file)
        cfg_file.flush()
        mock_create.return_value = iter(
            [
                backtest_service_pb.CreateBacktestResponse(
                    STORING=common_pb.Step(state=common_pb.State.COMPLETED),
                    backtest=created,
                )
            ]
        )
        result = runner(backtest, ["create", "--file", cfg_file.name])

    Path(cfg_file.name).unlink(missing_ok=True)
    assert result.exit_code == 0
    request = mock_create.call_args.kwargs["entity"]
    assert request.backtest.provider == "yahoo"
    assert request.backtest.profile == ""
    assert "from_file" in result.stdout
    assert "2021-01-01" in result.stdout
    assert "QQQ" in result.stdout
    assert "AAPL" in result.stdout
    assert "Using provider: yahoo (default)" in result.stdout


def _file_payload(**extra: object) -> dict:
    payload = {
        "name": "from_file",
        "benchmark": "QQQ",
        "periods": [
            {
                "start_date": "2021-01-01",
                "end_date": "2021-01-31",
                "assets": [
                    {"symbol": "AAPL", "enabled": True},
                ],
            }
        ],
    }
    payload.update(extra)
    return payload


def test_backtest_create_from_file_reads_provider_and_profile(runner):
    created = _sample_backtest(name="from_file", benchmark="QQQ", provider="alpaca")
    with (
        patch("foreverbull.broker.backtest.create") as mock_create,
        tempfile.NamedTemporaryFile("w", suffix=".yaml", delete=False) as cfg_file,
    ):
        yaml.safe_dump(_file_payload(provider="alpaca", profile="alpaca-paper"), cfg_file)
        cfg_file.flush()
        mock_create.return_value = iter(
            [
                backtest_service_pb.CreateBacktestResponse(
                    STORING=common_pb.Step(state=common_pb.State.COMPLETED),
                    backtest=created,
                )
            ]
        )
        result = runner(backtest, ["create", "--file", cfg_file.name])

    Path(cfg_file.name).unlink(missing_ok=True)
    assert result.exit_code == 0
    request = mock_create.call_args.kwargs["entity"]
    assert request.backtest.provider == "alpaca"
    assert request.backtest.profile == "alpaca-paper"
    assert f"Using provider: alpaca (from {Path(cfg_file.name).name})" in result.stdout


def test_backtest_create_provider_flag_overrides_file(runner):
    with (
        patch("foreverbull.broker.backtest.create") as mock_create,
        tempfile.NamedTemporaryFile("w", suffix=".yaml", delete=False) as cfg_file,
    ):
        yaml.safe_dump(_file_payload(provider="yahoo", profile="alpaca-paper"), cfg_file)
        cfg_file.flush()
        mock_create.return_value = iter(
            [
                backtest_service_pb.CreateBacktestResponse(
                    STORING=common_pb.Step(state=common_pb.State.COMPLETED),
                    backtest=_sample_backtest(name="from_file", benchmark="QQQ", provider="stooq"),
                )
            ]
        )
        result = runner(backtest, ["create", "--file", cfg_file.name, "--provider", "stooq"])

    Path(cfg_file.name).unlink(missing_ok=True)
    assert result.exit_code == 0
    request = mock_create.call_args.kwargs["entity"]
    assert request.backtest.provider == "stooq"
    assert request.backtest.profile == "alpaca-paper"
    assert "Using provider: stooq (from --provider)" in result.stdout


def test_backtest_create_unknown_provider_does_not_call_broker():
    # CliRunner catches typer.Exit; the shared runner fixture re-raises it.
    with patch("foreverbull.broker.backtest.create") as mock_create:
        result = CliRunner().invoke(backtest, ["create", "--provider", "binance"])

    assert result.exit_code == 1
    mock_create.assert_not_called()
    assert "Unknown price provider 'binance'" in result.stdout
    assert "Valid providers: yahoo, stooq, alpaca" in result.stdout


def test_backtest_create_etf_sends_provider_flag(runner):
    with patch("foreverbull.broker.backtest.create") as mock_create:
        mock_create.return_value = _created_backtest_response()
        result = runner(
            backtest,
            ["create", "--provider", "stooq"],
            input="demo\niShares S&P 100\n2020-01-01\n2020-01-31\n",
        )

    assert result.exit_code == 0
    request = mock_create.call_args.kwargs["entity"]
    assert request.provider == "stooq"
    assert request.name == "demo"
    assert "Using provider: stooq (from --provider)" in result.stdout


def test_backtest_create_etf_without_flag_uses_default_provider(runner):
    with patch("foreverbull.broker.backtest.create") as mock_create:
        mock_create.return_value = _created_backtest_response()
        result = runner(
            backtest,
            ["create"],
            input="demo\niShares S&P 100\n2020-01-01\n2020-01-31\n",
        )

    assert result.exit_code == 0
    request = mock_create.call_args.kwargs["entity"]
    assert request.provider == "yahoo"
    assert "Using provider: yahoo (default)" in result.stdout


def test_backtest_create_help_shows_provider_text():
    # Rich wraps rendered help at the terminal width, so compare the option text itself.
    group = typer.main.get_command(backtest)
    assert isinstance(group, click.Group)
    command = group.commands["create"]
    option = next(param for param in command.params if param.name == "provider")
    assert isinstance(option, click.Option)

    assert option.help == "Price source: yahoo, stooq or alpaca. Overrides `provider` in the YAML file. Default: yahoo."


def test_backtest_get(runner):
    with (
        patch("foreverbull.broker.backtest.get") as mock_get,
        patch("foreverbull.broker.backtest.list_executions") as mock_list_executions,
    ):
        mock_get.return_value = _sample_backtest()
        mock_list_executions.return_value = [_sample_execution()]
        result = runner(backtest, ["get", "test_name"])

        if result.exit_code != 0 and result.exc_info:
            traceback.print_exception(*result.exc_info)
        assert "test_name" in result.stdout
        assert "AAPL" in result.stdout
        assert "MSFT" in result.stdout
        assert "IBM" in result.stdout
        assert "2020-01-01" in result.stdout
        assert "2020-01-31" in result.stdout
        assert "exec-1" in result.stdout
        assert f"Link: {execution_link('exec-1')}" in result.stdout
        assert "SPY" in result.stdout
        assert "yahoo" in result.stdout


def test_backtest_get_yaml(runner):
    with (
        patch("foreverbull.broker.backtest.get") as mock_get,
        patch("foreverbull.broker.backtest.list_executions") as mock_list_executions,
    ):
        mock_get.return_value = _sample_backtest()
        mock_list_executions.return_value = []
        result = runner(backtest, ["get", "test_name", "--output", "yaml"])
        assert result.exit_code == 0
        dumped = yaml.safe_load(result.stdout)
        assert dumped["name"] == "test_name"
        assert dumped["benchmark"] == "SPY"
        assert dumped["provider"] == "yahoo"
        assert "profile" not in dumped
        assert dumped["periods"][0]["start_date"] == "2020-01-01"
        assert dumped["periods"][0]["assets"][0]["symbol"] == "AAPL"


@pytest.fixture
def start_grpc_server():
    grpc_server = grpc.server(thread_pool=futures.ThreadPoolExecutor(max_workers=1))

    def _add_servicer(broker_servicer, broker_session_servicer):
        backtest_service_pb_grpc.BacktestServicerServiceSync.add_to_server(broker_servicer, grpc_server)
        session_service_pb_grpc.SessionServicerServiceSync.add_to_server(broker_session_servicer, grpc_server)
        server_port = grpc_server.add_insecure_port("[::]:7877")
        grpc_server.start()
        time.sleep(1)
        return server_port

    yield _add_servicer
    grpc_server.stop(None)


def _run_session_mocks():
    mock_backtest_session = MagicMock(spec=BacktestSession)
    mock_backtest_session.backtest = _sample_backtest()
    mock_backtest_session.run_execution.return_value = iter(
        [
            (
                "exec-1",
                session_service_pb.RunExecutionResponse(num_current_period=1, num_total_periods=2),
            )
        ]
    )
    return mock_backtest_session


def _final_period(**overrides) -> execution_pb.Period:
    fields = {
        "date": from_pydate_to_proto_date(END),
        "portfolio_value": 104200.0,
        "algorithm_period_return": 0.042,
        "max_drawdown": -0.068,
        "sharpe": 1.35,
        "sortino": 1.1,
        "benchmark_period_return": 0.031,
        "longs_count": 2,
    }
    fields.update(overrides)
    return execution_pb.Period(**fields)


def _invoke_run(runner, mock_backtest_session, periods, backtest_entity=None):
    with (
        patch("foreverbull_cli.bull.backtest.Foreverbull") as mock_foreverbull,
        patch("foreverbull.models.Algorithm.from_file_path") as from_file_path,
        patch("foreverbull.broker.backtest.create_session") as create_session,
        patch("foreverbull.broker.backtest.get") as get,
        patch("foreverbull.broker.backtest.get_execution") as get_execution,
    ):
        from_file_path.return_value = True
        get.return_value = backtest_entity or _sample_backtest()
        create_session.return_value = iter(
            [backtest_service_pb.CreateSessionResponse(session=session_pb.Session(port=7878))]
        )
        get_execution.return_value = (_sample_execution(), periods)
        mock_foreverbull.return_value.backtest_session.return_value.__enter__.return_value = mock_backtest_session
        result = runner(backtest, ["run", "test_name", "demo.py"])
        create_session.assert_called_once()
        return result, get_execution


def test_backtest_run_prints_results_summary(runner):
    mock_backtest_session = _run_session_mocks()
    result, get_execution = _invoke_run(
        runner, mock_backtest_session, [_final_period(algorithm_period_return=0.01), _final_period()]
    )
    assert result.exit_code == 0
    get_execution.assert_called_once_with("exec-1")
    mock_backtest_session.run_execution.assert_called_once()
    assert "Backtest finished: test_name · 2020-01-01 → 2020-01-31 · provider: yahoo" in result.stdout
    # The last period holds the cumulative numbers for the whole run.
    assert "+4.2%" in result.stdout
    assert "+1.0%" not in result.stdout
    assert "Benchmark (SPY)" in result.stdout
    assert "+3.1%" in result.stdout
    assert "1.35" in result.stdout
    assert "1.10" in result.stdout
    assert "-6.8%" in result.stdout
    assert "Details: uv run fbull backtest execution exec-1" in result.stdout
    assert f"Charts:  {execution_link('exec-1')}" in result.stdout


def test_format_drawdown_has_no_plus_sign():
    assert format_drawdown(0.0) == "0.0%"
    assert format_drawdown(-0.068) == "-6.8%"
    # Returns keep the signed form, including for zero.
    assert format_percent(0.0) == "+0.0%"
    assert format_percent(-0.068) == "-6.8%"


def test_backtest_run_prints_zero_drawdown_without_plus(runner):
    result, _ = _invoke_run(runner, _run_session_mocks(), [_final_period(max_drawdown=0.0)])
    assert result.exit_code == 0
    drawdown_line = next(line for line in result.stdout.splitlines() if "Max drawdown" in line)
    assert "0.0%" in drawdown_line
    assert "+0.0%" not in drawdown_line


def test_backtest_run_says_when_benchmark_return_is_missing(runner):
    period = _final_period()
    period.clear_field("benchmark_period_return")
    period.clear_field("sharpe")
    result, _ = _invoke_run(runner, _run_session_mocks(), [period])
    assert result.exit_code == 0
    assert "Benchmark (SPY)" in result.stdout
    assert "not available" in result.stdout


def test_backtest_run_says_when_backtest_has_no_benchmark(runner):
    result, _ = _invoke_run(runner, _run_session_mocks(), [_final_period()], _sample_backtest(benchmark=""))
    assert result.exit_code == 0
    assert "Benchmark" in result.stdout
    assert "not set" in result.stdout


def test_backtest_run_says_plainly_when_no_trades_were_placed(runner):
    # A run that never held a position stores zeros and no ratios; say why instead of leaving them unexplained.
    flat = [
        _final_period(
            portfolio_value=100000.0,
            algorithm_period_return=0.0,
            max_drawdown=0.0,
            longs_count=0,
            benchmark_period_return=0.248,
        )
        for _ in range(250)
    ]
    for period in flat:
        period.clear_field("sharpe")
        period.clear_field("sortino")
    result, _ = _invoke_run(runner, _run_session_mocks(), flat)
    assert result.exit_code == 0
    assert "No trades: the strategy held no position on any of the 250 trading days." in result.stdout
    assert "+24.8%" in result.stdout


def test_backtest_run_with_a_held_position_has_no_no_trades_line(runner):
    periods = [_final_period(longs_count=0), _final_period(longs_count=0, shorts_count=1), _final_period(longs_count=0)]
    result, _ = _invoke_run(runner, _run_session_mocks(), periods)
    assert result.exit_code == 0
    assert "No trades" not in result.stdout


def test_backtest_run_without_stored_periods_says_so(runner):
    result, _ = _invoke_run(runner, _run_session_mocks(), [])
    assert result.exit_code == 0
    assert "✔ Backtest finished, but no results were stored for this execution." in result.stdout
    assert "Backtest finished:" not in result.stdout
    assert "Details: uv run fbull backtest execution exec-1" in result.stdout


def test_backtest_run_shows_short_execution_id(runner):
    full_id = "01234567-89ab-cdef-0123-456789abcdef"
    mock_backtest_session = _run_session_mocks()
    mock_backtest_session.run_execution.return_value = iter(
        [(full_id, session_service_pb.RunExecutionResponse(num_current_period=1, num_total_periods=1))]
    )
    result, get_execution = _invoke_run(runner, mock_backtest_session, [_final_period()])
    assert result.exit_code == 0
    get_execution.assert_called_once_with(full_id)
    assert "Details: uv run fbull backtest execution 01234567\n" in result.stdout
    assert f"Charts:  {execution_link(full_id)}" in result.stdout


def test_backtest_run_fails_when_no_execution_started(runner):
    mock_backtest_session = _run_session_mocks()
    mock_backtest_session.run_execution.return_value = iter([])
    result, get_execution = _invoke_run(lambda app, args: CliRunner().invoke(app, args), mock_backtest_session, [])
    assert result.exit_code == 1
    assert "✖ The backtest session ended before an execution started." in result.stdout
    get_execution.assert_not_called()


def test_backtest_run_dry(runner):
    with (
        patch("foreverbull.broker.backtest.get") as get,
        patch("foreverbull.broker.backtest.create_session") as create_session,
    ):
        get.return_value = _sample_backtest()
        result = runner(backtest, ["run", "test_name", "demo.py", "--dry"])
        assert result.exit_code == 0
        assert "Dry Run" in result.stdout
        assert "demo.py" in result.stdout
        assert "2020-01-01" in result.stdout
        create_session.assert_not_called()


def test_backtest_run_invalid_start(runner):
    with patch("foreverbull.broker.backtest.get") as get:
        get.return_value = _sample_backtest()
        result = runner(backtest, ["run", "test_name", "demo.py", "--start", "not-a-date"])
        assert result.exit_code == 0
        assert "Invalid start date" in result.stdout


def test_backtest_executions(runner):
    with patch("foreverbull.broker.backtest.list_executions") as mock_list_executions:
        mock_list_executions.return_value = [_sample_execution()]
        result = runner(backtest, ["executions", "--backtest", "test_name"])
        assert result.exit_code == 0
        assert "exec-1" in result.stdout
        assert "test_name" in result.stdout
        assert "sess-1" in result.stdout
        assert "2020-01-01" in result.stdout
        assert f"Link: {execution_link('exec-1')}" in result.stdout


def test_backtest_execution(runner):
    full_id = "01234567-89ab-cdef-0123-456789abcdef"
    period = execution_pb.Period(
        date=from_pydate_to_proto_date(date(2020, 1, 2)),
        portfolio_value=1000.5,
        alpha=0.12,
        beta=0.98,
        sharpe=1.5,
        sortino=1.2,
        benchmark_period_return=0.01,
        benchmark_volatility=0.02,
    )
    with (
        patch("foreverbull.broker.backtest.get_execution") as mock_get_execution,
        patch("foreverbull.broker.backtest.list_executions") as mock_list_executions,
    ):
        mock_get_execution.return_value = (_sample_execution(full_id), [period])
        result = runner(backtest, ["execution", full_id])
        assert result.exit_code == 0
        # A full UUID goes straight to the broker without listing executions.
        mock_list_executions.assert_not_called()
        mock_get_execution.assert_called_once_with(full_id)
        assert "1000.50000" in result.stdout
        assert "0.12000" in result.stdout
        assert "1.50000" in result.stdout
        assert "2020-01-02" in result.stdout
        assert f"Link: {execution_link(full_id)}" in result.stdout


def test_backtest_execution_resolves_short_id(runner):
    full_id = "01234567-89ab-cdef-0123-456789abcdef"
    with (
        patch("foreverbull.broker.backtest.get_execution") as mock_get_execution,
        patch("foreverbull.broker.backtest.list_executions") as mock_list_executions,
    ):
        mock_list_executions.return_value = [
            _sample_execution(full_id),
            _sample_execution("fedcba98-0000-0000-0000-000000000000"),
        ]
        mock_get_execution.return_value = (_sample_execution(full_id), [])
        result = runner(backtest, ["execution", "01234567"])
        assert result.exit_code == 0
        mock_get_execution.assert_called_once_with(full_id)


def _execution_failure(runner, prefix, executions):
    with (
        patch("foreverbull.broker.backtest.get_execution") as mock_get_execution,
        patch("foreverbull.broker.backtest.list_executions") as mock_list_executions,
    ):
        mock_list_executions.return_value = executions
        result = CliRunner().invoke(backtest, ["execution", prefix])
        mock_get_execution.assert_not_called()
        return result


def test_backtest_execution_refuses_unknown_short_id(runner):
    result = _execution_failure(runner, "deadbeef", [_sample_execution("01234567-89ab-cdef-0123-456789abcdef")])
    assert result.exit_code == 1
    assert "No execution ID starts with 'deadbeef'" in result.stdout


def test_backtest_execution_refuses_ambiguous_short_id(runner):
    executions = [
        _sample_execution("01234567-89ab-cdef-0123-456789abcdef"),
        _sample_execution("01234567-ffff-cdef-0123-456789abcdef"),
    ]
    result = _execution_failure(runner, "0123", executions)
    assert result.exit_code == 1
    assert "'0123' matches 2 executions (01234567-89ab, 01234567-ffff)" in result.stdout


def test_backtest_execution_resolves_distinguishing_prefix(runner):
    full_id = "01234567-89ab-cdef-0123-456789abcdef"
    with (
        patch("foreverbull.broker.backtest.get_execution") as mock_get_execution,
        patch("foreverbull.broker.backtest.list_executions") as mock_list_executions,
    ):
        mock_list_executions.return_value = [
            _sample_execution(full_id),
            _sample_execution("01234567-ffff-cdef-0123-456789abcdef"),
        ]
        mock_get_execution.return_value = (_sample_execution(full_id), [])
        result = runner(backtest, ["execution", "01234567-89ab"])
        assert result.exit_code == 0
        mock_get_execution.assert_called_once_with(full_id)


def test_verbosity_flags(runner):
    with (
        patch("foreverbull.broker.backtest.list_backtests", return_value=[]),
        patch("foreverbull_cli.bull.cli.logging.basicConfig") as basic_config,
    ):
        result = runner(cli, ["backtest", "list"])
        assert result.exit_code == 0
        assert basic_config.call_args.kwargs["level"] == "ERROR"

        result = runner(cli, ["-v", "backtest", "list"])
        assert result.exit_code == 0
        assert basic_config.call_args.kwargs["level"] == "WARNING"

        result = runner(cli, ["-vv", "backtest", "list"])
        assert result.exit_code == 0
        assert basic_config.call_args.kwargs["level"] == "INFO"

        result = runner(cli, ["-vvv", "backtest", "list"])
        assert result.exit_code == 0
        assert basic_config.call_args.kwargs["level"] == "DEBUG"

        result = runner(cli, ["-v", "-v", "backtest", "list"])
        assert result.exit_code == 0
        assert basic_config.call_args.kwargs["level"] == "INFO"


def test_cli_default_provider_matches_zipline_fallback(monkeypatch):
    # The CLI default and the OHLC_PROVIDER fallback Zipline reads bars with must name the same source.
    from foreverbull_cli.bull.backtest import _DEFAULT_PROVIDER

    from foreverbull.models.finance import ohlc_provider

    monkeypatch.delenv("OHLC_PROVIDER", raising=False)
    assert ohlc_provider() == _DEFAULT_PROVIDER
