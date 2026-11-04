import base64
import hashlib
import json
import logging
import socket
import stat
import threading
import time
import urllib.parse
import urllib.request

from http.server import BaseHTTPRequestHandler
from http.server import ThreadingHTTPServer

import pytest

from foreverbull_cli import output
from foreverbull_cli.bull import login as login_module
from foreverbull_cli.bull.auth import other_broker_line
from foreverbull_cli.bull.cli import TOKEN_REFUSED
from foreverbull_cli.bull.cli import cli
from foreverbull_cli.bull.cli import unauthenticated_line
from foreverbull_cli.bull.login import SignInError
from foreverbull_cli.bull.login import exchange_code
from foreverbull_cli.bull.login import sign_in
from foreverbull_cli.bull.login import wait_for_callback
from typer.testing import CliRunner

from foreverbull.broker.credentials import InsecureTransportError
from foreverbull.broker.credentials import TokenMeta
from foreverbull.broker.credentials import save_sign_in
from foreverbull.broker.credentials import set_cli_token
from foreverbull.broker.credentials import token_meta_path
from foreverbull.broker.credentials import token_path


TOKEN = "fb_" + "t" * 43
CODE = "c" * 43
EXPIRES = "2026-12-04T06:00:00.123456789Z"
LIFETIME = "valid until 2026-12-04 06:00 UTC, or until it goes unused for 14 days, whichever comes first"


class FakeBroker:
    """The broker's sign-in routes. It records every request body and header it got."""

    def __init__(self):
        self.auth_enabled = True
        self.revoke_status = 200
        self.revoke_body: dict = {"ok": True}
        self.exchange_status = 200
        # (status, content type, body, extra headers) sent instead of revoke_body.
        self.revoke_raw: tuple[int, str, bytes, dict[str, str]] | None = None
        self.challenge = ""
        self.requests: list[tuple[str, str, dict, dict]] = []
        broker = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, format, *args):
                logging.getLogger("fake-broker").debug(format, *args)

            def _send(self, status: int, body: dict):
                raw = json.dumps(body).encode()
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

            def do_GET(self):
                broker.requests.append(("GET", self.path, {}, dict(self.headers)))
                if self.path == "/api/auth/providers" and broker.auth_enabled:
                    self._send(200, {"password": True, "github": False})
                    return
                self._send(404, {"error": "not found"})

            def do_POST(self):
                length = int(self.headers.get("Content-Length") or 0)
                body = json.loads(self.rfile.read(length) or b"{}")
                broker.requests.append(("POST", self.path, body, dict(self.headers)))
                if self.path == "/api/auth/cli/token":
                    self._send(*broker.exchange(body))
                    return
                if self.path == "/api/auth/cli/revoke" and broker.revoke_raw is not None:
                    status, content_type, raw, headers = broker.revoke_raw
                    self.send_response(status)
                    self.send_header("Content-Type", content_type)
                    self.send_header("Content-Length", str(len(raw)))
                    for name, value in headers.items():
                        self.send_header(name, value)
                    self.end_headers()
                    self.wfile.write(raw)
                    return
                if self.path == "/api/auth/cli/revoke":
                    self._send(broker.revoke_status, broker.revoke_body)
                    return
                self._send(404, {"error": "not found"})

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.url = f"http://127.0.0.1:{self.server.server_address[1]}"
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)

    def exchange(self, body: dict) -> tuple[int, dict]:
        digest = hashlib.sha256(body["code_verifier"].encode()).digest()
        if base64.urlsafe_b64encode(digest).decode().rstrip("=") != self.challenge or body["code"] != CODE:
            return 400, {"error": "invalid_grant"}
        if self.exchange_status != 200:
            return self.exchange_status, {"error": "refused"}
        return 200, {"login": "henrik", "token": TOKEN, "scope": "write", "client": "cli@laptop", "expires_at": EXPIRES}

    def browser(self, *, state_override: str | None = None, deny: bool = False):
        """A browser that approves and follows the redirect to the CLI listener."""

        def open_browser(url: str) -> bool:
            query = dict(urllib.parse.parse_qsl(urllib.parse.urlsplit(url).query))
            self.challenge = query["code_challenge"]
            callback = {"state": state_override or query["state"]}
            if deny:
                callback["error"] = "access_denied"
            else:
                callback["code"] = CODE

            def follow():
                urllib.request.urlopen(query["redirect_uri"] + "?" + urllib.parse.urlencode(callback), timeout=5).read()

            threading.Thread(target=follow, daemon=True).start()
            return True

        return open_browser


