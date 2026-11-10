import importlib.util
import json
import os
import pathlib
import re
import subprocess
import time

import pytest
import typer

from click.testing import Result
from cookiecutter.main import cookiecutter
from foreverbull_cli.bull.backtest import backtest as backtest_cli
from foreverbull_cli.env import cli as env_cli
from typer.testing import CliRunner

from foreverbull import broker


REPO_ROOT = pathlib.Path(__file__).resolve().parents[2]
TEMPLATES = REPO_ROOT / "client" / "templates" / "simple"
ZIPLINE_BUNDLE_SCRIPT = pathlib.Path(__file__).with_name("zipline_bundle.py")
# fbenv's defaults: Kind cluster `foreverbull`, Helm namespace `foreverbull`.
KIND_CONTEXT = "kind-foreverbull"
NAMESPACE = "foreverbull"
# summary.print_run_summary prints this when no period closed with a position.
NO_TRADES_NOTICE = "No trades:"
# Ceiling for the old Zipline pod to finish terminating after a rollout.
POD_GONE_TIMEOUT_S = 180


@pytest.fixture
def runner():
    runner = CliRunner()

    def invoke(app: typer.Typer, args: list[str], env: dict[str, str] | None = None) -> Result:
        if env is None:
            env = {}
        result = runner.invoke(app, args, env=env)
        if os.environ.get("ECHO"):
            print()
            print(result.stdout)

        return result

    return invoke


@pytest.fixture
def simple_algorithm(tmp_path):
    cookiecutter(
        str(TEMPLATES),
        no_input=True,
        extra_context={
            "project_name": "simple",
            "project_slug": "simple",
            "install_dependencies": False,
            "run_setup": False,
        },
        output_dir=str(tmp_path),
        overwrite_if_exists=True,
    )
    project = tmp_path / "simple"
    return project / "crossover.py", project / "backtest.yaml"


def zipline_bundle(action: str, backtest: str) -> str:
    """Run zipline_bundle.py in the Zipline pod, where the run reads its bundle."""
    completed = subprocess.run(
        [
            "kubectl",
            "--context",
            KIND_CONTEXT,
            "-n",
            NAMESPACE,
            "exec",
            "-i",
            "deploy/zipline",
            "--",
            "python",
            "-",
            action,
            backtest,
        ],
        input=ZIPLINE_BUNDLE_SCRIPT.read_text(encoding="utf-8"),
        capture_output=True,
        text=True,
        timeout=300,
        check=False,
    )
    assert completed.returncode == 0, completed.stdout + completed.stderr
    return completed.stdout


def kubectl(*args: str) -> str:
    completed = subprocess.run(
        ["kubectl", "--context", KIND_CONTEXT, "-n", NAMESPACE, *args],
        capture_output=True,
        text=True,
        timeout=330,
        check=False,
    )
    assert completed.returncode == 0, completed.stdout + completed.stderr
    return completed.stdout


def pod_names(app: str) -> set[str]:
    output = kubectl("get", "pods", "-l", f"app.kubernetes.io/name={app}", "-o", "jsonpath={.items[*].metadata.name}")
    return set(output.split())


def restart_zipline() -> None:
    """Roll deploy/zipline and wait until the old pod is deleted.

    The old pod has to be gone, not only Terminating, or a broker still
    holding its address could pass by reaching it.
    """
    old_pods = pod_names("zipline")
    assert old_pods, "no zipline pod before the restart"
    kubectl("rollout", "restart", "deploy/zipline")
    kubectl("rollout", "status", "deploy/zipline", "--timeout=300s")
    deadline = time.monotonic() + POD_GONE_TIMEOUT_S
    while old_pods & pod_names("zipline"):
        assert time.monotonic() < deadline, f"old zipline pods still present: {old_pods & pod_names('zipline')}"
        time.sleep(2)
    assert pod_names("zipline"), "no zipline pod after the restart"


def bundle_state(backtest: str) -> dict:
    return json.loads(zipline_bundle("state", backtest).strip().splitlines()[-1])


def exercise_broker_grpc_tls() -> None:
    path = pathlib.Path(__file__).with_name("grpc_tls.py")
    spec = importlib.util.spec_from_file_location("foreverbull_e2e_grpc_tls", path)
    if spec is None or spec.loader is None:
        raise RuntimeError(f"cannot load {path}")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    module.exercise_broker_grpc_tls()


