import logging
import shlex

from typing import Annotated
from typing import Self

import typer

from grpc._channel import _InactiveRpcError
from rich.progress import Progress
from rich.progress import TextColumn
from rich.table import Table

from foreverbull_cli.env.environment import TOOL_HINTS
from foreverbull_cli.env.environment import ClusterImagePullError
from foreverbull_cli.env.environment import ClusterManager
from foreverbull_cli.env.environment import CommandError
from foreverbull_cli.env.environment import Config
from foreverbull_cli.env.environment import Environment
from foreverbull_cli.env.environment import ImagePullError
from foreverbull_cli.env.environment import MissingToolError
from foreverbull_cli.env.environment import ReplicaSetRevisionError
from foreverbull_cli.env.environment import RolloutNotice
from foreverbull_cli.env.environment import VersionError
from foreverbull_cli.env.environment import WorkloadStatusError
from foreverbull_cli.env.environment import ready_timeout_seconds
from foreverbull_cli.env.preflight import CheckState
from foreverbull_cli.env.preflight import PreflightError
from foreverbull_cli.output import FBSpinnerColumn
from foreverbull_cli.output import console


log = logging.getLogger(__name__)


class FBProgress(Progress):
    def __init__(self):
        spinner = FBSpinnerColumn()
        text = TextColumn("[progress.description]{task.description}")
        super().__init__(spinner, text)

    def __enter__(self) -> Self:
        super().__enter__()
        return self

    def __exit__(self, exc_type, exc_val, exc_tb):
        if isinstance(exc_val, _InactiveRpcError):
            details = exc_val.details()
        else:
            details = exc_val

        for task in self.tasks:
            if task.total == 2 and task.completed == 1:
                if isinstance(exc_val, (ImagePullError, ClusterImagePullError, ReplicaSetRevisionError)):
                    # The line printed after the progress says what failed.
                    self.update(task.id, description=f"{task.description} FAILED")
                else:
                    self.update(task.id, description=f"{task.description} FAILED: {details}")
                self.update(task.id, completed=-1)

        super().__exit__(exc_type, exc_val, exc_tb)


cli = typer.Typer()

ClusterFlag = Annotated[str | None, typer.Option("--cluster", help="local (Kind) or remote kube context")]
KubeconfigFlag = Annotated[str | None, typer.Option("--kubeconfig", help="Kubeconfig path (remote)")]
ContextFlag = Annotated[str | None, typer.Option("--context", help="Kube context (remote)")]
NamespaceFlag = Annotated[str | None, typer.Option("--namespace", help="Helm namespace")]
BrokerAddressFlag = Annotated[str | None, typer.Option("--broker-address", help="Broker host:port")]
VerboseFlag = Annotated[
    bool,
    typer.Option(
        "--verbose", "-v", help="Show Docker's, the cluster's or kubectl's own error behind a failure or warning"
    ),
]


def cluster_flags(
    *,
    cluster: str | None = None,
    kubeconfig: str | None = None,
    context: str | None = None,
    namespace: str | None = None,
    broker_address: str | None = None,
) -> str:
    """The cluster-selection flags given on this command line, as a suffix to repeat on a suggested command."""
    given = (
        ("--cluster", cluster),
        ("--kubeconfig", kubeconfig),
        ("--context", context),
        ("--namespace", namespace),
        ("--broker-address", broker_address),
    )
    return "".join(f" {flag} {shlex.quote(value)}" for flag, value in given if value is not None)


def _manager(
    ctx: typer.Context,
    *,
    cluster: str | None = None,
    kubeconfig: str | None = None,
    context: str | None = None,
    namespace: str | None = None,
    broker_address: str | None = None,
) -> ClusterManager:
    cm: ClusterManager = ctx.obj
    cm.config.apply_cli_overrides(
        cluster=cluster,
        kubeconfig=kubeconfig,
        context=context,
        namespace=namespace,
        broker_address=broker_address,
    )
    return cm


