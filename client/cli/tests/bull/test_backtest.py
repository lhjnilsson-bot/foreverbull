import itertools
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
import foreverbull_cli.bull.backtest as backtest_cli
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
from foreverbull.models.finance import WORKER_MARKET_DATA_DENIED
from foreverbull.models.finance import market_data_denied_line
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


# A backtest with no data is still listed, with a ⚠ line that says how to
# fix it, and the command exits 0: listing is not a failure.
def test_backtest_list_without_periods_warns_and_exits_0(monkeypatch):
    monkeypatch.setenv("TTY_COMPATIBLE", "0")
    with patch("foreverbull.broker.backtest.list_backtests") as mock_list:
        mock_list.return_value = [_sample_backtest(), backtest_pb.Backtest(name="empty", benchmark="SPY")]
        result = CliRunner().invoke(backtest, ["list"])
    assert result.exit_code == 0
    assert result.exception is None
    assert "test_name" in result.stdout
    assert result.stdout.count("⚠") == 1
    assert "✖" not in result.stdout
    assert ("⚠ empty has no data. Run `fbull backtest delete empty`, then `fbull backtest create`.") in " ".join(
        result.stdout.split()
    )
    assert "has no periods" not in result.stdout
    assert "Traceback" not in result.stdout


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
        assert "  Name       test_name" in result.stdout
        assert "  Start      2020-01-01" in result.stdout
        assert "  End        2020-01-31" in result.stdout
        # IBM is disabled, so only the enabled symbols are listed.
        assert "  Symbols    AAPL, MSFT\n" in result.stdout
        assert "  Benchmark  SPY" in result.stdout


def test_backtest_create_prints_table_from_the_broker_stream(runner):
    """The broker sends the stored backtest in one last frame after ingest."""
    unset = common_pb.State.UNSPECIFIED
    done = common_pb.State.COMPLETED

    def frame(creating, downloading, ingesting, storing, stored=None):
        return backtest_service_pb.CreateBacktestResponse(
            CREATING=common_pb.Step(state=creating),
            DOWNLOADING=common_pb.Step(state=downloading),
            INGESTING=common_pb.Step(state=ingesting),
            STORING=common_pb.Step(state=storing),
            backtest=stored,
        )

    with patch("foreverbull.broker.backtest.create") as mock_create:
        mock_create.return_value = iter(
            [
                frame(unset, unset, unset, unset),
                frame(done, unset, unset, unset),
                frame(done, done, unset, unset),
                frame(done, done, done, unset),
                frame(done, done, done, done, _sample_backtest()),
            ]
        )
        result = runner(backtest, ["create"], input="demo\niShares S&P 100\n2020-01-01\n2020-01-31\n")

    assert result.exit_code == 0, result.stdout
    lines = result.stdout.splitlines()
    record = lines[lines.index("Created Backtest") :]
    assert record == [
        "Created Backtest",
        "  Name       test_name",
        "  Start      2020-01-01",
        "  End        2020-01-31",
        "  Symbols    AAPL, MSFT",
        "  Benchmark  SPY",
        "  Provider   yahoo",
    ]


# The 99 iShares S&P 100 members a 2024 Q1 backtest stores (BK and BRKB left out).
# fmt: off
_OEF_2024Q1 = [
    "AAPL", "ABBV", "ABT", "ACN", "ADBE", "AIG", "AMD", "AMGN", "AMT", "AMZN", "AVGO", "AXP", "BA", "BAC", "BKNG",
    "BLK", "BMY", "C", "CAT", "CHTR", "CL", "CMCSA", "COF", "COP", "COST", "CRM", "CSCO", "CVS", "CVX", "DE", "DHR",
    "DIS", "DOW", "DUK", "EMR", "EXC", "F", "FDX", "GD", "GE", "GILD", "GM", "GOOG", "GOOGL", "GS", "HD", "HON",
    "IBM", "INTC", "JNJ", "JPM", "KHC", "KO", "LIN", "LLY", "LMT", "LOW", "MA", "MCD", "MDLZ", "MDT", "MET", "META",
    "MMM", "MO", "MRK", "MS", "MSFT", "NEE", "NFLX", "NKE", "NVDA", "ORCL", "PEP", "PFE", "PG", "PM", "PYPL", "QCOM",
    "RTX", "SBUX", "SCHW", "SO", "SPG", "T", "TGT", "TMO", "TMUS", "TSLA", "TXN", "UNH", "UNP", "UPS", "USB", "V",
    "VZ", "WFC", "WMT", "XOM",
]
# fmt: on


def _created_record(monkeypatch, symbols: list[str], *, columns: int, verbose: bool = False) -> list[str]:
    """The lines from "Created Backtest" on, printed at the given terminal width."""
    monkeypatch.setenv("COLUMNS", str(columns))
    # Rich reads COLUMNS when the console is built, and reports 80 on a dumb
    # terminal unless both dimensions are pinned.
    monkeypatch.setattr(backtest_cli.console, "_width", columns)
    monkeypatch.setattr(backtest_cli.console, "_height", 25)
    if verbose:
        monkeypatch.setenv("LOGGING_LEVEL", "INFO")
    else:
        monkeypatch.delenv("LOGGING_LEVEL", raising=False)
    stored = backtest_pb.Backtest(
        name="oef-2024q1",
        benchmark="OEF",
        provider="yahoo",
        periods=[
            backtest_pb.Backtest.Period(
                start_date=from_pydate_to_proto_date(date(2024, 1, 2)),
                end_date=from_pydate_to_proto_date(date(2024, 3, 28)),
                # The broker sent the members Z to A.
                assets=[backtest_pb.Backtest.Period.Asset(symbol=symbol, enabled=True) for symbol in symbols],
            )
        ],
    )
    done = common_pb.Step(state=common_pb.State.COMPLETED)
    with patch("foreverbull.broker.backtest.create") as mock_create:
        mock_create.return_value = iter(
            [
                backtest_service_pb.CreateBacktestResponse(
                    CREATING=done, DOWNLOADING=done, INGESTING=done, STORING=done, backtest=stored
                )
            ]
        )
        result = CliRunner().invoke(backtest, ["create"], input="demo\niShares S&P 100\n2024-01-02\n2024-03-28\n")
    assert result.exit_code == 0, result.stdout
    lines = result.stdout.splitlines()
    return lines[lines.index("Created Backtest") :]


@pytest.mark.parametrize("columns", [80, 120])
def test_backtest_create_shows_a_large_backtest_as_one_line_per_field(monkeypatch, columns):
    record = _created_record(monkeypatch, list(reversed(_OEF_2024Q1)), columns=columns)
    assert record == [
        "Created Backtest",
        "  Name       oef-2024q1",
        "  Start      2024-01-02",
        "  End        2024-03-28",
        "  Symbols    99: AAPL, ABBV, ABT, ACN, ADBE and 94 more",
        "  Benchmark  OEF",
        "  Provider   yahoo",
    ]
    assert not any(line.lstrip().startswith(",") for line in record)


def test_backtest_create_lists_a_few_symbols_without_a_count(monkeypatch):
    record = _created_record(monkeypatch, ["NVDA", "MSFT", "AAPL"], columns=80)
    assert "  Symbols    AAPL, MSFT, NVDA" in record
    assert not any("3:" in line for line in record)


@pytest.mark.parametrize("columns", [80, 120])
def test_backtest_create_verbose_lists_every_symbol_breaking_between_tickers(monkeypatch, columns):
    record = _created_record(monkeypatch, list(reversed(_OEF_2024Q1)), columns=columns, verbose=True)
    start = record.index(next(line for line in record if line.startswith("  Symbols    ")))
    end = record.index("  Benchmark  OEF")
    symbol_lines = record[start:end]
    assert len(symbol_lines) > 1
    assert all(len(line) <= columns for line in symbol_lines)
    # Lines fill the width: the next ticker would not have fit on any of them.
    assert all(len(line) + 1 + len(nxt.split()[0]) > columns for line, nxt in itertools.pairwise(symbol_lines))
    # Continuation lines hang under the value column.
    assert all(line.startswith(" " * 13) and line[13] != " " for line in symbol_lines[1:])
    assert not any(line.lstrip().startswith(",") for line in symbol_lines)
    # Every line but the last ends on a comma, so no ticker is split across lines.
    assert all(line.endswith(",") for line in symbol_lines[:-1])
    shown = " ".join(line.strip() for line in symbol_lines).removeprefix("Symbols").strip()
    assert shown == "99: " + ", ".join(_OEF_2024Q1)
    assert record[0:4] == [
        "Created Backtest",
        "  Name       oef-2024q1",
        "  Start      2024-01-02",
        "  End        2024-03-28",
    ]


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


