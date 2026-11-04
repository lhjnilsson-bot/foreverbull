import base64
import hashlib
import json
import logging
import re
import secrets
import socket
import time
import urllib.error
import urllib.parse
import urllib.request
import webbrowser

from collections.abc import Callable
from dataclasses import dataclass
from datetime import UTC
from datetime import datetime
from pathlib import Path

from foreverbull.broker.credentials import TokenMeta
from foreverbull.broker.credentials import check_plaintext_host
from foreverbull.broker.credentials import save_sign_in


# The browser has five minutes to come back to the listener.
CALLBACK_TIMEOUT_S = 300.0
# One callback request line and headers fit well under this.
CALLBACK_REQUEST_CAP_BYTES = 16 << 10
# A connected browser has this long to send its request.
CALLBACK_READ_TIMEOUT_S = 10.0
# Bound on every broker HTTP call and on the bytes read from its answer.
HTTP_TIMEOUT_S = 30.0
HTTP_RESPONSE_CAP_BYTES = 64 << 10
# A connection that sends nothing (a browser preconnect) is not the request.
# This many empty connections end the wait.
CALLBACK_EMPTY_CONNECTION_CAP = 16
CLIENT_HOST_MAX = 60
LIFETIME = "or until it goes unused for 14 days, whichever comes first"

_TOKEN_SAVE_HINT = (
    "Create a token in the web UI and save it with `fbull token save`, or run `fbull login` "
    "on a computer with a browser."
)


_ADDRESS_STEP = "Set FOREVERBULL_BROKER_URL or remote.broker_url to an https:// address, then run `fbull login` again."
_LOGIN_AGAIN = "Run `fbull login` again."

log = logging.getLogger(__name__)


class SignInError(Exception):
    """A sign-in step failed. The message is for the user and never holds a code, verifier, or token."""


class BrokerUnreachable(SignInError):
    """The broker could not be reached. The library's reason goes to the log, not to the user."""

    def __init__(self, broker_url: str):
        super().__init__(
            f"Couldn't reach foreverbull at {broker_url}. Check the address. For a local install, run "
            "`fbenv status` to check that the environment is running, then run `fbull login` again."
        )
        self.broker_url = broker_url


@dataclass(frozen=True)
class SignIn:
    path: Path
    meta: TokenMeta
    scope: str
    login: str


def client_name() -> str:
    """cli@<host>, with the host cut to the characters and length the broker accepts."""
    host = re.sub(r"[^A-Za-z0-9._-]", "-", socket.gethostname()) or "unknown"
    return "cli@" + host[:CLIENT_HOST_MAX]


def check_broker_url(url: str) -> urllib.parse.SplitResult:
    """https is required, except plain http to 127.0.0.1 or the Kind host."""
    parsed = urllib.parse.urlsplit(url)
    if not parsed.hostname:
        raise SignInError(f"foreverbull's address {url} has no host name. {_ADDRESS_STEP}")
    if parsed.scheme == "https":
        return parsed
    if parsed.scheme == "http":
        check_plaintext_host(parsed.hostname)
        return parsed
    raise SignInError(f"foreverbull's address {url} doesn't start with https://. {_ADDRESS_STEP}")


def display_time(value: str) -> str:
    """An RFC 3339 time as 'YYYY-MM-DD HH:MM UTC'."""
    return _parse_time(value).strftime("%Y-%m-%d %H:%M UTC")


def _parse_time(value: str) -> datetime:
    # Go writes up to nine fraction digits; fromisoformat takes six.
    trimmed = re.sub(r"\.\d+", "", value.strip())
    parsed = datetime.fromisoformat(trimmed)
    if parsed.tzinfo is None:
        raise ValueError(f"time has no zone: {value}")
    return parsed.astimezone(UTC)


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    """Refuse every redirect, so the Authorization header never goes to another address."""

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


_OPENER = urllib.request.build_opener(_NoRedirect)