def print_status(cm: ClusterManager):
    cfg = cm.config
    broker = cfg.broker_address or "unset"
    title = (
        f"Environment Status ({cfg.cluster}, ns={cfg.namespace}, "
        f"context={cfg.kube_context or 'default'}, broker={broker})"
    )
    table = Table(title=title)
    table.add_column("Service")
    table.add_column("Name")
    table.add_column("Status")
    table.add_column("Version")
    table.add_column("Container ID")
    try:
        rows = cm.status_rows()
    except MissingToolError as exc:
        message = (
            f"Couldn't check foreverbull's {'local' if cfg.is_local else 'remote'} environment: "
            "kubectl is not installed. "
            f"Install it from {TOOL_HINTS['kubectl']}, then run `fbenv status` again."
        )
        console.print(message, markup=False, highlight=False, soft_wrap=True)
        raise typer.Exit(code=1) from exc
    except WorkloadStatusError as exc:
        # The full kubectl command line (context, namespace, label) is for debugging, not the message.
        log.debug("kubectl command that failed: %s", exc.command)
        if cfg.is_local:
            hint = "Check that Docker is running, then run `fbenv status` again."
        else:
            hint = "Check that the remote cluster is reachable, then run `fbenv status` again."
        console.print(f"{exc}\n{hint}", markup=False, highlight=False, soft_wrap=True)
        raise typer.Exit(code=1) from exc
    for row in rows:
        table.add_row(row.service, row.name, row.status, row.image, row.container_id)
    console.print(table)
    diagnosis = cm.broker_diagnosis()
    if diagnosis is not None:
        console.print(diagnosis.advice)
        console.print(diagnosis.detail, markup=False, highlight=False)


CHECK_STYLES = {
    CheckState.PASS: "[green]pass[/green]",
    CheckState.FAIL: "[red]fail[/red]",
    CheckState.SKIP: "[dim]skip[/dim]",
}


def print_host_checks(cm: ClusterManager):
    table = Table(title="Host checks")
    table.add_column("Check")
    table.add_column("Result")
    table.add_column("Detail")
    for check in cm.host_checks():
        detail = check.detail
        if check.hint:
            detail = f"{detail}\n{check.hint}"
        table.add_row(check.name, CHECK_STYLES[check.state], detail)
    console.print(table)


def run_preflight(cm: ClusterManager):
    """Stop with the failed check and its fix hint before anything changes."""
    try:
        cm.preflight()
    except PreflightError as exc:
        console.print(str(exc), markup=False, highlight=False)
        raise typer.Exit(1) from exc


def check_web_address(cm: ClusterManager):
    """Refuse a web origin fbenv cannot pass to Grafana, before any change."""
    try:
        cm.config._helm_web_url()
    except ValueError as exc:
        console.print(str(exc), markup=False, highlight=False, soft_wrap=True)
        raise typer.Exit(1) from exc


def check_version(cm: ClusterManager):
    """Stop on a VERSION fbenv won't run, before host checks or any change."""
    try:
        _ = cm.config.version
    except VersionError as exc:
        lines = [f"✖ {exc}", exc.hint(), "Nothing was changed."]
        console.print("\n".join(lines), markup=False, highlight=False, soft_wrap=True)
        raise typer.Exit(1) from exc


def fail_image_pull(exc: ImagePullError, command: str, *, verbose: bool, nothing_changed: bool):
    """Stop with one ✖ line that names the image and the cause. Docker's output only with --verbose."""
    if exc.cause == "missing repository":
        # Lead with the PyPI fact; the checkout build in the hint is secondary.
        # Nothing-changed is promised only where it is true (local pulls run
        # before any change; a remote cluster can already have mutated).
        first = f"✖ The Foreverbull image {exc.image} isn't published yet, so this install can't start."
        if nothing_changed:
            first = f"{first} Nothing was changed."
        lines = [first, exc.hint(command)]
    else:
        lines = [f"✖ {exc}", exc.hint(command)]
        if nothing_changed:
            lines.append("Nothing was changed.")
    if verbose and exc.detail:
        lines.append(f"Docker said: {exc.detail}")
    elif exc.detail:
        lines.append(f"Run `{command} --verbose` to see Docker's error.")
    console.print("\n".join(lines), markup=False, highlight=False, soft_wrap=True)
    raise typer.Exit(1) from exc


def fail_cluster_pull(exc: ClusterImagePullError, command: str, *, verbose: bool):
    """Stop with one ✖ line that names the service and the image the cluster couldn't pull."""
    lines = [f"✖ {exc}", exc.hint(command)]
    if verbose:
        # The Kubernetes reason stays out of the ✖ line; --verbose is where it belongs.
        said = f"Kubernetes said: {exc.reason}"
        if exc.detail:
            said = f"{said}: {exc.detail}"
        lines.append(said)
    elif exc.detail:
        lines.append(f"Run `{command} --verbose` to see the cluster's error.")
    console.print("\n".join(lines), markup=False, highlight=False, soft_wrap=True)
    raise typer.Exit(1) from exc