def _notices_stream(lines: list[str]):
    def stream(**_kwargs):
        yield backtest_service_pb.CreateBacktestResponse(
            CREATING=common_pb.Step(state=common_pb.State.UNSPECIFIED),
            DOWNLOADING=common_pb.Step(state=common_pb.State.UNSPECIFIED),
            INGESTING=common_pb.Step(state=common_pb.State.UNSPECIFIED),
            STORING=common_pb.Step(state=common_pb.State.UNSPECIFIED),
        )
        # The orchestrator copies a step's notices into the completed step's
        # notices field; message stays empty on success.
        completed = backtest_service_pb.CreateBacktestResponse(
            CREATING=common_pb.Step(state=common_pb.State.COMPLETED),
            DOWNLOADING=common_pb.Step(state=common_pb.State.COMPLETED),
            INGESTING=common_pb.Step(state=common_pb.State.COMPLETED, notices=lines),
        )
        yield completed
        yield completed

    return stream


def _create_with_notices(monkeypatch, lines: list[str]):
    monkeypatch.setenv("TTY_COMPATIBLE", "0")
    with (
        patch("foreverbull.broker.backtest.create", side_effect=_notices_stream(lines)),
        tempfile.NamedTemporaryFile("w", suffix=".yaml", delete=False) as cfg_file,
    ):
        yaml.safe_dump(_file_payload(), cfg_file)
        cfg_file.flush()
        result = CliRunner().invoke(backtest, ["create", "--file", cfg_file.name, "--provider", "yahoo"])
    Path(cfg_file.name).unlink(missing_ok=True)
    return result


def test_backtest_create_prints_each_step_notice_once(monkeypatch):
    monkeypatch.delenv("LOGGING_LEVEL", raising=False)
    lines = ["First notice.", "Second notice.", "Third notice."]
    result = _create_with_notices(monkeypatch, lines)
    assert result.exit_code == 0
    # Printed once although the completed step arrives twice, each on its
    # own line, with nothing folded.
    shown = [line for line in result.output.splitlines() if "⚠" in line]
    assert shown == [f"⚠ {line}" for line in lines]


_MISSING_ONE_DAY = (
    "Couldn't load data for backtest 'oef-2024q1': yahoo is missing prices for XYZ on 2024-02-14, a trading day. "
    "The backtest wasn't saved. Try another provider with --provider, or change the backtest dates, then run "
    "`fbull backtest create` again."
)
_MISSING_SEVERAL_DAYS = (
    "Couldn't load data for backtest 'oef-2024q1': yahoo is missing prices for XYZ on 3 trading days: "
    "2024-02-14, 2024-02-20 to 2024-02-21. The backtest wasn't saved. Try another provider with --provider, "
    "or change the backtest dates, then run `fbull backtest create` again."
)
_MISSING_TWO_SYMBOLS = (
    "Couldn't load data for backtest 'oef-2024q1': yahoo is missing prices on trading days for 2 symbols. "
    "The backtest wasn't saved.\n"
    "  AAA: 2024-02-14\n"
    "  BBB: 2024-02-20 to 2024-02-21, 2024-03-05\n"
    "Try another provider with --provider, or change the backtest dates, then run `fbull backtest create` again."
)


@pytest.mark.parametrize("message", [_MISSING_ONE_DAY, _MISSING_SEVERAL_DAYS, _MISSING_TWO_SYMBOLS])
def test_backtest_create_missing_sessions_prints_the_message_as_sent(message, monkeypatch):
    """The broker's missing-session failure is the step message, printed under one ✖."""
    monkeypatch.setenv("TTY_COMPATIBLE", "0")
    monkeypatch.setenv("LOGGING_LEVEL", "ERROR")
    with (
        patch("foreverbull.broker.backtest.create", side_effect=_download_failed_stream(message)),
        patch("foreverbull.broker.backtest.delete") as mock_delete,
    ):
        result = _create_from_file()

    # The broker already removed the backtest.
    mock_delete.assert_not_called()
    assert result.exit_code == 1
    assert result.output == f"Using provider: yahoo (from --provider)\n✖ {message}\n"
    assert "⚠" not in result.output
    assert "previous close" not in result.output


def test_backtest_create_missing_sessions_lists_symbols_on_their_own_lines(monkeypatch):
    monkeypatch.setenv("TTY_COMPATIBLE", "0")
    monkeypatch.setenv("LOGGING_LEVEL", "ERROR")
    with patch("foreverbull.broker.backtest.create", side_effect=_download_failed_stream(_MISSING_TWO_SYMBOLS)):
        result = _create_from_file()

    assert result.output.splitlines() == [
        "Using provider: yahoo (from --provider)",
        (
            "✖ Couldn't load data for backtest 'oef-2024q1': yahoo is missing prices on trading days for 2 symbols. "
            "The backtest wasn't saved."
        ),
        "  AAA: 2024-02-14",
        "  BBB: 2024-02-20 to 2024-02-21, 2024-03-05",
        "Try another provider with --provider, or change the backtest dates, then run `fbull backtest create` again.",
    ]


def test_backtest_create_without_notices_prints_no_warning(monkeypatch):
    # A broker without the notices field, or a step with nothing to say,
    # sends a completed step with empty notices and an empty message.
    monkeypatch.delenv("LOGGING_LEVEL", raising=False)
    result = _create_with_notices(monkeypatch, [])
    assert result.exit_code == 0
    assert "⚠" not in result.output
    assert "✖" not in result.output


def test_step_notices_round_trip_and_leave_message_empty():
    # notices is its own field, so the failure text in message stays empty
    # and a client built before notices existed skips the field.
    step = common_pb.Step(state=common_pb.State.COMPLETED, notices=["First notice.", "Second notice."])
    decoded = common_pb.Step.from_binary(step.to_binary())
    assert list(decoded.notices) == ["First notice.", "Second notice."]
    assert decoded.message == ""
    # A step from a broker without notices decodes to an empty list.
    legacy = common_pb.Step.from_binary(common_pb.Step(state=common_pb.State.COMPLETED).to_binary())
    assert list(legacy.notices) == []


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


_NO_PRICE_DATA_COPY = (
    "Couldn't load data for backtest 'from_file': yahoo has no prices for FB between 2019-01-01 and 2020-12-31. "
    "The backtest wasn't saved. Check the symbol and dates, or try another provider with --provider, "
    "then run `fbull backtest create` again."
)


def _download_failed_stream(message: str):
    def stream(**_kwargs):
        yield backtest_service_pb.CreateBacktestResponse(
            CREATING=common_pb.Step(state=common_pb.State.COMPLETED),
            DOWNLOADING=common_pb.Step(state=common_pb.State.RUNNING),
        )
        yield backtest_service_pb.CreateBacktestResponse(
            CREATING=common_pb.Step(state=common_pb.State.COMPLETED),
            DOWNLOADING=common_pb.Step(state=common_pb.State.FAILED, message=message),
        )

    return stream


def test_backtest_create_no_price_data_prints_exact_copy_and_exits_1(monkeypatch):
    monkeypatch.setenv("TTY_COMPATIBLE", "0")
    monkeypatch.setenv("LOGGING_LEVEL", "ERROR")
    with (
        patch("foreverbull.broker.backtest.create", side_effect=_download_failed_stream(_NO_PRICE_DATA_COPY)),
        patch("foreverbull.broker.backtest.delete") as mock_delete,
    ):
        result = _create_from_file()

    mock_delete.assert_not_called()
    assert result.exit_code == 1
    assert result.output == f"Using provider: yahoo (from --provider)\n✖ {_NO_PRICE_DATA_COPY}\n"
    assert "later ticker" not in result.output.lower()
    assert "isin" not in result.output.lower()


_DOWNLOAD_OTHER_ERROR_COPY = (
    "Couldn't load data for backtest 'from_file': the price download failed for AAPL. The backtest wasn't saved. "
    "Try again in a few minutes, or use another provider with --provider."
)


def test_backtest_create_other_download_error_prints_exact_copy_and_exits_1(monkeypatch):
    """Non-no-bars download STATUS_ERROR must fail create like no-bars (internal #46)."""
    monkeypatch.setenv("TTY_COMPATIBLE", "0")
    monkeypatch.setenv("LOGGING_LEVEL", "ERROR")
    with (
        patch(
            "foreverbull.broker.backtest.create",
            side_effect=_download_failed_stream(_DOWNLOAD_OTHER_ERROR_COPY),
        ),
        patch("foreverbull.broker.backtest.delete") as mock_delete,
    ):
        result = _create_from_file()

    mock_delete.assert_not_called()
    assert result.exit_code == 1
    assert result.output == f"Using provider: yahoo (from --provider)\n✖ {_DOWNLOAD_OTHER_ERROR_COPY}\n"
    assert "rpc error" not in result.output.lower()
    assert "traceback" not in result.output.lower()
    assert "grpc" not in result.output.lower()