def assert_run_traded(output: str) -> None:
    """The run made trades and measured a nonzero benchmark.

    Before the fix the demo held no position and its benchmark stayed at
    zero, so the summary printed the no-trades notice and +0.0%. The
    stored periods are read through the broker, as `fbull backtest
    execution` does, so a rounded summary cannot hide a zero.
    """
    assert NO_TRADES_NOTICE not in output, output
    assert re.search(r"Benchmark \(SPY\) total\s+[+-]\d+\.\d%", output) is not None, output
    match = re.search(r"fbull backtest execution ([0-9a-f]+)", output)
    assert match is not None, output
    ids = [current.id for current in broker.backtest.list_executions("demo") if current.id.startswith(match.group(1))]
    assert len(ids) == 1, ids
    _, periods, _, _ = broker.backtest.get_execution(ids[0])
    assert periods, ids[0]
    # A daily fill moves cash on its session, so these are the days with trades.
    trade_days = [period for period in periods if period.capital_used != 0]
    assert len(trade_days) > 0, "no period has a fill"
    assert any(period.longs_count > 0 or period.shorts_count > 0 for period in periods)
    last = periods[-1]
    assert last.has_field("benchmark_period_return"), "benchmark return is missing"
    assert last.benchmark_period_return != 0, "benchmark return is zero"


def test_start_stop(runner, simple_algorithm):
    """Install Kind with fbenv and run the simple template through fbull.

    CI retags the images built on this commit to `:local` and runs this test.
    """
    crossover, backtest_file = simple_algorithm

    result = runner(env_cli.cli, "stop --remove --purge")
    assert result.exit_code == 0, result.output

    result = runner(env_cli.cli, "create", env={"VERSION": "local"})
    assert result.exit_code == 0, result.output

    result = runner(env_cli.cli, "start", env={"VERSION": "local"})
    assert result.exit_code == 0, result.output
    # The image entrypoint runs apply-web-links before Grafana. Kind passes
    # the page origin, so the placeholder is gone and the overview link is absolute.
    linked = kubectl(
        "exec",
        "deploy/grafana",
        "--",
        "cat",
        "/etc/grafana/provisioning/dashboards/backtest-overview.json",
    )
    assert "http://localhost:8080/backtests" in linked
    assert "__FOREVERBULL_WEB_URL__" not in linked
    execution_dash = kubectl(
        "exec",
        "deploy/grafana",
        "--",
        "cat",
        "/etc/grafana/provisioning/dashboards/executions.json",
    )
    assert "http://localhost:8080${web_path}" in execution_dash
    assert "__FOREVERBULL_WEB_URL__" not in execution_dash

    # Remote gRPC through Traefik, before the demo's plaintext localhost calls.
    exercise_broker_grpc_tls()

    result = runner(backtest_cli, f"create --file {backtest_file}")
    assert result.exit_code == 0, result.output
    assert "Using provider: yahoo (default)" in result.output

    result = runner(backtest_cli, f"run demo {crossover}")
    assert result.exit_code == 0, result.output
    assert "Backtest finished: demo · 2021-01-01 → 2021-03-31 · provider: yahoo" in result.output
    assert "Total return" in result.output
    assert "Price return" in result.output
    assert_run_traded(result.output)

    match = re.search(r"fbull backtest execution ([0-9a-f]+)", result.output)
    assert match is not None, result.output
    execution = runner(backtest_cli, f"execution {match.group(1)}")
    assert execution.exit_code == 0, execution.output
    assert "Portfolio" in execution.output
    assert "2021-" in execution.output

    # A backtest created before the session-alignment fix: its stored
    # bundle carries 14:30 UTC asset dates and no version marker. It must
    # run without `fbull backtest create`, rebuilding exactly once.
    zipline_bundle("stale", "demo")
    assert bundle_state("demo") == {"ingests": 1, "version": 0, "misaligned": True}

    result = runner(backtest_cli, f"run demo {crossover}")
    assert result.exit_code == 0, result.output
    assert "Backtest finished: demo · 2021-01-01 → 2021-03-31 · provider: yahoo" in result.output
    assert_run_traded(result.output)
    # The rebuild writes a second, aligned ingest next to the stale one.
    assert bundle_state("demo") == {"ingests": 2, "version": 1, "misaligned": False}

    # The run downloads the stale ingestion again; the rebuilt ingest is
    # newer and stamped, so a second rebuild would only add a third.
    result = runner(backtest_cli, f"run demo {crossover}")
    assert result.exit_code == 0, result.output
    assert "Backtest finished: demo · 2021-01-01 → 2021-03-31 · provider: yahoo" in result.output
    assert_run_traded(result.output)
    assert bundle_state("demo") == {"ingests": 2, "version": 1, "misaligned": False}

    # Zipline restarts while the broker keeps running. The broker must
    # reach the new pod for both ingest and the session without a restart.
    broker_pods = pod_names("foreverbull")
    restart_zipline()
    restarted = backtest_file.with_name("restart.yaml")
    restarted.write_text(
        backtest_file.read_text(encoding="utf-8").replace("name: demo", "name: demo-restart", 1),
        encoding="utf-8",
    )

    result = runner(backtest_cli, f"create --file {restarted}")
    assert result.exit_code == 0, result.output

    result = runner(backtest_cli, f"run demo-restart {crossover}")
    assert result.exit_code == 0, result.output
    assert "Backtest finished: demo-restart · 2021-01-01 → 2021-03-31 · provider: yahoo" in result.output
    assert pod_names("foreverbull") == broker_pods, "the broker restarted during the Zipline rollout"