@pytest.fixture
def broker():
    fake = FakeBroker()
    fake.thread.start()
    yield fake
    fake.server.shutdown()
    fake.server.server_close()


@pytest.fixture
def home(tmp_path, monkeypatch):
    monkeypatch.setenv("HOME", str(tmp_path))
    monkeypatch.delenv("FOREVERBULL_TOKEN", raising=False)
    set_cli_token(None)
    # One line per message, so the tests can compare whole lines.
    monkeypatch.setattr(output.console, "_width", 10_000)
    monkeypatch.setattr(output.console, "_height", 50)
    return tmp_path


def lines(result) -> list[str]:
    return [line.rstrip() for line in result.output.splitlines() if line.strip()]


def test_full_sign_in_and_logout_log_no_secret(broker, home, monkeypatch, caplog):
    caplog.set_level(logging.DEBUG)
    monkeypatch.setenv("FOREVERBULL_BROKER_URL", broker.url)
    verifiers: list[str] = []
    real_pkce = login_module._pkce

    def recording_pkce():
        verifier, challenge = real_pkce()
        verifiers.append(verifier)
        return verifier, challenge

    monkeypatch.setattr(login_module, "_pkce", recording_pkce)
    monkeypatch.setattr(login_module.webbrowser, "open", broker.browser())

    runner = CliRunner()
    signed_in = runner.invoke(cli, ["-vvv", "login"])
    assert signed_in.exit_code == 0, signed_in.output
    path = token_path(home)
    assert path.read_text() == TOKEN
    assert stat.S_IMODE(path.stat().st_mode) == 0o600
    assert stat.S_IMODE(path.parent.stat().st_mode) == 0o700
    meta = json.loads(token_meta_path(home).read_text())
    assert meta == {"expires_at": EXPIRES, "client": "cli@laptop", "broker": broker.url}
    assert lines(signed_in) == [
        f"Opened {broker.url}/cli/authorize in your browser. Approve the sign-in there.",
        f"Signed in as henrik with write access. Token saved to {path}. It's {LIFETIME}.",
    ]

    exchange = [r for r in broker.requests if r[1] == "/api/auth/cli/token"]
    assert len(exchange) == 1
    assert exchange[0][2]["redirect_uri"].startswith("http://127.0.0.1:")
    assert all(TOKEN not in r[1] and CODE not in r[1] for r in broker.requests), "no secret in a URL"

    signed_out = runner.invoke(cli, ["-vvv", "logout"])
    assert signed_out.exit_code == 0, signed_out.output
    assert not path.exists()
    assert not token_meta_path(home).exists()
    revoke = [r for r in broker.requests if r[1] == "/api/auth/cli/revoke"]
    assert revoke[0][3]["Authorization"] == f"Bearer {TOKEN}"
    assert revoke[0][2] == {"all": False}
    assert lines(signed_out) == [f"Signed out and revoked the token. Removed {path}."]

    logs = caplog.text + signed_in.output + signed_out.output
    assert verifiers
    for secret in (TOKEN, CODE, *verifiers):
        assert secret not in logs


def test_wrong_state_aborts_and_saves_nothing(broker, home):
    with pytest.raises(SignInError) as raised:
        sign_in(broker.url, open_browser=broker.browser(state_override="x" * 43), home=home)
    assert str(raised.value) == (
        "The browser sent back a different sign-in than the one fbull started, so fbull stopped. "
        "Run `fbull login` again."
    )
    assert not token_path(home).exists()
    assert not [r for r in broker.requests if r[1] == "/api/auth/cli/token"]


def test_denied_sign_in_saves_nothing(broker, home):
    with pytest.raises(SignInError) as raised:
        sign_in(broker.url, open_browser=broker.browser(deny=True), home=home)
    assert str(raised.value) == "The sign-in was denied in the browser. If that was a mistake, run `fbull login` again."
    assert not token_path(home).exists()


def test_no_browser_is_a_hard_error_naming_token_save(broker, home):
    with pytest.raises(SignInError) as raised:
        sign_in(broker.url, open_browser=lambda _url: False, home=home)
    assert str(raised.value) == (
        "fbull couldn't open a browser. Create a token in the web UI and save it with `fbull token save`, "
        "or run `fbull login` on a computer with a browser."
    )
    assert not token_path(home).exists()