def warn_rollout(notice: RolloutNotice, command: str, *, verbose: bool):
    """Print the ⚠ rollout line. kubectl's error only with --verbose."""
    lines = [notice.line]
    if verbose and notice.detail:
        lines.append(f"kubectl said: {notice.detail}")
    elif notice.detail:
        lines.append(f"Run `{command} --verbose` to see the details.")
    console.print("\n".join(lines), markup=False, highlight=False, soft_wrap=True)


def print_cluster(config: Config, *, path_note: str | None = None):
    if path_note:
        console.print(path_note)
    console.print(f"cluster: {config.cluster}")
    console.print(f"namespace: {config.namespace}")
    console.print(f"context: {config.kube_context or '(default)'}")
    console.print(f"kubeconfig: {config.kubeconfig or '(default)'}")
    console.print(f"broker: {config.broker_address or '(unset)'}")


@cli.command(help="Show host checks and the selected cluster")
def status(
    ctx: typer.Context,
    cluster: ClusterFlag = None,
    kubeconfig: KubeconfigFlag = None,
    context: ContextFlag = None,
    namespace: NamespaceFlag = None,
    broker_address: BrokerAddressFlag = None,
):
    cm = _manager(
        ctx,
        cluster=cluster,
        kubeconfig=kubeconfig,
        context=context,
        namespace=namespace,
        broker_address=broker_address,
    )
    print_host_checks(cm)
    print_status(cm)


@cli.command(help="Install the Kind cluster and Helm release. Workloads stay stopped.")
def create(
    ctx: typer.Context,
    start: Annotated[bool, typer.Option("--start", help="Start workloads after install")] = False,
    cluster: ClusterFlag = None,
    kubeconfig: KubeconfigFlag = None,
    context: ContextFlag = None,
    namespace: NamespaceFlag = None,
    broker_address: BrokerAddressFlag = None,
    verbose: VerboseFlag = False,
):
    cm = _manager(
        ctx,
        cluster=cluster,
        kubeconfig=kubeconfig,
        context=context,
        namespace=namespace,
        broker_address=broker_address,
    )
    check_version(cm)
    check_web_address(cm)
    run_preflight(cm)

    changing = False
    try:
        with FBProgress() as progress:
            verify_images = progress.add_task("Verifying images", total=2)
            create_task = progress.add_task("Creating environment", total=2)
            if start:
                start_task = progress.add_task("Starting environment", total=2)
            else:
                start_task = None

            progress.update(verify_images, completed=1)
            cm.verify_images()
            progress.update(verify_images, completed=2)

            changing = True
            progress.update(create_task, completed=1)
            cm.create()

            progress.update(create_task, completed=2)
            if start_task:
                progress.update(start_task, completed=1)
                cm.start()
                progress.update(start_task, completed=2)
    except ImagePullError as exc:
        fail_image_pull(exc, "fbenv create", verbose=verbose, nothing_changed=not changing)

    print_status(cm)
    if not start:
        console.print("Workloads are stopped. Start them with `fbenv start`.")


@cli.command(help="Scale workloads to 1 and wait until they are ready.")
def start(
    ctx: typer.Context,
    cluster: ClusterFlag = None,
    kubeconfig: KubeconfigFlag = None,
    context: ContextFlag = None,
    namespace: NamespaceFlag = None,
    broker_address: BrokerAddressFlag = None,
):
    cm = _manager(
        ctx,
        cluster=cluster,
        kubeconfig=kubeconfig,
        context=context,
        namespace=namespace,
        broker_address=broker_address,
    )
    run_preflight(cm)

    try:
        with FBProgress() as progress:
            start_task = progress.add_task("Starting environment", total=2)
            progress.update(start_task, completed=1)
            cm.start()
            progress.update(start_task, completed=2)
    except TimeoutError as exc:
        console.print(str(exc))
        raise typer.Exit(1) from exc

    print_status(cm)