_INVALID_TICKER_COPY = (
    "Couldn't load data for backtest 'oef-2024q1': symbol 7 of 99 isn't a valid ticker. "
    "Use 1 to 32 letters, digits, or . ^ = _ -. The backtest wasn't saved. "
    "Fix that symbol and run `fbull backtest create` again."
)


def test_backtest_create_invalid_ticker_prints_exact_copy_and_exits_1(monkeypatch):
    monkeypatch.setenv("TTY_COMPATIBLE", "0")
    monkeypatch.setenv("LOGGING_LEVEL", "ERROR")
    with (
        patch("foreverbull.broker.backtest.create", side_effect=_download_failed_stream(_INVALID_TICKER_COPY)),
        patch("foreverbull.broker.backtest.delete") as mock_delete,
    ):
        result = _create_from_file()

    mock_delete.assert_not_called()
    assert result.exit_code == 1
    assert result.output == f"Using provider: yahoo (from --provider)\n✖ {_INVALID_TICKER_COPY}\n"
    assert "Downloading prices failed" not in result.output


_AMBIGUOUS_SYMBOL_COPY = (
    "Couldn't load data for backtest 'oef-2024q1': yahoo has more than one instrument for SYM. "
    "The backtest wasn't saved. Use the exact ticker with its exchange suffix (for example VOD.L), "
    "or use another provider with --provider."
)


def test_backtest_create_ambiguous_symbol_prints_exact_copy_and_exits_1(monkeypatch):
    monkeypatch.setenv("TTY_COMPATIBLE", "0")
    monkeypatch.setenv("LOGGING_LEVEL", "ERROR")
    with (
        patch("foreverbull.broker.backtest.create", side_effect=_download_failed_stream(_AMBIGUOUS_SYMBOL_COPY)),
        patch("foreverbull.broker.backtest.delete") as mock_delete,
    ):
        result = _create_from_file()

    mock_delete.assert_not_called()
    assert result.exit_code == 1
    assert result.output == f"Using provider: yahoo (from --provider)\n✖ {_AMBIGUOUS_SYMBOL_COPY}\n"


_OVER_SYMBOL_LIMIT_COPY = (
    "Couldn't load data for backtest 'oef-2024q1': it has 10,001 different symbols, and the limit is 10,000. "
    "The backtest wasn't saved. Remove symbols or split them across several backtests, "
    "then run `fbull backtest create` again."
)


def _create_failed_stream(message: str):
    """The broker refused the create before storing anything (CREATING failed)."""

    def stream(**_kwargs):
        yield backtest_service_pb.CreateBacktestResponse(
            CREATING=common_pb.Step(state=common_pb.State.RUNNING),
        )
        yield backtest_service_pb.CreateBacktestResponse(
            CREATING=common_pb.Step(state=common_pb.State.FAILED, message=message),
        )

    return stream


def test_backtest_create_over_symbol_limit_prints_exact_copy_and_exits_1(monkeypatch):
    """The broker counts symbols before it stores the backtest; its sentence is printed as is."""
    monkeypatch.setenv("TTY_COMPATIBLE", "0")
    monkeypatch.setenv("LOGGING_LEVEL", "ERROR")
    with (
        patch("foreverbull.broker.backtest.create", side_effect=_create_failed_stream(_OVER_SYMBOL_LIMIT_COPY)),
        patch("foreverbull.broker.backtest.delete") as mock_delete,
    ):
        result = _create_from_file()

    mock_delete.assert_not_called()
    assert result.exit_code == 1
    assert result.output == f"Using provider: yahoo (from --provider)\n✖ {_OVER_SYMBOL_LIMIT_COPY}\n"
    assert "10,001" in result.output
    assert "10,000" in result.output


def test_backtest_create_over_symbol_limit_at_download_prints_exact_copy_and_exits_1(monkeypatch):
    """The download step's own check removes the backtest and sends the same sentence."""
    monkeypatch.setenv("TTY_COMPATIBLE", "0")
    monkeypatch.setenv("LOGGING_LEVEL", "ERROR")
    with (
        patch("foreverbull.broker.backtest.create", side_effect=_download_failed_stream(_OVER_SYMBOL_LIMIT_COPY)),
        patch("foreverbull.broker.backtest.delete") as mock_delete,
    ):
        result = _create_from_file()

    mock_delete.assert_not_called()
    assert result.exit_code == 1
    assert result.output == f"Using provider: yahoo (from --provider)\n✖ {_OVER_SYMBOL_LIMIT_COPY}\n"
    assert "Downloading prices failed" not in result.output


_INGEST_DRIVER_COPY = (
    "Couldn't load data for backtest 'from_file': the data service is missing its database driver (psycopg). "
    "This is a bug in the Foreverbull install, not in your strategy or settings. Nothing was saved, so run "
    "`fbull backtest create` again once it's fixed."
)
_INGEST_DRIVER_CAUSE = "ModuleNotFoundError: No module named 'psycopg'"
_SECRET = "s3cr3t-Pa55"


def _ingest_failed_stream(message: str):
    def stream(**_kwargs):
        yield backtest_service_pb.CreateBacktestResponse(
            CREATING=common_pb.Step(state=common_pb.State.COMPLETED),
            DOWNLOADING=common_pb.Step(state=common_pb.State.COMPLETED),
            INGESTING=common_pb.Step(state=common_pb.State.RUNNING),
        )
        # The broker removed the backtest and copied its message into the step.
        yield backtest_service_pb.CreateBacktestResponse(
            CREATING=common_pb.Step(state=common_pb.State.COMPLETED),
            DOWNLOADING=common_pb.Step(state=common_pb.State.COMPLETED),
            INGESTING=common_pb.Step(state=common_pb.State.FAILED, message=message),
        )

    return stream


def test_backtest_create_ingest_missing_driver_prints_the_copy(monkeypatch):
    monkeypatch.setenv("TTY_COMPATIBLE", "0")
    monkeypatch.setenv("LOGGING_LEVEL", "ERROR")
    message = f"{_INGEST_DRIVER_COPY}\nDetails: {_INGEST_DRIVER_CAUSE}"
    with (
        patch("foreverbull.broker.backtest.create", side_effect=_ingest_failed_stream(message)),
        patch("foreverbull.broker.backtest.delete") as mock_delete,
    ):
        result = _create_from_file()

    # The broker already removed the backtest; the client does not delete again.
    mock_delete.assert_not_called()
    assert result.exit_code == 1
    assert result.output == f"Using provider: yahoo (from --provider)\n✖ {_INGEST_DRIVER_COPY}\n"
    assert "Could not connect to database" not in result.output


@pytest.mark.parametrize(
    "copy",
    [
        (
            "Couldn't load data for backtest 'from_file': there are no stored prices for META (earlier FB) "
            "between 2024-01-02 and 2024-03-28. The backtest wasn't saved. "
            "Try another provider with --provider, then run `fbull backtest create` again."
        ),
        (
            "Couldn't load data for backtest 'from_file': 2 symbols have no stored prices. The backtest wasn't saved.\n"
            "  META (earlier FB): no prices between 2024-01-02 and 2024-03-28.\n"
            "  OEF: no prices between 2024-01-02 and 2024-03-28.\n"
            "Try another provider with --provider, then run `fbull backtest create` again."
        ),
        (
            "Couldn't load data for backtest 'from_file': foreverbull couldn't read its record of ticker changes, "
            "so renamed symbols can't be followed. The backtest wasn't saved. "
            "Run `fbenv status` to check that the environment is running, then run `fbull backtest create` again."
        ),
    ],
    ids=["no-prices", "no-prices-block", "ticker-history"],
)
def test_backtest_create_ingest_refusal_prints_zipline_sentence_as_written(copy, monkeypatch):
    # The broker makes a refused ingest's sentence the whole step message.
    monkeypatch.setenv("TTY_COMPATIBLE", "0")
    monkeypatch.setenv("LOGGING_LEVEL", "ERROR")
    with (
        patch("foreverbull.broker.backtest.create", side_effect=_ingest_failed_stream(copy)),
        patch("foreverbull.broker.backtest.delete") as mock_delete,
    ):
        result = _create_from_file()

    mock_delete.assert_not_called()
    assert result.exit_code == 1
    assert result.output == f"Using provider: yahoo (from --provider)\n✖ {copy}\n"


_INGEST_UNREACHABLE_COPY = (
    "Couldn't load data for backtest 'from_file': Foreverbull's data service isn't reachable. "
    "Run `fbenv status` to check it's running. Nothing was saved, so run `fbull backtest create` again after that."
)
_INGEST_OTHER_COPY = (
    "Couldn't load data for backtest 'from_file': the data service failed while ingesting prices. "
    "Nothing was saved, so run `fbull backtest create` again once it's fixed."
)
_INGEST_NOT_REMOVED_COPY = (
    "Couldn't load data for backtest 'from_file': the data service failed while ingesting prices. "
    "The backtest was partly saved and couldn't be removed. Run `fbull backtest delete from_file` first. "
    "Run `fbull backtest create` again once it's fixed."
)