def test_auth_off_says_so(broker, home):
    broker.auth_enabled = False
    with pytest.raises(SignInError) as raised:
        sign_in(broker.url, open_browser=broker.browser(), home=home)
    assert str(raised.value) == (
        f"Sign-in is turned off on foreverbull at {broker.url}, so there's nothing to sign in to. "
        "Run fbull commands without signing in, or, if you run that broker, set AUTH_ENABLED=true."
    )


def test_plain_http_to_a_remote_broker_is_refused_before_any_request(home):
    opened: list[str] = []
    with pytest.raises(InsecureTransportError) as raised:
        sign_in("http://fb.example.com", open_browser=lambda url: opened.append(url) or True, home=home)
    assert str(raised.value) == (
        "fbull won't send your token to fb.example.com without encryption. "
        "Use an https:// address, or, for a local install, 127.0.0.1 or localhost."
    )
    assert opened == []


def test_listener_times_out(home):
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        listener.listen(1)
        started = time.monotonic()
        with pytest.raises(SignInError) as raised:
            wait_for_callback(listener, time.monotonic() + 0.2)
        assert str(raised.value) == "The browser didn't answer within 5 minutes. Run `fbull login` again."
        assert time.monotonic() - started < 5


def test_listener_takes_one_request_and_closes(broker, home):
    redirects: list[str] = []
    approve = broker.browser()

    def open_browser(url: str) -> bool:
        redirects.append(dict(urllib.parse.parse_qsl(urllib.parse.urlsplit(url).query))["redirect_uri"])
        return approve(url)

    sign_in(broker.url, open_browser=open_browser, home=home)
    with pytest.raises(OSError):
        urllib.request.urlopen(redirects[0] + "?state=x&code=y", timeout=2)


def test_an_unexpected_path_ends_the_wait(home):
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        listener.listen(1)
        port = listener.getsockname()[1]

        def visit():
            try:
                urllib.request.urlopen(f"http://127.0.0.1:{port}/favicon.ico", timeout=5).read()
            except OSError:
                pass

        threading.Thread(target=visit, daemon=True).start()
        with pytest.raises(SignInError) as raised:
            wait_for_callback(listener, time.monotonic() + 5)
        assert str(raised.value) == (
            "The browser came back to an unexpected address, so fbull stopped. Run `fbull login` again."
        )


def _signed_in(home, broker_url: str):
    save_sign_in(TOKEN, TokenMeta(expires_at=EXPIRES, client="cli@laptop", broker=broker_url), home=home)


def test_logout_keeps_the_file_when_the_broker_refuses(broker, home, monkeypatch):
    monkeypatch.setenv("FOREVERBULL_BROKER_URL", broker.url)
    _signed_in(home, broker.url)
    broker.revoke_status = 500
    broker.revoke_body = {"error": "internal"}
    result = CliRunner().invoke(cli, ["logout"])
    assert result.exit_code == 1
    assert token_path(home).read_text() == TOKEN
    assert lines(result) == [
        (
            f"Couldn't revoke the token: foreverbull at {broker.url} answered HTTP 500. "
            f"Kept {token_path(home)}, and the token is still {LIFETIME}. "
            "Run `fbenv status` to check that the environment is running, then run `fbull logout` again."
        )
    ]


def test_logout_keeps_the_file_when_the_broker_is_unreachable(home, monkeypatch):
    with socket.socket() as unused:
        unused.bind(("127.0.0.1", 0))
        url = f"http://127.0.0.1:{unused.getsockname()[1]}"
    monkeypatch.setenv("FOREVERBULL_BROKER_URL", url)
    _signed_in(home, url)
    result = CliRunner().invoke(cli, ["logout"])
    assert result.exit_code == 1
    assert token_path(home).exists()
    assert lines(result) == [
        (
            f"Couldn't revoke the token: fbull couldn't reach foreverbull at {url}. "
            f"Kept {token_path(home)}, and the token is still {LIFETIME}. "
            "Run `fbenv status` to check that the environment is running, then run `fbull logout` again."
        )
    ]


def test_logout_local_deletes_only_the_file(broker, home, monkeypatch):
    monkeypatch.setenv("FOREVERBULL_BROKER_URL", broker.url)
    _signed_in(home, broker.url)
    result = CliRunner().invoke(cli, ["logout", "--local"])
    assert result.exit_code == 0, result.output
    assert not token_path(home).exists()
    assert not token_meta_path(home).exists()
    assert not [r for r in broker.requests if r[1] == "/api/auth/cli/revoke"]
    assert lines(result) == [
        (
            f"Removed {token_path(home)}. The token itself is still {LIFETIME}. "
            "To revoke it now, use Sign out everywhere on your account page in the web UI."
        )
    ]


