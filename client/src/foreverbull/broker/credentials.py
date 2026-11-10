import json
import os
import re
import stat
import tempfile

from collections.abc import Mapping
from contextvars import ContextVar
from dataclasses import dataclass
from pathlib import Path

import grpc


TOKEN_ENV = "FOREVERBULL_TOKEN"
SESSION_METADATA = "x-foreverbull-session"
_AUTHORIZATION = "authorization"
_cli_token: ContextVar[str] = ContextVar("foreverbull_cli_token", default="")


# Plaintext gRPC is only for these hosts. "localhost" is the Kind host port
# that `fbenv` publishes. Any other host is remote and uses TLS, token or not.
PLAINTEXT_HOSTS = frozenset({"127.0.0.1", "::1", "localhost"})
# PEM trust anchor for a private CA. Unset uses the system trust store.
CA_ENV = "FOREVERBULL_BROKER_CA"


class TokenFileError(Exception):
    """A token file the process must not use. The message is a path, never the secret."""


class InsecureTransportError(Exception):
    """A token would cross a plaintext connection to a host that is not loopback or Kind."""


@dataclass(frozen=True)
class TokenMeta:
    """What `fbull login` wrote next to the token. It never holds the secret."""

    expires_at: str
    client: str
    broker: str


class _CallDetails(grpc.ClientCallDetails):
    def __init__(self, method, timeout, metadata, credentials, wait_for_ready, compression):
        self.method = method
        self.timeout = timeout
        self.metadata = metadata
        self.credentials = credentials
        self.wait_for_ready = wait_for_ready
        self.compression = compression


class _MetadataInterceptor(
    grpc.UnaryUnaryClientInterceptor,
    grpc.UnaryStreamClientInterceptor,
    grpc.StreamUnaryClientInterceptor,
    grpc.StreamStreamClientInterceptor,
):
    def __init__(self, pairs: tuple[tuple[str, str], ...]) -> None:
        self._pairs = pairs

    def _details(self, client_call_details):
        metadata: list[tuple[str, str | bytes]] = []
        if client_call_details.metadata is not None:
            metadata.extend(client_call_details.metadata)
        metadata.extend(self._pairs)
        return _CallDetails(
            client_call_details.method,
            client_call_details.timeout,
            metadata,
            client_call_details.credentials,
            getattr(client_call_details, "wait_for_ready", None),
            getattr(client_call_details, "compression", None),
        )

    def intercept_unary_unary(self, continuation, client_call_details, request):
        return continuation(self._details(client_call_details), request)

    def intercept_unary_stream(self, continuation, client_call_details, request):
        return continuation(self._details(client_call_details), request)

    def intercept_stream_unary(self, continuation, client_call_details, request_iterator):
        return continuation(self._details(client_call_details), request_iterator)

    def intercept_stream_stream(self, continuation, client_call_details, request_iterator):
        return continuation(self._details(client_call_details), request_iterator)


def set_cli_token(value: str | None) -> None:
    """Remember --token for this process. None clears it. The value is not written to disk."""
    if value is None:
        _cli_token.set("")
        return
    _cli_token.set(value.strip())


def token_path(home: Path | None = None) -> Path:
    home = Path.home() if home is None else home
    return home / ".foreverbull" / "token"


def token_meta_path(home: Path | None = None) -> Path:
    return token_path(home).with_name("token.meta")


def resolve_token(
    *,
    environ: Mapping[str, str] | None = None,
    home: Path | None = None,
) -> str:
    """First match wins: --token, FOREVERBULL_TOKEN, then ~/.foreverbull/token. Empty means no credential."""
    environ = os.environ if environ is None else environ
    home = Path.home() if home is None else home
    cli_token = _cli_token.get().strip()
    if cli_token:
        return cli_token
    env_token = str(environ.get(TOKEN_ENV) or "").strip()
    if env_token:
        return env_token
    return read_token_file(home)