@pytest.mark.parametrize(
    ("copy", "cause"),
    [
        (_INGEST_DRIVER_COPY, _INGEST_DRIVER_CAUSE),
        (_INGEST_UNREACHABLE_COPY, 'OperationalError: connection to server at "postgres" refused'),
        (_INGEST_OTHER_COPY, "KeyError: 'AAPL'"),
        (_INGEST_NOT_REMOVED_COPY, "KeyError: 'AAPL'"),
    ],
    ids=["missing-driver", "unreachable", "other", "not-removed"],
)
def test_backtest_create_ingest_failure_exact_default_output(copy, cause, monkeypatch):
    monkeypatch.setenv("TTY_COMPATIBLE", "0")
    monkeypatch.setenv("LOGGING_LEVEL", "ERROR")
    message = f"{copy}\nDetails: {cause}"
    with patch("foreverbull.broker.backtest.create", side_effect=_ingest_failed_stream(message)):
        result = _create_from_file()

    assert result.exit_code == 1
    assert result.output == f"Using provider: yahoo (from --provider)\n✖ {copy}\n"
    # The exception text is only shown with --verbose.
    assert cause not in result.output
    assert "Details:" not in result.output


def test_backtest_create_ingest_failure_verbose_shows_the_cause(monkeypatch):
    monkeypatch.setenv("TTY_COMPATIBLE", "0")
    monkeypatch.setenv("LOGGING_LEVEL", "ERROR")
    message = f"{_INGEST_DRIVER_COPY}\nDetails: {_INGEST_DRIVER_CAUSE}"
    with (
        patch("foreverbull.broker.backtest.create", side_effect=_ingest_failed_stream(message)),
        tempfile.NamedTemporaryFile("w", suffix=".yaml", delete=False) as cfg_file,
    ):
        yaml.safe_dump(_file_payload(), cfg_file)
        cfg_file.flush()
        result = CliRunner().invoke(cli, ["--verbose", "backtest", "create", "--file", cfg_file.name])
    Path(cfg_file.name).unlink(missing_ok=True)

    assert result.exit_code == 1
    assert f"✖ {_INGEST_DRIVER_COPY}\n" in result.output
    assert result.output.endswith(f"Details: {_INGEST_DRIVER_CAUSE}\n")


@pytest.mark.parametrize(
    "hop",
    [
        "the Zipline pod couldn't reach Postgres: OperationalError: connection refused",
        "the broker couldn't reach the Zipline pod: connection error: dial tcp 10.96.0.12:50055: connection refused",
    ],
    ids=["postgres", "zipline-pod"],
)
def test_backtest_create_unreachable_verbose_names_the_hop(hop, monkeypatch):
    monkeypatch.setenv("TTY_COMPATIBLE", "0")
    monkeypatch.setenv("LOGGING_LEVEL", "ERROR")
    message = f"{_INGEST_UNREACHABLE_COPY}\nDetails: {hop}"
    with (
        patch("foreverbull.broker.backtest.create", side_effect=_ingest_failed_stream(message)),
        tempfile.NamedTemporaryFile("w", suffix=".yaml", delete=False) as cfg_file,
    ):
        yaml.safe_dump(_file_payload(), cfg_file)
        cfg_file.flush()
        result = CliRunner().invoke(cli, ["--verbose", "backtest", "create", "--file", cfg_file.name])
    Path(cfg_file.name).unlink(missing_ok=True)

    assert result.exit_code == 1
    assert f"✖ {_INGEST_UNREACHABLE_COPY}\n" in result.output
    assert result.output.endswith(f"Details: {hop}\n")


@pytest.mark.parametrize("level", ["ERROR", "WARNING"])
def test_backtest_create_ingest_failure_never_prints_the_password(level, monkeypatch):
    # A broker older than this client sends the URL unredacted.
    monkeypatch.setenv("TTY_COMPATIBLE", "0")
    monkeypatch.setenv("LOGGING_LEVEL", level)
    url = f"postgresql://foreverbull:{_SECRET}@postgres:5432/foreverbull"
    message = (
        f"error ingesting data: Could not connect to database at {url}"
        f"\nDetails: OperationalError: connection to {url} refused"
    )
    with patch("foreverbull.broker.backtest.create", side_effect=_ingest_failed_stream(message)):
        result = _create_from_file()

    assert result.exit_code == 1
    assert _SECRET not in result.output
    assert "postgresql://foreverbull:***@postgres:5432/foreverbull" in result.output


_LOAD_FAILED_COPY = (
    "Backtest from_file was created, but its details couldn't be loaded. Run `fbull backtest get from_file` to see it."
)


def _stream_failing_after_ingest(**_kwargs):
    done = common_pb.State.COMPLETED
    yield backtest_service_pb.CreateBacktestResponse(
        CREATING=common_pb.Step(state=done),
        DOWNLOADING=common_pb.Step(state=done),
        INGESTING=common_pb.Step(state=done),
    )
    raise _BrokenStream(_LOAD_FAILED_COPY, grpc.StatusCode.INTERNAL)


def test_backtest_create_keeps_the_backtest_when_only_loading_it_fails(monkeypatch):
    monkeypatch.setenv("TTY_COMPATIBLE", "0")
    with (
        patch("foreverbull.broker.backtest.create", side_effect=_stream_failing_after_ingest),
        patch("foreverbull.broker.backtest.delete") as mock_delete,
    ):
        result = _create_from_file()

    mock_delete.assert_not_called()
    assert result.exit_code == 1
    assert _LOAD_FAILED_COPY in " ".join(result.output.split())


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


def _oef_backtest(symbols: list[str], *, name: str = "oef-2024q1") -> backtest_pb.Backtest:
    return backtest_pb.Backtest(
        name=name,
        benchmark="OEF",
        provider="yahoo",
        periods=[
            backtest_pb.Backtest.Period(
                start_date=from_pydate_to_proto_date(date(2024, 1, 2)),
                end_date=from_pydate_to_proto_date(date(2024, 3, 28)),
                assets=[backtest_pb.Backtest.Period.Asset(symbol=symbol, enabled=True) for symbol in symbols],
            )
        ],
    )


def _get_lines(monkeypatch, stored: backtest_pb.Backtest, *, columns: int, verbose: bool = False) -> list[str]:
    """`fbull backtest get` output at the given terminal width."""
    monkeypatch.setenv("COLUMNS", str(columns))
    monkeypatch.setattr(backtest_cli.console, "_width", columns)
    monkeypatch.setattr(backtest_cli.console, "_height", 25)
    if verbose:
        monkeypatch.setenv("LOGGING_LEVEL", "INFO")
    else:
        monkeypatch.delenv("LOGGING_LEVEL", raising=False)
    with (
        patch("foreverbull.broker.backtest.get", return_value=stored),
        patch("foreverbull.broker.backtest.list_executions", return_value=[]),
    ):
        result = CliRunner().invoke(backtest, ["get", stored.name])
    assert result.exit_code == 0, result.stdout
    return result.stdout.splitlines()


@pytest.mark.parametrize("columns", [80, 120])
def test_backtest_get_shows_a_large_backtest_like_create(monkeypatch, columns):
    lines = _get_lines(monkeypatch, _oef_backtest(list(reversed(_OEF_2024Q1))), columns=columns)
    assert lines[:7] == [
        "Backtest",
        "  Name       oef-2024q1",
        "  Start      2024-01-02",
        "  End        2024-03-28",
        "  Symbols    99: AAPL, ABBV, ABT, ACN, ADBE and 94 more",
        "  Benchmark  OEF",
        "  Provider   yahoo",
    ]
    # One period with every symbol enabled is the record itself, so it isn't repeated.
    assert not any(line.startswith("Period") for line in lines)
    assert all(len(line) <= columns for line in lines)
    assert not any(line.lstrip().startswith(",") for line in lines)


@pytest.mark.parametrize("columns", [80, 120])
def test_backtest_get_prints_a_long_name_in_full(monkeypatch, columns):
    name = "oef-2024q1-" + "x" * 90
    lines = _get_lines(monkeypatch, _oef_backtest(_OEF_2024Q1, name=name), columns=columns)
    assert f"  Name       {name}" in lines