def request_json(
    broker_url: str, path: str, *, method: str, body: dict | None = None, bearer: str | None = None
) -> tuple[int, dict]:
    """One broker call. Returns the status and the JSON object. A connection failure raises BrokerUnreachable."""
    url = broker_url + path
    data = None if body is None else json.dumps(body).encode("utf-8")
    request = urllib.request.Request(url, data=data, method=method)
    request.add_header("Accept", "application/json")
    if data is not None:
        request.add_header("Content-Type", "application/json")
    if bearer is not None:
        request.add_header("Authorization", f"Bearer {bearer}")
    try:
        with _OPENER.open(request, timeout=HTTP_TIMEOUT_S) as response:
            return response.status, _json_body(response.headers, response.read(HTTP_RESPONSE_CAP_BYTES + 1))
    except urllib.error.HTTPError as exc:
        if 300 <= exc.code < 400:
            log.info("broker request %s %s redirected (%s) to %s", method, path, exc.code, exc.headers.get("Location"))
            raise SignInError(
                f"foreverbull at {broker_url} answered with a redirect, so fbull stopped. "
                "Set FOREVERBULL_BROKER_URL or remote.broker_url to the address foreverbull answers on."
            ) from None
        return exc.code, _json_body(exc.headers, exc.read(HTTP_RESPONSE_CAP_BYTES + 1))
    except (urllib.error.URLError, OSError) as exc:
        log.info("broker request %s %s failed: %s", method, path, getattr(exc, "reason", exc))
        raise BrokerUnreachable(broker_url) from None


def _json_body(headers, raw: bytes) -> dict:
    """The broker's JSON object. A body that isn't JSON, such as a proxy's error page, reads as {}."""
    if headers is None or headers.get_content_type() != "application/json":
        return {}
    if len(raw) > HTTP_RESPONSE_CAP_BYTES:
        return {}
    try:
        decoded = json.loads(raw.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError):
        return {}
    return decoded if isinstance(decoded, dict) else {}


def _pkce() -> tuple[str, str]:
    verifier = secrets.token_urlsafe(48)
    digest = hashlib.sha256(verifier.encode("ascii")).digest()
    return verifier, base64.urlsafe_b64encode(digest).decode("ascii").rstrip("=")


def check_auth_enabled(broker_url: str) -> None:
    status, _ = request_json(broker_url, "/api/auth/providers", method="GET")
    if status == 200:
        return
    if status == 404:
        raise SignInError(
            f"Sign-in is turned off on foreverbull at {broker_url}, so there's nothing to sign in to. "
            "Run fbull commands without signing in, or, if you run that broker, set AUTH_ENABLED=true."
        )
    raise SignInError(
        f"foreverbull at {broker_url} answered HTTP {status} when fbull asked how to sign in. "
        "Run `fbenv status` to check that the environment is running, then run `fbull login` again."
    )


def sign_in(
    broker_url: str,
    *,
    open_browser: Callable[[str], bool] | None = None,
    announce: Callable[[str], None] = lambda _line: None,
    home: Path | None = None,
    timeout_s: float = CALLBACK_TIMEOUT_S,
) -> SignIn:
    """Run the browser sign-in and save the token. Every failure raises SignInError and saves nothing."""
    broker_url = broker_url.rstrip("/")
    check_broker_url(broker_url)
    check_auth_enabled(broker_url)
    client = client_name()
    state = secrets.token_urlsafe(32)
    verifier, challenge = _pkce()
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as listener:
        listener.bind(("127.0.0.1", 0))
        listener.listen(1)
        redirect_uri = f"http://127.0.0.1:{listener.getsockname()[1]}/callback"
        query = urllib.parse.urlencode(
            {
                "client": client,
                "redirect_uri": redirect_uri,
                "state": state,
                "code_challenge": challenge,
                "code_challenge_method": "S256",
                "scope": "write",
            }
        )
        authorize = f"{broker_url}/cli/authorize?{query}"
        opener = webbrowser.open if open_browser is None else open_browser
        if not opener(authorize):
            raise SignInError(f"fbull couldn't open a browser. {_TOKEN_SAVE_HINT}")
        announce(f"Opened {broker_url}/cli/authorize in your browser. Approve the sign-in there.")
        params = wait_for_callback(listener, time.monotonic() + timeout_s)
    code = _callback_code(params, state)
    issued = exchange_code(broker_url, code, verifier, redirect_uri)
    meta = TokenMeta(expires_at=issued["expires_at"], client=issued["client"], broker=broker_url)
    path = save_sign_in(issued["token"], meta, home=home)
    return SignIn(path=path, meta=meta, scope=issued["scope"], login=issued["login"])


def _callback_code(params: dict[str, str], state: str) -> str:
    if not secrets.compare_digest(params.get("state", ""), state):
        raise SignInError(
            f"The browser sent back a different sign-in than the one fbull started, so fbull stopped. {_LOGIN_AGAIN}"
        )
    if params.get("error") == "access_denied":
        raise SignInError("The sign-in was denied in the browser. If that was a mistake, run `fbull login` again.")
    code = params.get("code", "")
    if not code:
        raise SignInError(f"The browser came back without a sign-in code. {_LOGIN_AGAIN}")
    return code