def read_token_file(home: Path | None = None) -> str:
    """The token in ~/.foreverbull/token, or empty when there is no file.

    The directory must be 0700 and the file 0600, both owned by this user.
    """
    path = token_path(home)
    if not path.exists():
        return ""
    _check_private(path.parent, 0o700)
    _check_private(path, 0o600)
    if not path.is_file():
        raise TokenFileError(_refused(path, "isn't a regular file", _REMOVE_STEP))
    try:
        text = path.read_text(encoding="utf-8")
    except UnicodeDecodeError:
        raise TokenFileError(_refused(path, "can't be read", _REMOVE_STEP)) from None
    return text.strip()


def read_token_meta(home: Path | None = None) -> TokenMeta | None:
    """The sign-in details `fbull login` saved, or None for a token from `fbull token save`."""
    path = token_meta_path(home)
    if not path.exists():
        return None
    _check_private(path.parent, 0o700)
    _check_private(path, 0o600)
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError):
        raise TokenFileError(_refused(path, "can't be read", _REMOVE_STEP)) from None
    if not isinstance(data, dict):
        raise TokenFileError(_refused(path, "can't be read", _REMOVE_STEP))
    fields = {}
    for key in ("expires_at", "client", "broker"):
        value = data.get(key)
        if not isinstance(value, str) or not value:
            raise TokenFileError(_refused(path, f"has no {key}", _REMOVE_STEP))
        fields[key] = value
    return TokenMeta(**fields)


def save_token_file(raw: str, *, force: bool, home: Path | None = None) -> Path:
    """Write ~/.foreverbull/token mode 0600 in a 0700 directory. The secret is not an argument and is not logged.

    A saved token has no sign-in details, so an old token.meta is removed.
    """
    token = raw.strip()
    if not token:
        raise TokenFileError("token is empty")
    path = token_path(home)
    if path.exists() and not force:
        raise TokenFileError(f"token file already exists: {path}")
    _private_dir(path.parent)
    _write_private(path, token)
    meta = token_meta_path(home)
    if meta.exists():
        meta.unlink()
    return path


def save_sign_in(raw: str, meta: TokenMeta, *, home: Path | None = None) -> Path:
    """Write the token and token.meta from `fbull login`, replacing any earlier token."""
    token = raw.strip()
    if not token:
        raise TokenFileError("token is empty")
    path = token_path(home)
    _private_dir(path.parent)
    # The meta goes first: a token without meta would read as a non-expiring saved token.
    _write_private(
        token_meta_path(home),
        json.dumps({"expires_at": meta.expires_at, "client": meta.client, "broker": meta.broker}),
    )
    _write_private(path, token)
    return path


def delete_token_files(home: Path | None = None) -> Path:
    """Remove the token and token.meta. Returns the token path."""
    path = token_path(home)
    for target in (path, token_meta_path(home)):
        if target.exists():
            target.unlink()
    return path


def _plaintext_host(host: str) -> bool:
    """True for loopback and the Kind host port. Everything else is remote."""
    return host.strip().strip("[]").lower() in PLAINTEXT_HOSTS


def check_plaintext_host(host: str) -> None:
    """Refuse to send a credential over a plaintext connection to a host that is not loopback or Kind."""
    if _plaintext_host(host):
        return
    raise InsecureTransportError(
        f"fbull won't send your token to {host} without encryption. "
        "Use an https:// address, or, for a local install, 127.0.0.1 or localhost."
    )


def tls_channel_credentials(host: str) -> grpc.ChannelCredentials:
    """Trust anchors for a remote broker. A set CA path that cannot be used fails the call."""
    raw = os.environ.get(CA_ENV, "").strip()
    if not raw:
        return grpc.ssl_channel_credentials()
    path = Path(raw)
    try:
        pem = path.read_bytes()
    except OSError:
        raise InsecureTransportError(
            f"fbull won't send your token to {host} without encryption. {CA_ENV} at {path} can't be read."
        ) from None
    if not pem.strip():
        raise InsecureTransportError(
            f"fbull won't send your token to {host} without encryption. {CA_ENV} at {path} is empty."
        )
    if b"-----BEGIN CERTIFICATE-----" not in pem:
        raise InsecureTransportError(
            f"fbull won't send your token to {host} without encryption. {CA_ENV} at {path} is not a PEM certificate."
        )
    try:
        return grpc.ssl_channel_credentials(root_certificates=pem)
    except Exception as exc:
        raise InsecureTransportError(
            f"fbull won't send your token to {host} without encryption. {CA_ENV} at {path} is not a PEM certificate."
        ) from exc