@pytest.mark.parametrize("columns", [80, 120])
def test_backtest_get_verbose_lists_every_symbol_breaking_between_tickers(monkeypatch, columns):
    lines = _get_lines(monkeypatch, _oef_backtest(list(reversed(_OEF_2024Q1))), columns=columns, verbose=True)
    start = lines.index(next(line for line in lines if line.startswith("  Symbols    ")))
    end = lines.index("  Benchmark  OEF")
    symbol_lines = lines[start:end]
    assert len(symbol_lines) > 1
    assert all(len(line) <= columns for line in symbol_lines)
    assert all(line.endswith(",") for line in symbol_lines[:-1])
    assert not any(line.lstrip().startswith(",") for line in symbol_lines)
    shown = " ".join(line.strip() for line in symbol_lines).removeprefix("Symbols").strip()
    assert shown == "99: " + ", ".join(_OEF_2024Q1)


def test_backtest_get_shows_one_period_disabled_symbols_in_the_record(monkeypatch):
    lines = _get_lines(monkeypatch, _sample_backtest(), columns=80)
    assert lines[:8] == [
        "Backtest",
        "  Name       test_name",
        "  Start      2020-01-01",
        "  End        2020-01-31",
        "  Symbols    AAPL, MSFT",
        "  Disabled   IBM",
        "  Benchmark  SPY",
        "  Provider   yahoo",
    ]
    assert not any(line.startswith("Period") for line in lines)
    assert lines[8] == "", "a blank line comes before the executions"


def test_backtest_get_aligns_every_period_record(monkeypatch):
    stored = _sample_backtest()
    stored.periods.append(
        backtest_pb.Backtest.Period(
            start_date=from_pydate_to_proto_date(date(2020, 2, 1)),
            end_date=from_pydate_to_proto_date(date(2020, 2, 28)),
            assets=[
                backtest_pb.Backtest.Period.Asset(symbol="AAPL", enabled=True),
                backtest_pb.Backtest.Period.Asset(symbol="IBM", enabled=True),
            ],
        )
    )
    lines = _get_lines(monkeypatch, stored, columns=80)
    assert "  Disabled   IBM" not in lines[:8], "two periods keep disabled symbols in their records"
    p1 = lines.index("Period 1")
    p2 = lines.index("Period 2")
    assert lines[p1 - 1] == "" and lines[p2 - 1] == "", "records are separated by a blank line"
    assert lines[p1 + 1 : p2 - 1] == [
        "  Start      2020-01-01",
        "  End        2020-01-31",
        "  Enabled    AAPL, MSFT",
        "  Disabled   IBM",
    ]
    assert lines[p2 + 1 : p2 + 4] == [
        "  Start      2020-02-01",
        "  End        2020-02-28",
        "  Enabled    AAPL, IBM",
    ]
    rows = [line for line in lines[: p2 + 4] if line.startswith("  ")]
    assert rows and all(line[13] != " " and line[11:13] == "  " for line in rows), rows


def test_backtest_create_and_get_share_the_symbol_cap():
    from foreverbull.models.finance import BACKTEST_SYMBOLS_SHOWN

    assert backtest_cli.BACKTEST_SYMBOLS_SHOWN is BACKTEST_SYMBOLS_SHOWN
    items = backtest_cli._symbol_items(_OEF_2024Q1, verbose=False)
    assert len(items) == BACKTEST_SYMBOLS_SHOWN


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
        "starting_cash": 100_000.0,
        "starting_value": 0.0,
        # ending_value is positions only. The summary must not print it as End.
        "ending_value": 90_000.0,
        "algorithm_period_return": 0.042,
        "max_drawdown": -0.068,
        "sharpe": 1.35,
        "sortino": 1.1,
        "benchmark_period_return": 0.031,
        "longs_count": 2,
    }
    fields.update(overrides)
    return execution_pb.Period(**fields)


def _invoke_run(
    runner, mock_backtest_session, periods, backtest_entity=None, execution_entity=None, args=None, app=None
):
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
        get_execution.return_value = (execution_entity or _sample_execution(), periods, [], "yahoo")
        mock_foreverbull.return_value.backtest_session.return_value.__enter__.return_value = mock_backtest_session
        result = runner(app or backtest, args or ["run", "test_name", "demo.py"])
        create_session.assert_called_once()
        return result, get_execution


_LEFT_OUT = "3 symbols were delisted after their last price on 2026-03-31 and were left out: ABC, DEF, GHI."
_LEFT_OUT_XYZ = "1 symbol was delisted after its last price on 2026-04-15 and was left out: XYZ."
_LEFT_OUT_THIRD = "2 symbols were delisted after their last price on 2026-05-29 and were left out: JKL, MNO."


def _run_with_notices(runner, notices, args=None, app=None):
    execution = _sample_execution()
    execution.notices = notices
    return _invoke_run(runner, _run_session_mocks(), [_final_period()], execution_entity=execution, args=args, app=app)


def _warnings(result) -> list[str]:
    return [line.strip() for line in result.stdout.splitlines() if "⚠" in line]


def test_backtest_run_summary_names_members_left_out_on_one_date(runner, monkeypatch):
    monkeypatch.delenv("LOGGING_LEVEL", raising=False)
    result, _ = _run_with_notices(runner, [_LEFT_OUT])
    assert result.exit_code == 0
    assert _warnings(result) == [f"⚠ {_LEFT_OUT}"]
    assert result.stdout.index("Active days") < result.stdout.index(_LEFT_OUT) < result.stdout.index("Details:")


def test_backtest_run_summary_prints_one_line_per_date_for_two_dates(runner, monkeypatch):
    # The broker stores one line per date the prices ended. With two dates
    # the summary prints each, so a delisted member is never dropped silently.
    monkeypatch.delenv("LOGGING_LEVEL", raising=False)
    result, _ = _run_with_notices(runner, [_LEFT_OUT, _LEFT_OUT_XYZ])
    assert result.exit_code == 0
    assert _warnings(result) == [f"⚠ {_LEFT_OUT}", f"⚠ {_LEFT_OUT_XYZ}"]


def test_backtest_run_summary_folds_three_dates_into_one_line(runner, monkeypatch):
    monkeypatch.delenv("LOGGING_LEVEL", raising=False)
    result, _ = _run_with_notices(runner, [_LEFT_OUT, _LEFT_OUT_XYZ, _LEFT_OUT_THIRD])
    assert result.exit_code == 0
    assert _warnings(result) == [
        "⚠ 6 symbols were delisted before the run ended and were left out. Run with --verbose for the list."
    ]
    assert "ABC" not in result.stdout


def test_backtest_run_summary_verbose_lists_every_date(runner, monkeypatch):
    # The global flag, as `fbull --verbose backtest create` reads it.
    monkeypatch.setenv("LOGGING_LEVEL", "ERROR")
    many = "7 symbols were delisted after their last price on 2026-06-30 and were left out: A, B, C, D, E, F, G."
    result, _ = _run_with_notices(
        runner,
        [_LEFT_OUT, _LEFT_OUT_XYZ, _LEFT_OUT_THIRD, many],
        args=["--verbose", "backtest", "run", "test_name", "demo.py"],
        app=cli,
    )
    assert result.exit_code == 0
    assert _warnings(result) == [
        "⚠ 13 symbols were delisted before the run ended and were left out.",
        f"⚠ {_LEFT_OUT}",
        f"⚠ {_LEFT_OUT_XYZ}",
        f"⚠ {_LEFT_OUT_THIRD}",
        f"⚠ {many}",
    ]


def test_backtest_run_summary_names_at_most_five_symbols_per_line(runner, monkeypatch):
    monkeypatch.delenv("LOGGING_LEVEL", raising=False)
    nine = "9 symbols were delisted after their last price on 2026-03-31 and were left out: ABC, DEF, GHI, JKL, MNO, PQR, STU, VWX, YZA."
    five = "5 symbols were delisted after their last price on 2026-04-30 and were left out: A, B, C, D, E."
    result, _ = _run_with_notices(runner, [nine, five])
    assert result.exit_code == 0
    assert _warnings(result) == [
        "⚠ 9 symbols were delisted after their last price on 2026-03-31 and were left out: ABC, DEF, GHI, JKL, MNO and 4 more.",
        f"⚠ {five}",
    ]


def test_backtest_run_summary_says_held_positions_were_closed_only_on_their_line(runner, monkeypatch):
    monkeypatch.delenv("LOGGING_LEVEL", raising=False)
    held = (
        "1 symbol was delisted after its last price on 2023-01-13 and was left out: XYZ. "
        "Positions held were closed at the last price on 2023-01-13."
    )
    result, _ = _run_with_notices(runner, [held, _LEFT_OUT_XYZ])
    assert result.exit_code == 0
    assert _warnings(result) == [f"⚠ {held}", f"⚠ {_LEFT_OUT_XYZ}"]