@cli.command(help="Scale workloads to 0.")
def stop(
    ctx: typer.Context,
    remove: bool = typer.Option(False, "--remove", help="Uninstall the Helm release"),
    purge: bool = typer.Option(
        False,
        "--purge",
        help="With --remove, delete the Kind cluster (local) and .foreverbull data",
    ),
    cluster: ClusterFlag = None,
    kubeconfig: KubeconfigFlag = None,
    context: ContextFlag = None,
    namespace: NamespaceFlag = None,
    broker_address: BrokerAddressFlag = None,
):
    cm = _manager(
        ctx,
        cluster=cluster,
        kubeconfig=kubeconfig,
        context=context,
        namespace=namespace,
        broker_address=broker_address,
    )

    with FBProgress() as progress:
        stop_task = progress.add_task("Stopping environment", total=2)
        if remove:
            remove_task = progress.add_task("Removing environment", total=2)
        else:
            remove_task = None

        progress.update(stop_task, completed=1)
        try:
            cm.stop()
        except CommandError as exc:
            if remove_task is None:
                raise
            console.print(f"Scale to 0 failed ({exc}). Removing the release anyway.")
        else:
            progress.update(stop_task, completed=2)

        if remove_task:
            progress.update(remove_task, completed=1)
            cm.remove(purge)
            progress.update(remove_task, completed=2)
    if remove and purge:
        # The purge deleted what status would query (the Kind cluster, locally), so there is nothing to show.
        where = "local" if cm.config.is_local else "remote"
        console.print(f"The {where} environment and its data were removed.", markup=False, highlight=False)
        return
    print_status(cm)


@cli.command(help="update environment images")
def update(
    ctx: typer.Context,
    cluster: ClusterFlag = None,
    kubeconfig: KubeconfigFlag = None,
    context: ContextFlag = None,
    namespace: NamespaceFlag = None,
    broker_address: BrokerAddressFlag = None,
    verbose: VerboseFlag = False,
):
    cm = _manager(
        ctx,
        cluster=cluster,
        kubeconfig=kubeconfig,
        context=context,
        namespace=namespace,
        broker_address=broker_address,
    )
    check_version(cm)
    check_web_address(cm)

    rollout_notice: RolloutNotice | None = None
    try:
        with FBProgress() as progress:
            update_task = progress.add_task("Updating environment", total=2)
            progress.update(update_task, completed=1)
            notice = cm.update()
            rollout_notice = notice if isinstance(notice, RolloutNotice) else None
            progress.update(update_task, completed=2)
    except ImagePullError as exc:
        # Local update pulls every missing image before kind load and helm
        # upgrade, so nothing changed yet. A remote update has no local pull
        # step: its cluster pulls after Helm runs, so do not promise that.
        fail_image_pull(exc, "fbenv update", verbose=verbose, nothing_changed=cm.config.is_local)
    except ClusterImagePullError as exc:
        fail_cluster_pull(exc, "fbenv update", verbose=verbose)
    except ReplicaSetRevisionError as exc:
        if cm.config.is_local:
            # Flags on `fbenv update` apply to that run only, so repeat them for the reinstall commands.
            flags = cluster_flags(
                cluster=cluster,
                kubeconfig=kubeconfig,
                context=context,
                namespace=namespace,
                broker_address=broker_address,
            )
            lines = [
                exc.describe(remote=False),
                (
                    "To fix it, reinstall the environment. This deletes all of foreverbull's data: "
                    "stored prices, backtest results, accounts and API tokens."
                ),
                f"Run `fbenv stop --remove{flags}`, then `fbenv create --start{flags}`.",
            ]
        else:
            # fbenv doesn't own a remote cluster, so it doesn't offer a reinstall that deletes its data.
            lines = [
                exc.describe(remote=True),
                "Whoever manages that cluster needs to fix it before `fbenv update` can run.",
            ]
        console.print("\n".join(lines), markup=False, highlight=False, soft_wrap=True)
        raise typer.Exit(1) from exc

    if rollout_notice:
        warn_rollout(rollout_notice, "fbenv update", verbose=verbose)
    print_status(cm)


@cli.command(help="Select cluster: local (Kind) or remote. Writes .foreverbull/config.yaml")
def use(
    ctx: typer.Context,
    cluster: Annotated[str | None, typer.Argument(help="local or remote")] = None,
    kubeconfig: KubeconfigFlag = None,
    context: ContextFlag = None,
    namespace: NamespaceFlag = None,
    broker_address: BrokerAddressFlag = None,
):
    cm: ClusterManager = ctx.obj
    config: Config = cm.config
    if cluster is None:
        print_cluster(config)
        return
    try:
        path = config.persist_selection(
            cluster,
            kubeconfig=kubeconfig,
            context=context,
            namespace=namespace,
            broker_address=broker_address,
        )
    except ValueError as exc:
        raise typer.BadParameter(str(exc)) from exc
    print_cluster(config, path_note=f"Wrote {path}")


@cli.callback()
def initialize(ctx: typer.Context):
    try:
        timeout = ready_timeout_seconds()
    except ValueError as exc:
        console.print(f"✖ {exc}\nNothing was changed.", markup=False, highlight=False)
        raise typer.Exit(1) from exc
    config = Config()
    environment = Environment()
    ctx.obj = ClusterManager(environment, config, ready_timeout_seconds=timeout)