def _private_dir(directory: Path) -> None:
    """Create the token directory at 0700, or make an existing one 0700. Another owner is refused."""
    directory.mkdir(mode=0o700, parents=True, exist_ok=True)
    info = directory.lstat()
    if not stat.S_ISDIR(info.st_mode):
        raise TokenFileError(_refused(directory, "isn't a directory", _REMOVE_STEP))
    if info.st_uid != os.getuid():
        raise TokenFileError(_refused(directory, "belongs to another user", _REMOVE_STEP))
    if stat.S_IMODE(info.st_mode) != 0o700:
        os.chmod(directory, 0o700)


def _write_private(path: Path, text: str) -> None:
    # Write a new file at 0600, then replace. Truncating the old inode would
    # leave the secret readable for a moment when --force meets mode 0644.
    fd, name = tempfile.mkstemp(dir=path.parent, prefix=f".{path.name}.", suffix=".tmp")
    tmp = Path(name)
    try:
        os.write(fd, text.encode("utf-8"))
        os.fsync(fd)
    except Exception:
        os.close(fd)
        os.unlink(tmp)
        raise
    else:
        os.close(fd)
    os.chmod(tmp, 0o600)
    os.replace(tmp, path)


_REMOVE_STEP = "Remove it, then run `fbull login`."


def _refused(path: Path, problem: str, step: str) -> str:
    return f"fbull won't use the token: {path} {problem}. {step}"


def _check_private(path: Path, mode: int) -> None:
    """Refuse a path that is a symlink, has group or other bits, or belongs to another user."""
    info = path.lstat()
    if stat.S_ISLNK(info.st_mode):
        raise TokenFileError(_refused(path, "is a symlink", "Remove the link, then run `fbull login`."))
    if info.st_uid != os.getuid():
        raise TokenFileError(_refused(path, "belongs to another user", _REMOVE_STEP))
    if stat.S_IMODE(info.st_mode) & 0o077:
        raise TokenFileError(_refused(path, "must be readable only by you", f"Run `chmod {mode:o} {path}`."))


def broker_channel(address: str) -> grpc.Channel:
    """Broker channel. The host decides the transport. The token only adds authorization metadata.

    127.0.0.1, ::1, and localhost stay plaintext. Any other host uses TLS.
    A TLS setup failure is returned. The call does not open a plaintext channel instead.
    """
    host = _address_host(address)
    if _plaintext_host(host):
        channel = grpc.insecure_channel(address)
    else:
        channel = grpc.secure_channel(address, tls_channel_credentials(host))
    token = resolve_token()
    if not token:
        return channel
    return grpc.intercept_channel(channel, _MetadataInterceptor(((_AUTHORIZATION, f"Bearer {token}"),)))


# host, host:port, [v6], or [v6]:port, and nothing else.
_HOST_PORT = re.compile(
    r"(?:\[(?P<v6>[0-9A-Fa-f:.]+)\]|(?P<host>[A-Za-z0-9](?:[A-Za-z0-9.-]{0,251}[A-Za-z0-9])?))(?::(?P<port>[0-9]{1,5}))?"
)


def _address_host(address: str) -> str:
    """The host of a gRPC host:port address. Anything else, such as user info or a path, is refused."""
    match = _HOST_PORT.fullmatch(address.strip())
    port = match and match.group("port")
    if match is None or (port is not None and not 0 < int(port) < 65536):
        raise InsecureTransportError(
            f"fbull won't send your token to `{address}` because it isn't a host:port address. "
            "Set FOREVERBULL_BROKER_ADDRESS or remote.broker_address to host:port, for example 127.0.0.1:50055."
        )
    return match.group("v6") or match.group("host")


def session_channel(address: str, secret: str) -> grpc.Channel:
    """Session channel. The user bearer is not sent. An empty secret adds no metadata."""
    channel = grpc.insecure_channel(address)
    cleaned = secret.strip()
    if not cleaned:
        return channel
    return grpc.intercept_channel(channel, _MetadataInterceptor(((SESSION_METADATA, cleaned),)))
