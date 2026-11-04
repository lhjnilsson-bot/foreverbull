import logging
import os

from typing import Annotated

import grpc
import typer

from rich.logging import RichHandler

from foreverbull.broker.credentials import InsecureTransportError
from foreverbull.broker.credentials import TokenFileError
from foreverbull.broker.credentials import set_cli_token
from foreverbull_cli.bull.auth import login
from foreverbull_cli.bull.auth import logout
from foreverbull_cli.bull.backtest import backtest
from foreverbull_cli.bull.strategy import strategy
from foreverbull_cli.bull.token import token_app
from foreverbull_cli.bull.trading import trading
from foreverbull_cli.output import console


log = logging.getLogger(__name__)

SIGN_IN_EXPIRED = "Your sign-in has expired. Run `fbull login` to sign in again."
TOKEN_REFUSED = (
    "foreverbull didn't accept your token. Run `fbull login` to get a new one. "
    "If you set FOREVERBULL_TOKEN or --token, check that it holds the right token."
)


def unauthenticated_line(details: str) -> str:
    """The line for an UNAUTHENTICATED answer. The broker's own text goes to the log unless it is the expiry."""
    if details == SIGN_IN_EXPIRED:
        return SIGN_IN_EXPIRED
    log.info("broker refused the token: %s", details)
    return TOKEN_REFUSED


class _Cli(typer.Typer):
    """fbull. A refused credential ends the command with its message, not a traceback."""

    def __call__(self, *args, **kwargs):
        try:
            return super().__call__(*args, **kwargs)
        except (InsecureTransportError, TokenFileError) as exc:
            console.print(str(exc), style="red", highlight=False)
            raise SystemExit(1) from None
        except grpc.RpcError as exc:
            code = exc.code() if callable(getattr(exc, "code", None)) else None
            if code != grpc.StatusCode.UNAUTHENTICATED:
                raise
            details = exc.details() if callable(getattr(exc, "details", None)) else ""
            console.print(unauthenticated_line(details or ""), style="red", highlight=False)
            raise SystemExit(1) from None


cli = _Cli()

cli.add_typer(backtest, name="backtest")
cli.add_typer(strategy, name="strategy")
cli.add_typer(trading, name="trading")
cli.add_typer(token_app, name="token")
cli.command("login")(login)
cli.command("logout")(logout)

_LOG_LEVELS = {
    0: "ERROR",
    1: "WARNING",
    2: "INFO",
    3: "DEBUG",
}


def _logging_level(verbose: int) -> str:
    return _LOG_LEVELS.get(verbose, "DEBUG")


@cli.callback()
def setup_logging(
    verbose: Annotated[
        int,
        typer.Option("--verbose", "-v", count=True, help="Increase logging verbosity; repeat for more detail"),
    ] = 0,
    token: Annotated[
        str | None,
        typer.Option("--token", help="Bearer for this command. Not stored."),
    ] = None,
):
    set_cli_token(token)
    level = _logging_level(verbose)
    os.environ["LOGGING_LEVEL"] = level
    logging.basicConfig(
        level=level,
        format="%(message)s",
        datefmt="[%X]",
        handlers=[RichHandler(markup=True, console=console)],
    )