def test_logout_all_asks_the_broker_for_everything(broker, home, monkeypatch):
    monkeypatch.setenv("FOREVERBULL_BROKER_URL", broker.url)
    _signed_in(home, broker.url)
    result = CliRunner().invoke(cli, ["logout", "--all"])
    assert result.exit_code == 0, result.output
    assert [r[2] for r in broker.requests if r[1] == "/api/auth/cli/revoke"] == [{"all": True}]
    assert not token_path(home).exists()
    assert lines(result) == [
        (
            "Signed out everywhere: every token and browser session of your account is revoked. "
            f"Removed {token_path(home)}."
        )
    ]


def test_logout_local_and_all_together_is_refused(home):
    result = CliRunner().invoke(cli, ["logout", "--local", "--all"])
    assert result.exit_code == 1
    assert lines(result) == ["Use either --local or --all, not both."]


def test_logout_of_an_expired_sign_in_removes_the_file(broker, home, monkeypatch):
    monkeypatch.setenv("FOREVERBULL_BROKER_URL", broker.url)
    _signed_in(home, broker.url)
    broker.revoke_status = 401
    broker.revoke_body = {
        "error": "sign_in_expired",
        "message": "Your sign-in has expired. Run `fbull login` to sign in again.",
    }
    result = CliRunner().invoke(cli, ["logout"])
    assert result.exit_code == 0, result.output
    assert not token_path(home).exists()
    assert lines(result) == [
        f"Your sign-in had already expired, so there was nothing to revoke. Removed {token_path(home)}."
    ]


def test_logout_refuses_a_token_from_another_broker(broker, home, monkeypatch):
    monkeypatch.setenv("FOREVERBULL_BROKER_URL", broker.url)
    _signed_in(home, "https://other.example.com")
    result = CliRunner().invoke(cli, ["logout"])
    assert result.exit_code == 1
    assert token_path(home).exists()
    assert not [r for r in broker.requests if r[1] == "/api/auth/cli/revoke"]
    assert lines(result) == [
        (
            "Couldn't revoke the token: it was made by foreverbull at https://other.example.com, but fbull is set to "
            f"use foreverbull at {broker.url}. Kept {token_path(home)}, and the token is still {LIFETIME}. "
            "To revoke it, set FOREVERBULL_BROKER_URL to https://other.example.com and run `fbull logout` again, "
            "or run `fbull logout --local` to only remove the file."
        )
    ]


def test_logout_without_a_token_file_fails(home):
    result = CliRunner().invoke(cli, ["logout"])
    assert result.exit_code == 1
    assert lines(result) == [f"You're not signed in: {token_path(home)} doesn't exist."]


def test_logout_of_a_token_the_broker_no_longer_accepts_removes_the_file(broker, home, monkeypatch):
    monkeypatch.setenv("FOREVERBULL_BROKER_URL", broker.url)
    _signed_in(home, broker.url)
    broker.revoke_status = 401
    broker.revoke_body = {"error": "unauthorized"}
    result = CliRunner().invoke(cli, ["logout"])
    assert result.exit_code == 0, result.output
    assert lines(result) == [
        f"foreverbull no longer accepts this token, so there was nothing to revoke. Removed {token_path(home)}."
    ]


def test_logout_of_a_saved_token_names_the_web_ui(broker, home, monkeypatch, caplog):
    caplog.set_level(logging.INFO)
    monkeypatch.setenv("FOREVERBULL_BROKER_URL", broker.url)
    path = token_path(home)
    path.parent.mkdir(mode=0o700, parents=True)
    path.write_text(TOKEN)
    path.chmod(0o600)
    broker.revoke_status = 500
    broker.revoke_body = {"error": "internal", "message": "pq: connection refused"}
    result = CliRunner().invoke(cli, ["logout"])
    assert result.exit_code == 1
    assert lines(result) == [
        (
            f"Couldn't revoke the token: foreverbull at {broker.url} answered HTTP 500. "
            f"Kept {path}, and the token stays valid until it's revoked in the web UI. "
            "Run `fbenv status` to check that the environment is running, then run `fbull logout` again."
        )
    ]
    assert "pq: connection refused" in caplog.text

    local = CliRunner().invoke(cli, ["logout", "--local"])
    assert local.exit_code == 0, local.output
    assert lines(local) == [
        (
            f"Removed {path}. The token itself stays valid until it's revoked in the web UI. "
            "To revoke it now, use Sign out everywhere on your account page in the web UI."
        )
    ]