def test_backtest_run_summary_held_line_keeps_its_sentence_after_five_symbols(runner, monkeypatch):
    monkeypatch.delenv("LOGGING_LEVEL", raising=False)
    held = (
        "7 symbols were delisted after their last price on 2023-01-13 and were left out: A, B, C, D, E, F, G. "
        "Positions held were closed at the last price on 2023-01-13."
    )
    result, _ = _run_with_notices(runner, [held])
    assert _warnings(result) == [
        (
            "⚠ 7 symbols were delisted after their last price on 2023-01-13 and were left out: A, B, C, D, E and 2 more. "
            "Positions held were closed at the last price on 2023-01-13."
        )
    ]


def test_backtest_run_summary_folded_line_says_held_positions_were_closed(runner, monkeypatch):
    monkeypatch.delenv("LOGGING_LEVEL", raising=False)
    held = (
        "2 symbols were delisted after their last price on 2026-05-29 and were left out: JKL, MNO. "
        "Positions held were closed at the last price on 2026-05-29."
    )
    result, _ = _run_with_notices(runner, [_LEFT_OUT, _LEFT_OUT_XYZ, held])
    assert _warnings(result) == [
        (
            "⚠ 6 symbols were delisted before the run ended and were left out. "
            "Positions held were closed at their last price. Run with --verbose for the list."
        )
    ]


def test_backtest_run_summary_folded_line_without_held_positions_says_nothing_of_a_close(runner, monkeypatch):
    monkeypatch.delenv("LOGGING_LEVEL", raising=False)
    result, _ = _run_with_notices(runner, [_LEFT_OUT, _LEFT_OUT_XYZ, _LEFT_OUT_THIRD])
    assert "Positions held" not in result.stdout


def test_backtest_run_summary_prefixes_each_notice_with_the_create_warning_sign(runner, monkeypatch):
    # The same "⚠ " the create notices print, inside the summary's indent.
    monkeypatch.delenv("LOGGING_LEVEL", raising=False)
    result, _ = _run_with_notices(runner, [_LEFT_OUT, _LEFT_OUT_XYZ])
    shown = [line for line in result.stdout.splitlines() if "⚠" in line]
    assert shown == [f"  ⚠ {_LEFT_OUT}", f"  ⚠ {_LEFT_OUT_XYZ}"]


def test_backtest_run_summary_shows_a_listing_next_to_an_auto_close(runner, monkeypatch):
    monkeypatch.delenv("LOGGING_LEVEL", raising=False)
    auto_close = "1 symbol was delisted after its last price on 2024-03-01 and was left out: OLD."
    listing = "XYZ was listed on 2024-03-15 and has no prices before then."
    result, _ = _run_with_notices(runner, [auto_close, listing])
    assert result.exit_code == 0
    assert _warnings(result) == [f"⚠ {auto_close}", f"⚠ {listing}"]


def test_backtest_run_summary_without_notices_prints_no_warning(runner):
    result, _ = _invoke_run(runner, _run_session_mocks(), [_final_period()])
    assert result.exit_code == 0
    assert "⚠" not in result.stdout


def test_backtest_run_prints_results_summary(runner):
    mock_backtest_session = _run_session_mocks()
    result, get_execution = _invoke_run(
        runner,
        mock_backtest_session,
        [
            _final_period(
                algorithm_period_return=0.01,
                portfolio_value=101_000.0,
                starting_cash=100_000.0,
                starting_value=0.0,
                longs_count=0,
            ),
            # Last morning's cash plus positions is 102_000. Start stays the first open.
            _final_period(starting_cash=40_000.0, starting_value=62_000.0),
        ],
    )
    assert result.exit_code == 0
    get_execution.assert_called_once_with("exec-1")
    mock_backtest_session.run_execution.assert_called_once()
    assert "Backtest finished: test_name · 2020-01-01 → 2020-01-31 · provider: yahoo" in result.stdout
    # The last period holds the cumulative numbers for the whole run.
    assert "$100,000.00" in result.stdout
    assert "$104,200.00" in result.stdout
    assert "$102,000.00" not in result.stdout
    assert "$90,000.00" not in result.stdout
    assert "+4.2%" in result.stdout
    assert "+1.0%" not in result.stdout
    assert "Benchmark (SPY)" in result.stdout
    assert "+3.1%" in result.stdout
    assert "1.35" in result.stdout
    assert "1.10" in result.stdout
    assert "-6.8%" in result.stdout
    assert "1 of 2" in result.stdout
    drawdown_line = next(line for line in result.stdout.splitlines() if "Max drawdown" in line)
    assert "Active days" in drawdown_line
    assert "1 of 2" in drawdown_line
    summary = result.stdout
    assert (
        summary.index("Start")
        < summary.index("Return")
        < summary.index("Sharpe")
        < summary.index("Max drawdown")
        < summary.index("Active days")
    )
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
    assert "Return and drawdown stay at 0 and Sharpe and Sortino are not defined." in result.stdout
    assert "+0.0%" in result.stdout
    assert "+24.8%" in result.stdout
    assert "0 of 250" in result.stdout
    sharpe_line = next(line for line in result.stdout.splitlines() if line.strip().startswith("Sharpe"))
    assert "-" in sharpe_line
    assert "0.00" not in sharpe_line
    drawdown_line = next(line for line in result.stdout.splitlines() if "Max drawdown" in line)
    assert "0.0%" in drawdown_line
    assert "+0.0%" not in drawdown_line
    assert "Active days" in drawdown_line
    assert "0 of 250" in drawdown_line


def test_backtest_run_with_a_held_position_has_no_no_trades_line(runner):
    periods = [_final_period(longs_count=0), _final_period(longs_count=0, shorts_count=1), _final_period(longs_count=0)]
    result, _ = _invoke_run(runner, _run_session_mocks(), periods)
    assert result.exit_code == 0
    assert "No trades" not in result.stdout
    drawdown_line = next(line for line in result.stdout.splitlines() if "Max drawdown" in line)
    assert "Active days" in drawdown_line
    assert "1 of 3" in drawdown_line


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


# A run with nothing to run over fails fast instead of exiting 0 silently.
@pytest.mark.parametrize("extra", [[], ["--dry"]], ids=["run", "dry"])
def test_backtest_run_without_periods_fails_with_one_line(monkeypatch, extra):
    monkeypatch.setenv("TTY_COMPATIBLE", "0")
    with (
        patch("foreverbull.broker.backtest.get") as get,
        patch("foreverbull.broker.backtest.create_session") as create_session,
    ):
        get.return_value = backtest_pb.Backtest(name="empty", benchmark="SPY")
        result = CliRunner().invoke(backtest, ["run", "empty", "demo.py", *extra])
    assert result.exit_code == 1
    assert isinstance(result.exception, SystemExit)
    assert result.stdout.count("✖") == 1
    assert (
        "✖ Backtest 'empty' has no data to run. Delete it with `fbull backtest delete empty`, "
        "then create it again with `fbull backtest create`."
    ) in " ".join(result.stdout.split())
    assert "Traceback" not in result.stdout
    assert "Dry Run" not in result.stdout
    create_session.assert_not_called()


def test_backtest_run_unknown_name_prints_one_plain_line(monkeypatch):
    monkeypatch.setenv("TTY_COMPATIBLE", "0")
    missing = _BrokenStream("backtest typo not found", grpc.StatusCode.NOT_FOUND)
    with (
        patch("foreverbull.broker.backtest.get", side_effect=missing),
        patch("foreverbull.broker.backtest.create_session") as create_session,
    ):
        result = CliRunner().invoke(backtest, ["run", "typo", "demo.py"])

    assert result.exit_code == 1
    assert result.output == (
        "✖ There's no backtest named 'typo'. Run `fbull backtest list` to see your "
        "backtests, or create it with `fbull backtest create`.\n"
    )
    assert "no rows in result set" not in result.output
    create_session.assert_not_called()


def test_backtest_run_other_lookup_errors_are_not_reported_as_missing():
    broken = _BrokenStream("Socket closed")
    with patch("foreverbull.broker.backtest.get", side_effect=broken):
        result = CliRunner().invoke(backtest, ["run", "demo", "demo.py"])

    assert result.exit_code != 0
    assert result.exception is broken
    assert "no backtest named" not in result.output


def test_backtest_run_invalid_start(runner):
    with patch("foreverbull.broker.backtest.get") as get:
        get.return_value = _sample_backtest()
        result = runner(backtest, ["run", "test_name", "demo.py", "--start", "not-a-date"])
        assert result.exit_code == 0
        assert "Invalid start date" in result.stdout


def _invoke_run_with_dates(args):
    with (
        patch("foreverbull.broker.backtest.get") as get,
        patch("foreverbull.broker.backtest.create_session") as create_session,
    ):
        get.return_value = _sample_backtest()
        result = CliRunner().invoke(backtest, ["run", "test_name", "demo.py", *args])
    return result, create_session


