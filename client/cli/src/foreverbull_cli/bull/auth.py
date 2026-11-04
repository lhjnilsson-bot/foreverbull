import logging

from typing import Annotated

import typer

from foreverbull.broker.config import resolve_broker_url
from foreverbull.broker.credentials import InsecureTransportError
from foreverbull.broker.credentials import TokenFileError
from foreverbull.broker.credentials import TokenMeta
from foreverbull.broker.credentials import delete_token_files
from foreverbull.broker.credentials import read_token_file
from foreverbull.broker.credentials import read_token_meta
from foreverbull.broker.credentials import token_path
from foreverbull_cli.bull.login import BrokerUnreachable
from foreverbull_cli.bull.login import Revoked
from foreverbull_cli.bull.login import SignInError
from foreverbull_cli.bull.login import revoke
from foreverbull_cli.bull.login import sign_in
from foreverbull_cli.bull.login import valid_until
from foreverbull_cli.output import console


log = logging.getLogger(__name__)

_LOGOUT_STEP = "Run `fbenv status` to check that the environment is running, then run `fbull logout` again."


def _fail(message: str) -> typer.Exit:
    console.print(message, style="red", highlight=False)
    return typer.Exit(1)


def signed_in_line(login: str, scope: str, path: object, meta: TokenMeta) -> str:
    return f"Signed in as {login} with {scope} access. Token saved to {path}. It's {valid_until(meta)}."


def removed_local_line(path: object, meta: TokenMeta | None) -> str:
    if meta is None:
        lifetime = "stays valid until it's revoked in the web UI"
    else:
        lifetime = f"is still {valid_until(meta)}"
    return (
        f"Removed {path}. The token itself {lifetime}. "
        "To revoke it now, use Sign out everywhere on your account page in the web UI."
    )


def revoke_failed_line(detail: str, path: object, meta: TokenMeta | None) -> str:
    return f"Couldn't revoke the token: {detail}. {_kept(path, meta)} {_LOGOUT_STEP}"


def _kept(path: object, meta: TokenMeta | None) -> str:
    if meta is None:
        return f"Kept {path}, and the token stays valid until it's revoked in the web UI."
    return f"Kept {path}, and the token is still {valid_until(meta)}."


def other_broker_line(made_by: str, selected: str, path: object, meta: TokenMeta | None) -> str:
    return (
        f"Couldn't revoke the token: it was made by foreverbull at {made_by}, but fbull is set to use foreverbull "
        f"at {selected}. {_kept(path, meta)} To revoke it, set FOREVERBULL_BROKER_URL to {made_by} and run "
        "`fbull logout` again, or run `fbull logout --local` to only remove the file."
    )


def _sentence(reason: str) -> str:
    """The reason with exactly one closing period."""
    return reason.strip().rstrip(".") + "."


def _revoke_detail(broker_url: str, revoked: Revoked) -> str:
    if revoked.status == 401:
        return f"foreverbull at {broker_url} didn't accept the token"
    if revoked.status == 429:
        return f"foreverbull at {broker_url} is turning away too many requests right now"
    return f"foreverbull at {broker_url} answered HTTP {revoked.status}"


def login():
    """Sign in through the browser and save a token to ~/.foreverbull/token."""
    try:
        broker_url = resolve_broker_url()
        result = sign_in(broker_url, announce=lambda line: console.print(line, highlight=False))
    except (SignInError, InsecureTransportError, TokenFileError, ValueError) as exc:
        raise _fail(str(exc)) from None
    console.print(signed_in_line(result.login, result.scope, result.path, result.meta), highlight=False)


def logout(
    local: Annotated[
        bool, typer.Option("--local", help="Only delete ~/.foreverbull/token. The token stays valid on the broker.")
    ] = False,
    all_credentials: Annotated[
        bool,
        typer.Option("--all", help="Revoke every token and browser session of your account, then delete the file."),
    ] = False,
):
    """Revoke the saved token on the broker, then delete ~/.foreverbull/token."""
    if local and all_credentials:
        raise _fail("Use either --local or --all, not both.")
    path = token_path()
    try:
        token = read_token_file()
        meta = read_token_meta()
    except TokenFileError as exc:
        raise _fail(str(exc)) from None
    if not token:
        raise _fail(f"You're not signed in: {path} doesn't exist.")
    if local:
        delete_token_files()
        console.print(removed_local_line(path, meta), highlight=False)
        return
    try:
        broker_url = resolve_broker_url()
        if meta is not None and meta.broker.rstrip("/") != broker_url.rstrip("/"):
            raise _fail(other_broker_line(meta.broker, broker_url, path, meta))
        revoked = revoke(broker_url, token, all_credentials=all_credentials)
    except BrokerUnreachable as exc:
        raise _fail(revoke_failed_line(f"fbull couldn't reach foreverbull at {exc.broker_url}", path, meta)) from None
    except (SignInError, InsecureTransportError, ValueError) as exc:
        raise _fail(f"Couldn't revoke the token, so fbull kept {path}. {_sentence(str(exc))}") from None
    if revoked.ok:
        delete_token_files()
        if all_credentials:
            console.print(
                f"Signed out everywhere: every token and browser session of your account is revoked. Removed {path}.",
                highlight=False,
            )
            return
        console.print(f"Signed out and revoked the token. Removed {path}.", highlight=False)
        return
    if (
        revoked.status == 401
        and not all_credentials
        and meta is not None
        and revoked.error in ("sign_in_expired", "unauthorized")
    ):
        # The broker named in token.meta answered in its own JSON that it
        # already refuses this token, which is the state logout wants.
        delete_token_files()
        if revoked.error == "sign_in_expired":
            console.print(
                f"Your sign-in had already expired, so there was nothing to revoke. Removed {path}.", highlight=False
            )
        else:
            console.print(
                f"foreverbull no longer accepts this token, so there was nothing to revoke. Removed {path}.",
                highlight=False,
            )
        return
    raise _fail(revoke_failed_line(_revoke_detail(broker_url, revoked), path, meta))