@pytest.mark.parametrize(
    ("status", "message"),
    [
        (429, "Too many sign-in attempts. Wait 15 minutes, then run `fbull login` again."),
        (400, "foreverbull didn't accept the sign-in code (HTTP 400). Run `fbull login` again."),
        (
            403,
            (
                "foreverbull at {url} didn't let this account sign in. "
                "Ask whoever runs this foreverbull to approve your account, then run `fbull login` again."
            ),
        ),
    ],
)
def test_a_refused_exchange_names_the_next_step(broker, status, message):
    broker.exchange_status = status
    verifier = "v" * 43
    broker.challenge = base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).decode().rstrip("=")
    with pytest.raises(SignInError) as raised:
        exchange_code(broker.url, CODE, verifier, "http://127.0.0.1:1/callback")
    assert str(raised.value) == message.format(url=broker.url)


def test_an_answer_without_a_login_is_refused(broker, monkeypatch):
    monkeypatch.setattr(broker, "exchange", lambda _body: (200, {"token": TOKEN}))
    with pytest.raises(SignInError) as raised:
        exchange_code(broker.url, CODE, "v" * 43, "http://127.0.0.1:1/callback")
    assert str(raised.value) == (
        f"foreverbull at {broker.url} sent a sign-in answer fbull can't read (no login). "
        "Check that fbull and foreverbull are the same version, then run `fbull login` again."
    )


def test_an_unreachable_broker_logs_the_reason(home, caplog):
    caplog.set_level(logging.INFO)
    with socket.socket() as unused:
        unused.bind(("127.0.0.1", 0))
        url = f"http://127.0.0.1:{unused.getsockname()[1]}"
    with pytest.raises(SignInError) as raised:
        sign_in(url, open_browser=lambda _url: True, home=home)
    assert str(raised.value) == (
        f"Couldn't reach foreverbull at {url}. Check the address. For a local install, run `fbenv status` "
        "to check that the environment is running, then run `fbull login` again."
    )
    assert "refused" in caplog.text.lower()


def test_a_remote_broker_must_use_https(home):
    with pytest.raises(SignInError) as raised:
        sign_in("ftp://fb.example.com", open_browser=lambda _url: True, home=home)
    assert str(raised.value) == (
        "foreverbull's address ftp://fb.example.com doesn't start with https://. "
        "Set FOREVERBULL_BROKER_URL or remote.broker_url to an https:// address, then run `fbull login` again."
    )


def test_a_broker_address_needs_a_host_name(home):
    with pytest.raises(SignInError) as raised:
        sign_in("https://:8443", open_browser=lambda _url: True, home=home)
    assert str(raised.value) == (
        "foreverbull's address https://:8443 has no host name. "
        "Set FOREVERBULL_BROKER_URL or remote.broker_url to an https:// address, then run `fbull login` again."
    )


def test_other_broker_line_for_a_saved_token():
    assert other_broker_line("https://a.example.com", "https://b.example.com", "/home/me/.foreverbull/token", None) == (
        "Couldn't revoke the token: it was made by foreverbull at https://a.example.com, but fbull is set to use "
        "foreverbull at https://b.example.com. Kept /home/me/.foreverbull/token, and the token stays valid until "
        "it's revoked in the web UI. To revoke it, set FOREVERBULL_BROKER_URL to https://a.example.com and run "
        "`fbull logout` again, or run `fbull logout --local` to only remove the file."
    )


def test_a_logout_reason_ends_with_one_period(home, monkeypatch, tmp_path):
    monkeypatch.delenv("FOREVERBULL_BROKER_URL", raising=False)
    monkeypatch.delenv("FOREVERBULL_CONFIG", raising=False)
    monkeypatch.chdir(tmp_path)
    config = tmp_path / ".foreverbull" / "config.yaml"
    config.parent.mkdir()
    config.write_text("cluster: remote\nremote:\n  broker_address: fb.example.com:50055\n")
    _signed_in(home, "https://fb.example.com")
    result = CliRunner().invoke(cli, ["logout"])
    assert result.exit_code == 1
    assert lines(result) == [
        (
            f"Couldn't revoke the token, so fbull kept {token_path(home)}. cluster is remote but remote.broker_url "
            "is not set; set it in .foreverbull/config.yaml or FOREVERBULL_BROKER_URL."
        )
    ]

    monkeypatch.setenv("FOREVERBULL_BROKER_URL", "http://fb.example.com")
    _signed_in(home, "http://fb.example.com")
    result = CliRunner().invoke(cli, ["logout"])
    assert result.exit_code == 1
    assert lines(result) == [
        (
            f"Couldn't revoke the token, so fbull kept {token_path(home)}. fbull won't send your token to fb.example.com "
            "without encryption. Use an https:// address, or, for a local install, 127.0.0.1 or localhost."
        )
    ]