def test_backtest_run_start_before_backtest_fails_fast(monkeypatch):
    monkeypatch.setenv("TTY_COMPATIBLE", "0")
    result, create_session = _invoke_run_with_dates(["--start", "2019-06-01"])
    assert result.exit_code == 1
    assert result.output == (
        "✖ --start 2019-06-01 is before backtest 'test_name' begins. Pick dates between 2020-01-01 "
        "and 2020-01-31, or leave out --start and --end to run the whole period.\n"
    )
    create_session.assert_not_called()


def test_backtest_run_end_after_backtest_fails_fast(monkeypatch):
    monkeypatch.setenv("TTY_COMPATIBLE", "0")
    result, create_session = _invoke_run_with_dates(["--end", "2020-03-01"])
    assert result.exit_code == 1
    assert result.output == (
        "✖ --end 2020-03-01 is after backtest 'test_name' ends. Pick dates between 2020-01-01 "
        "and 2020-01-31, or leave out --start and --end to run the whole period.\n"
    )
    create_session.assert_not_called()


def test_backtest_run_both_dates_out_of_range_prints_one_line_per_flag(monkeypatch):
    monkeypatch.setenv("TTY_COMPATIBLE", "0")
    result, create_session = _invoke_run_with_dates(["--start", "2019-06-01", "--end", "2020-03-01"])
    assert result.exit_code == 1
    lines = result.output.splitlines()
    assert len(lines) == 2
    assert lines[0].startswith("✖ --start 2019-06-01 is before backtest 'test_name' begins.")
    assert lines[1].startswith("✖ --end 2020-03-01 is after backtest 'test_name' ends.")
    create_session.assert_not_called()


def test_backtest_run_start_after_backtest_end_fails_fast(monkeypatch):
    monkeypatch.setenv("TTY_COMPATIBLE", "0")
    result, create_session = _invoke_run_with_dates(["--start", "2020-02-15"])
    assert result.exit_code == 1
    assert result.output.startswith("✖ --start 2020-02-15 is after backtest 'test_name' ends.")
    create_session.assert_not_called()


def test_backtest_run_start_after_end_fails_fast(monkeypatch):
    monkeypatch.setenv("TTY_COMPATIBLE", "0")
    result, create_session = _invoke_run_with_dates(["--start", "2020-01-20", "--end", "2020-01-10"])
    assert result.exit_code == 1
    assert result.output == (
        "✖ --start 2020-01-20 is after --end 2020-01-10. Pick a start date on or before the end date.\n"
    )
    create_session.assert_not_called()


def test_backtest_run_dry_with_out_of_range_date_fails_fast(monkeypatch):
    monkeypatch.setenv("TTY_COMPATIBLE", "0")
    result, create_session = _invoke_run_with_dates(["--end", "2021-01-01", "--dry"])
    assert result.exit_code == 1
    assert "Dry Run" not in result.output
    assert result.output.startswith("✖ --end 2021-01-01 is after backtest 'test_name' ends.")
    create_session.assert_not_called()


def test_backtest_run_dry_with_dates_inside_range(runner):
    with (
        patch("foreverbull.broker.backtest.get") as get,
        patch("foreverbull.broker.backtest.create_session") as create_session,
    ):
        get.return_value = _sample_backtest()
        result = runner(
            backtest, ["run", "test_name", "demo.py", "--start", "2020-01-01", "--end", "2020-01-31", "--dry"]
        )
        assert result.exit_code == 0
        assert "Dry Run" in result.stdout
        create_session.assert_not_called()


def test_backtest_run_with_dates_inside_range_runs(runner):
    mock_backtest_session = _run_session_mocks()
    with (
        patch("foreverbull_cli.bull.backtest.Foreverbull") as mock_foreverbull,
        patch("foreverbull.models.Algorithm.from_file_path") as from_file_path,
        patch("foreverbull.broker.backtest.create_session") as create_session,
        patch("foreverbull.broker.backtest.get") as get,
        patch("foreverbull.broker.backtest.get_execution") as get_execution,
    ):
        from_file_path.return_value = True
        get.return_value = _sample_backtest()
        create_session.return_value = iter(
            [backtest_service_pb.CreateSessionResponse(session=session_pb.Session(port=7878))]
        )
        get_execution.return_value = (_sample_execution(), [_final_period()], [], "yahoo")
        mock_foreverbull.return_value.backtest_session.return_value.__enter__.return_value = mock_backtest_session
        result = runner(backtest, ["run", "test_name", "demo.py", "--start", "2020-01-10", "--end", "2020-01-20"])
        assert result.exit_code == 0
        create_session.assert_called_once()
        mock_backtest_session.run_execution.assert_called_once_with(
            from_pydate_to_proto_date(date(2020, 1, 10)), from_pydate_to_proto_date(date(2020, 1, 20))
        )


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
        mock_get_execution.return_value = (_sample_execution(full_id), [period], [], "yahoo")
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


def test_backtest_execution_shows_pinned_inputs(runner):
    full_id = "01234567-89ab-cdef-0123-456789abcdef"
    functions = [
        execution_pb.ExecutionFunction(
            name="rebalance",
            parameters=[
                execution_pb.ExecutionFunction.Parameter(key="fastperiod", value="12", value_type="int"),
                execution_pb.ExecutionFunction.Parameter(key="cash_reserve", value="0.05", value_type="float"),
            ],
        )
    ]
    with (
        patch("foreverbull.broker.backtest.get_execution") as mock_get_execution,
        patch("foreverbull.broker.backtest.list_executions") as mock_list_executions,
    ):
        mock_get_execution.return_value = (_sample_execution(full_id), [], functions, "stooq")
        result = runner(backtest, ["execution", full_id])
        assert result.exit_code == 0
        mock_list_executions.assert_not_called()
        assert "stooq" in result.stdout
        assert "Pinned inputs" in result.stdout
        assert "rebalance" in result.stdout
        assert "fastperiod" in result.stdout
        assert "12" in result.stdout
        assert "cash_reserve" in result.stdout


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
        mock_get_execution.return_value = (_sample_execution(full_id), [], [], "yahoo")
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
        mock_get_execution.return_value = (_sample_execution(full_id), [], [], "yahoo")
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


class _BrokerRejection(grpc.RpcError):
    def __init__(self, details: str, code: grpc.StatusCode = grpc.StatusCode.INVALID_ARGUMENT):
        self._details = details
        self._code = code

    def code(self):
        return self._code

    def details(self):
        return self._details


@pytest.mark.parametrize(
    ("message", "code"),
    [
        (
            (
                "Start 2019-12-30 is before backtest 'test_name' begins. Pick dates between 2020-01-01 and 2020-01-31, "
                "or leave out the dates to run the whole backtest."
            ),
            grpc.StatusCode.INVALID_ARGUMENT,
        ),
        (
            "Start 2020-01-31 is after end 2020-01-02. Pick a start on or before the end.",
            grpc.StatusCode.INVALID_ARGUMENT,
        ),
        (
            (
                "Backtest 'test_name' has no data to run. Delete it with `fbull backtest delete test_name`, "
                "then create it again with `fbull backtest create`."
            ),
            grpc.StatusCode.FAILED_PRECONDITION,
        ),
        (
            (
                "Couldn't run backtest 'test_name': the benchmark SPY has no prices on 3 trading days: "
                "2024-02-14, 2024-02-20 to 2024-02-21. Nothing was run. "
                "Run `fbull backtest create` again to reload its prices, or choose another benchmark."
            ),
            grpc.StatusCode.FAILED_PRECONDITION,
        ),
        (
            (
                "The price data for backtest 'test_name' can't be read: its version marker is damaged. "
                "Run `fbull backtest create` again to rebuild it."
            ),
            grpc.StatusCode.FAILED_PRECONDITION,
        ),
    ],
    ids=["before-start", "start-after-end", "no-periods", "benchmark-gap", "damaged-marker"],
)
def test_backtest_run_shows_broker_rejection_as_one_line(fb_env, message, code):
    mock_backtest_session = _run_session_mocks()
    mock_backtest_session.run_execution.side_effect = _BrokerRejection(message, code)
    with (
        patch("foreverbull_cli.bull.backtest.Foreverbull") as mock_foreverbull,
        patch("foreverbull.models.Algorithm.from_file_path") as from_file_path,
        patch("foreverbull.broker.backtest.create_session") as create_session,
        patch("foreverbull.broker.backtest.get") as get,
        patch("foreverbull.broker.backtest.get_execution") as get_execution,
    ):
        from_file_path.return_value = True
        get.return_value = _sample_backtest()
        create_session.return_value = iter(
            [backtest_service_pb.CreateSessionResponse(session=session_pb.Session(port=7878))]
        )
        context = mock_foreverbull.return_value.backtest_session.return_value
        context.__enter__.return_value = mock_backtest_session
        # A real context manager lets the error through.
        context.__exit__.return_value = False
        result = CliRunner().invoke(backtest, ["run", "test_name", "demo.py"])
    assert result.exit_code == 1
    assert isinstance(result.exception, SystemExit)
    assert result.stdout.count("✖") == 1
    assert f"✖ {message}" in " ".join(result.stdout.split())
    assert "Traceback" not in result.stdout
    get_execution.assert_not_called()