def exchange_code(broker_url: str, code: str, verifier: str, redirect_uri: str) -> dict[str, str]:
    status, body = request_json(
        broker_url,
        "/api/auth/cli/token",
        method="POST",
        body={"code": code, "code_verifier": verifier, "redirect_uri": redirect_uri},
    )
    if status == 429:
        raise SignInError("Too many sign-in attempts. Wait 15 minutes, then run `fbull login` again.")
    if status == 403:
        raise SignInError(
            f"foreverbull at {broker_url} didn't let this account sign in. "
            "Ask whoever runs this foreverbull to approve your account, then run `fbull login` again."
        )
    if status != 200:
        raise SignInError(f"foreverbull didn't accept the sign-in code (HTTP {status}). {_LOGIN_AGAIN}")
    issued: dict[str, str] = {}
    for key in ("login", "token", "expires_at", "scope", "client"):
        value = body.get(key)
        if not isinstance(value, str) or not value:
            raise _unreadable_answer(broker_url, f"no {key}")
        issued[key] = value
    if not issued["token"].startswith("fb_"):
        raise _unreadable_answer(broker_url, "not a foreverbull token")
    if issued["scope"] not in ("read", "write"):
        raise _unreadable_answer(broker_url, "unknown scope")
    try:
        _parse_time(issued["expires_at"])
    except ValueError:
        raise _unreadable_answer(broker_url, "unreadable expiry") from None
    return issued


_CALLBACK_DONE = (
    "<!doctype html><meta charset='utf-8'><title>fbull</title>"
    "<p>fbull has your answer. You can close this tab and go back to the terminal.</p>"
)


def wait_for_callback(listener: socket.socket, deadline: float) -> dict[str, str]:
    """Accept one request on the listener, answer it, and return its query.

    A connection that sends nothing is closed and does not count. The first
    request ends the wait, whatever its path.
    """
    for _ in range(CALLBACK_EMPTY_CONNECTION_CAP):
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            break
        listener.settimeout(remaining)
        try:
            connection, _address = listener.accept()
        except TimeoutError:
            break
        with connection:
            request = _read_request(connection)
            if not request:
                continue
            target = _request_target(request)
            connection.sendall(
                b"HTTP/1.1 200 OK\r\nContent-Type: text/html; charset=utf-8\r\n"
                b"Cache-Control: no-store\r\nReferrer-Policy: no-referrer\r\nConnection: close\r\n\r\n"
                + _CALLBACK_DONE.encode("utf-8")
            )
        parsed = urllib.parse.urlsplit(target)
        if parsed.path != "/callback":
            raise SignInError(f"The browser came back to an unexpected address, so fbull stopped. {_LOGIN_AGAIN}")
        return {key: values[0] for key, values in urllib.parse.parse_qs(parsed.query).items()}
    raise SignInError(f"The browser didn't answer within {int(CALLBACK_TIMEOUT_S // 60)} minutes. {_LOGIN_AGAIN}")


def _read_request(connection: socket.socket) -> bytes:
    connection.settimeout(CALLBACK_READ_TIMEOUT_S)
    received = b""
    while b"\r\n\r\n" not in received and len(received) < CALLBACK_REQUEST_CAP_BYTES:
        try:
            chunk = connection.recv(4096)
        except TimeoutError:
            break
        if not chunk:
            break
        received += chunk
    return received


def _request_target(request: bytes) -> str:
    line = request.split(b"\r\n", 1)[0].decode("latin-1")
    parts = line.split(" ")
    if len(parts) != 3 or parts[0] != "GET":
        raise SignInError(f"The browser sent an unexpected request, so fbull stopped. {_LOGIN_AGAIN}")
    return parts[1]


def _unreadable_answer(broker_url: str, what: str) -> SignInError:
    return SignInError(
        f"foreverbull at {broker_url} sent a sign-in answer fbull can't read ({what}). "
        "Check that fbull and foreverbull are the same version, then run `fbull login` again."
    )


@dataclass(frozen=True)
class Revoked:
    ok: bool
    status: int
    error: str


def revoke(broker_url: str, token: str, *, all_credentials: bool) -> Revoked:
    """Ask the broker to revoke the token, or every token and session of its account."""
    broker_url = broker_url.rstrip("/")
    check_broker_url(broker_url)
    status, body = request_json(
        broker_url, "/api/auth/cli/revoke", method="POST", body={"all": all_credentials}, bearer=token
    )
    error = body.get("error")
    if status != 200:
        log.info("revoke answered %s: %s", status, body.get("message"))
    return Revoked(ok=status == 200, status=status, error=error if isinstance(error, str) else "")


def valid_until(meta: TokenMeta) -> str:
    return f"valid until {display_time(meta.expires_at)}, {LIFETIME}"