def test_unauthenticated_lines():
    assert unauthenticated_line("Your sign-in has expired. Run `fbull login` to sign in again.") == (
        "Your sign-in has expired. Run `fbull login` to sign in again."
    )
    assert unauthenticated_line("unauthorized") == TOKEN_REFUSED
    assert TOKEN_REFUSED == (
        "foreverbull didn't accept your token. Run `fbull login` to get a new one. "
        "If you set FOREVERBULL_TOKEN or --token, check that it holds the right token."
    )


def _kept_line(home, broker_url: str, detail: str) -> list[str]:
    return [
        (
            f"Couldn't revoke the token: {detail}. Kept {token_path(home)}, and the token is still {LIFETIME}. "
            "Run `fbenv status` to check that the environment is running, then run `fbull logout` again."
        )
    ]


@pytest.mark.parametrize(
    ("content_type", "raw"),
    [
        ("text/html", b"<h1>401 Authorization Required</h1>"),
        ("application/json", b'{"error":"proxy_auth_required"}'),
        ("text/plain", b'{"error":"unauthorized"}'),
    ],
)
def test_logout_keeps_the_file_on_a_401_that_is_not_the_brokers_own(broker, home, monkeypatch, content_type, raw):
    monkeypatch.setenv("FOREVERBULL_BROKER_URL", broker.url)
    _signed_in(home, broker.url)
    broker.revoke_raw = (401, content_type, raw, {})
    result = CliRunner().invoke(cli, ["logout"])
    assert result.exit_code == 1
    assert token_path(home).read_text() == TOKEN
    assert token_meta_path(home).exists()
    assert lines(result) == _kept_line(home, broker.url, f"foreverbull at {broker.url} didn't accept the token")


def test_logout_keeps_a_saved_token_on_a_401(broker, home, monkeypatch):
    monkeypatch.setenv("FOREVERBULL_BROKER_URL", broker.url)
    path = token_path(home)
    path.parent.mkdir(mode=0o700, parents=True)
    path.write_text(TOKEN)
    path.chmod(0o600)
    broker.revoke_status = 401
    broker.revoke_body = {"error": "unauthorized"}
    result = CliRunner().invoke(cli, ["logout"])
    assert result.exit_code == 1
    assert path.read_text() == TOKEN
    assert lines(result) == [
        (
            f"Couldn't revoke the token: foreverbull at {broker.url} didn't accept the token. "
            f"Kept {path}, and the token stays valid until it's revoked in the web UI. "
            "Run `fbenv status` to check that the environment is running, then run `fbull logout` again."
        )
    ]


def test_logout_all_keeps_the_file_on_a_401(broker, home, monkeypatch):
    monkeypatch.setenv("FOREVERBULL_BROKER_URL", broker.url)
    _signed_in(home, broker.url)
    broker.revoke_status = 401
    broker.revoke_body = {"error": "unauthorized"}
    result = CliRunner().invoke(cli, ["logout", "--all"])
    assert result.exit_code == 1
    assert token_path(home).read_text() == TOKEN
    assert lines(result) == _kept_line(home, broker.url, f"foreverbull at {broker.url} didn't accept the token")


def test_logout_does_not_follow_a_redirect(broker, home, monkeypatch, caplog):
    caplog.set_level(logging.INFO)
    other = FakeBroker()
    other.thread.start()
    try:
        monkeypatch.setenv("FOREVERBULL_BROKER_URL", broker.url)
        _signed_in(home, broker.url)
        broker.revoke_raw = (307, "text/plain", b"", {"Location": f"{other.url}/api/auth/cli/revoke"})
        result = CliRunner().invoke(cli, ["logout"])
    finally:
        other.server.shutdown()
        other.server.server_close()
    assert result.exit_code == 1
    assert other.requests == []
    assert token_path(home).read_text() == TOKEN
    assert lines(result) == [
        (
            f"Couldn't revoke the token, so fbull kept {token_path(home)}. "
            f"foreverbull at {broker.url} answered with a redirect, so fbull stopped. "
            "Set FOREVERBULL_BROKER_URL or remote.broker_url to the address foreverbull answers on."
        )
    ]
    assert other.url in caplog.text