def test_backtest_run_permission_denied_exits_with_one_plain_line(fb_env):
    mock_backtest_session = _run_session_mocks()
    mock_backtest_session.run_execution.side_effect = _BrokerRejection(
        WORKER_MARKET_DATA_DENIED,
        grpc.StatusCode.FAILED_PRECONDITION,
    )
    with (
        patch("foreverbull_cli.bull.backtest.Foreverbull") as mock_foreverbull,
        patch("foreverbull.models.Algorithm.from_file_path") as from_file_path,
        patch("foreverbull.broker.backtest.create_session") as create_session,
        patch("foreverbull.broker.backtest.get") as get,
        patch("foreverbull.broker.backtest.get_execution") as get_execution,
    ):
        from_file_path.return_value = True
        get.return_value = _sample_backtest()
        create_session.return_value = iter(
            [backtest_service_pb.CreateSessionResponse(session=session_pb.Session(port=7878))]
        )
        context = mock_foreverbull.return_value.backtest_session.return_value
        context.__enter__.return_value = mock_backtest_session
        context.__exit__.return_value = False
        result = CliRunner().invoke(backtest, ["run", "test_name", "demo.py"])
    assert result.exit_code == 1
    assert isinstance(result.exception, SystemExit)
    assert result.stdout.count("✖") == 1
    assert f"✖ {market_data_denied_line('test_name')}" in " ".join(result.stdout.split())
    assert "No trades" not in result.stdout
    assert "SQLAlchemy" not in result.stdout
    assert "permission denied for table" not in result.stdout
    assert "worker" not in result.stdout.lower()
    assert "database" not in result.stdout.lower()
    assert "table" not in result.stdout.lower()
    assert "Traceback" not in result.stdout
    get_execution.assert_not_called()


_UNKNOWN_BACKTEST_LINE = (
    "✖ There's no backtest named 'typo'. Run `fbull backtest list` to see your "
    "backtests, or create it with `fbull backtest create`.\n"
)

# Deleting a backtest that doesn't exist only points at the list; suggesting to create
# the backtest the user was trying to delete would be odd.
_UNKNOWN_BACKTEST_DELETE_LINE = "✖ There's no backtest named 'typo'. Run `fbull backtest list` to see your backtests.\n"


def test_backtest_get_unknown_name_prints_one_plain_line(monkeypatch):
    monkeypatch.setenv("TTY_COMPATIBLE", "0")
    missing = _BrokenStream("backtest typo not found", grpc.StatusCode.NOT_FOUND)
    with (
        patch("foreverbull.broker.backtest.get", side_effect=missing),
        patch("foreverbull.broker.backtest.list_executions") as list_executions,
    ):
        result = CliRunner().invoke(backtest, ["get", "typo"])

    assert result.exit_code == 1
    assert result.output == _UNKNOWN_BACKTEST_LINE
    assert "or create it with `fbull backtest create`" in result.output
    list_executions.assert_not_called()


def test_backtest_get_other_lookup_errors_are_not_reported_as_missing():
    broken = _BrokenStream("Socket closed")
    with patch("foreverbull.broker.backtest.get", side_effect=broken):
        result = CliRunner().invoke(backtest, ["get", "demo"])

    assert result.exit_code != 0
    assert result.exception is broken
    assert "no backtest named" not in result.output


def test_backtest_delete_unknown_name_prints_one_plain_line(monkeypatch):
    monkeypatch.setenv("TTY_COMPATIBLE", "0")

    def get(name):
        if name == "typo":
            raise _BrokenStream("backtest typo not found", grpc.StatusCode.NOT_FOUND)
        return _sample_backtest()

    with (
        patch("foreverbull.broker.backtest.get", side_effect=get),
        patch("foreverbull.broker.backtest.delete") as delete,
    ):
        result = CliRunner().invoke(backtest, ["delete", "test_name", "typo", "--force"])

    assert result.exit_code == 1
    assert result.output == _UNKNOWN_BACKTEST_DELETE_LINE
    assert "fbull backtest create" not in result.output
    delete.assert_not_called()


def test_backtest_delete_other_lookup_errors_are_not_reported_as_missing():
    broken = _BrokenStream("Socket closed")
    with (
        patch("foreverbull.broker.backtest.get", side_effect=broken),
        patch("foreverbull.broker.backtest.delete") as delete,
    ):
        result = CliRunner().invoke(backtest, ["delete", "demo", "--force"])

    assert result.exit_code != 0
    assert result.exception is broken
    assert "no backtest named" not in result.output
    delete.assert_not_called()


def test_rpc_details_redacts_url_password():
    class _FakeRpc(grpc.RpcError):
        def details(self):
            return "could not connect to postgresql://foreverbull:s3cret@postgres:5432/foreverbull"

    text = backtest_cli._rpc_details(_FakeRpc())
    assert "s3cret" not in text
    assert "***@" in text


_PROVIDER_TIMEOUT_COPY = (
    "Couldn't load data for backtest 'oef-2024q1': alpaca took too long to respond for AAPL. "
    "The backtest wasn't saved. Try again later, or use another provider with --provider."
)


def test_backtest_create_provider_timeout_prints_exact_copy_and_exits_1(monkeypatch):
    monkeypatch.setenv("TTY_COMPATIBLE", "0")
    monkeypatch.setenv("LOGGING_LEVEL", "ERROR")

    def stream(**_kwargs):
        yield backtest_service_pb.CreateBacktestResponse(
            CREATING=common_pb.Step(state=common_pb.State.COMPLETED),
            DOWNLOADING=common_pb.Step(
                state=common_pb.State.RUNNING, message="Downloading prices: 1,240 of 10,000 symbols"
            ),
        )
        yield backtest_service_pb.CreateBacktestResponse(
            CREATING=common_pb.Step(state=common_pb.State.COMPLETED),
            DOWNLOADING=common_pb.Step(state=common_pb.State.FAILED, message=_PROVIDER_TIMEOUT_COPY),
        )

    with (
        patch("foreverbull.broker.backtest.create", side_effect=stream),
        patch("foreverbull.broker.backtest.delete") as mock_delete,
    ):
        result = _create_from_file(provider="alpaca")

    mock_delete.assert_not_called()
    assert result.exit_code == 1
    # The progress row is cleared with the others; only the sentence stays.
    assert result.output == f"Using provider: alpaca (from --provider)\n✖ {_PROVIDER_TIMEOUT_COPY}\n"


@pytest.mark.parametrize(
    "copy",
    [
        (
            "Couldn't load data for backtest 'oef-2024q1': the download hit the 2-hour limit after "
            "6,212 of 10,000 symbols. The backtest wasn't saved. Split the symbols across several backtests, "
            "then run `fbull backtest create` again."
        ),
        (
            "Couldn't load data for backtest 'oef-2024q1': the download hit the 1-hour limit after "
            "6,212 of 10,000 symbols. The backtest wasn't saved. Split the symbols across several backtests, "
            "then run `fbull backtest create` again."
        ),
        (
            "Couldn't load data for backtest 'oef-2024q1': the download hit the 90-minute limit after "
            "6,212 of 10,000 symbols. The backtest wasn't saved. Split the symbols across several backtests, "
            "then run `fbull backtest create` again."
        ),
    ],
    ids=["2h", "1h", "90m"],
)
def test_backtest_create_time_limit_prints_exact_copy_and_exits_1(monkeypatch, copy):
    monkeypatch.setenv("TTY_COMPATIBLE", "0")
    monkeypatch.setenv("LOGGING_LEVEL", "ERROR")

    def stream(**_kwargs):
        yield backtest_service_pb.CreateBacktestResponse(
            CREATING=common_pb.Step(state=common_pb.State.COMPLETED),
            DOWNLOADING=common_pb.Step(
                state=common_pb.State.RUNNING, message="Downloading prices: 6,212 of 10,000 symbols"
            ),
        )
        yield backtest_service_pb.CreateBacktestResponse(
            CREATING=common_pb.Step(state=common_pb.State.COMPLETED),
            DOWNLOADING=common_pb.Step(state=common_pb.State.FAILED, message=copy),
        )

    with (
        patch("foreverbull.broker.backtest.create", side_effect=stream),
        patch("foreverbull.broker.backtest.delete") as mock_delete,
    ):
        result = _create_from_file(provider="alpaca")

    mock_delete.assert_not_called()
    assert result.exit_code == 1
    assert result.output == f"Using provider: alpaca (from --provider)\n✖ {copy}\n"
