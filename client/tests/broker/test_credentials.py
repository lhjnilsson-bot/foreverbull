import os
import stat

from pathlib import Path

import pytest

from foreverbull.broker import credentials
from foreverbull.broker.config import resolve_broker_url
from foreverbull.broker.config import save_cluster_config
from foreverbull.broker.credentials import CA_ENV
from foreverbull.broker.credentials import InsecureTransportError
from foreverbull.broker.credentials import TokenFileError
from foreverbull.broker.credentials import TokenMeta
from foreverbull.broker.credentials import broker_channel
from foreverbull.broker.credentials import delete_token_files
from foreverbull.broker.credentials import read_token_meta
from foreverbull.broker.credentials import resolve_token
from foreverbull.broker.credentials import save_sign_in
from foreverbull.broker.credentials import save_token_file
from foreverbull.broker.credentials import set_cli_token
from foreverbull.broker.credentials import token_meta_path
from foreverbull.broker.credentials import token_path


@pytest.fixture(autouse=True)
def _clear_cli_token():
    set_cli_token(None)
    yield
    set_cli_token(None)


def test_resolve_order_trims_and_ignores_cluster_config(tmp_path: Path):
    home = tmp_path / "home"
    config = home / ".foreverbull"
    config.mkdir(parents=True, mode=0o700)
    (config / "config.yaml").write_text("cluster: local\ntoken: fb_from_config\n")
    path = token_path(home)
    path.write_text("  fb_file \n")
    path.chmod(0o600)

    assert resolve_token(environ={}, home=home) == "fb_file"
    assert resolve_token(environ={"FOREVERBULL_TOKEN": " fb_env "}, home=home) == "fb_env"
    set_cli_token(" fb_cli ")
    assert resolve_token(environ={"FOREVERBULL_TOKEN": "fb_env"}, home=home) == "fb_cli"
    assert "fb_from_config" not in path.read_text()


def test_loose_token_file_refuses_without_the_secret(tmp_path: Path):
    home = tmp_path / "home"
    path = token_path(home)
    path.parent.mkdir(parents=True, mode=0o700)
    path.write_text("fb_hidden_secret")
    path.chmod(0o644)
    with pytest.raises(TokenFileError) as raised:
        resolve_token(environ={}, home=home)
    assert (
        str(raised.value) == f"fbull won't use the token: {path} must be readable only by you. Run `chmod 600 {path}`."
    )


def test_save_token_file_is_0600_and_refuses_without_force(tmp_path: Path):
    home = tmp_path / "home"
    path = save_token_file(" fb_first \n", force=False, home=home)
    assert stat.S_IMODE(path.stat().st_mode) == 0o600
    assert path.read_text() == "fb_first"
    with pytest.raises(TokenFileError, match="already exists"):
        save_token_file("fb_second", force=False, home=home)
    assert path.read_text() == "fb_first"
    path.chmod(0o644)
    replaced = save_token_file("fb_second", force=True, home=home)
    assert replaced.read_text() == "fb_second"
    assert stat.S_IMODE(replaced.stat().st_mode) == 0o600
    assert list(path.parent.glob(f".{path.name}.*.tmp")) == []


def test_save_cluster_config_drops_token(tmp_path: Path):
    path = tmp_path / "config.yaml"
    path.write_text("cluster: local\ntoken: fb_secret\n")
    save_cluster_config({"cluster": "remote", "token": "fb_other"}, path=path)
    text = path.read_text()
    assert "fb_secret" not in text
    assert "fb_other" not in text
    assert "token:" not in text
    assert os.path.basename(path) == "config.yaml"


META = TokenMeta(expires_at="2026-12-04T06:00:00Z", client="cli@laptop", broker="http://localhost:8080")


def test_save_makes_the_directory_0700(tmp_path: Path):
    home = tmp_path / "home"
    (home / ".foreverbull").mkdir(parents=True, mode=0o755)
    (home / ".foreverbull").chmod(0o755)
    path = save_token_file("fb_first", force=False, home=home)
    assert stat.S_IMODE(path.parent.stat().st_mode) == 0o700


def test_loose_token_directory_refuses_without_the_secret(tmp_path: Path):
    home = tmp_path / "home"
    path = save_token_file("fb_hidden_secret", force=False, home=home)
    path.parent.chmod(0o755)
    with pytest.raises(TokenFileError) as raised:
        resolve_token(environ={}, home=home)
    assert str(raised.value) == (
        f"fbull won't use the token: {path.parent} must be readable only by you. Run `chmod 700 {path.parent}`."
    )


def test_token_file_of_another_owner_is_refused(tmp_path: Path, monkeypatch):
    home = tmp_path / "home"
    path = save_token_file("fb_hidden_secret", force=False, home=home)
    other = os.getuid() + 1
    monkeypatch.setattr(credentials.os, "getuid", lambda: other)
    with pytest.raises(TokenFileError) as raised:
        resolve_token(environ={}, home=home)
    assert str(raised.value) == (
        f"fbull won't use the token: {path.parent} belongs to another user. Remove it, then run `fbull login`."
    )


def test_symlinked_token_file_is_refused(tmp_path: Path):
    home = tmp_path / "home"
    real = tmp_path / "elsewhere"
    real.write_text("fb_hidden_secret")
    real.chmod(0o600)
    path = token_path(home)
    path.parent.mkdir(parents=True, mode=0o700)
    path.symlink_to(real)
    with pytest.raises(TokenFileError) as raised:
        resolve_token(environ={}, home=home)
    assert str(raised.value) == (
        f"fbull won't use the token: {path} is a symlink. Remove the link, then run `fbull login`."
    )


def test_sign_in_writes_token_and_meta_without_the_secret_in_meta(tmp_path: Path):
    home = tmp_path / "home"
    path = save_sign_in("fb_signed_in", META, home=home)
    meta_path = token_meta_path(home)
    assert stat.S_IMODE(path.stat().st_mode) == 0o600
    assert stat.S_IMODE(meta_path.stat().st_mode) == 0o600
    assert "fb_signed_in" not in meta_path.read_text()
    assert read_token_meta(home) == META
    assert resolve_token(environ={}, home=home) == "fb_signed_in"
    assert list(path.parent.glob(".*.tmp")) == []


def test_loose_meta_is_refused(tmp_path: Path):
    home = tmp_path / "home"
    save_sign_in("fb_signed_in", META, home=home)
    token_meta_path(home).chmod(0o644)
    with pytest.raises(TokenFileError) as raised:
        read_token_meta(home)
    meta = token_meta_path(home)
    assert (
        str(raised.value) == f"fbull won't use the token: {meta} must be readable only by you. Run `chmod 600 {meta}`."
    )


def test_token_save_drops_an_old_sign_in_meta(tmp_path: Path):
    home = tmp_path / "home"
    save_sign_in("fb_signed_in", META, home=home)
    save_token_file("fb_saved", force=True, home=home)
    assert read_token_meta(home) is None
    assert resolve_token(environ={}, home=home) == "fb_saved"


def test_delete_token_files_removes_both(tmp_path: Path):
    home = tmp_path / "home"
    save_sign_in("fb_signed_in", META, home=home)
    delete_token_files(home)
    assert not token_path(home).exists()
    assert not token_meta_path(home).exists()


@pytest.mark.parametrize("address", ["localhost:50055", "127.0.0.1:50055", "[::1]:50055"])
def test_token_goes_over_plaintext_grpc_only_to_loopback_or_kind(monkeypatch, address):
    monkeypatch.setenv("FOREVERBULL_TOKEN", "fb_secret")
    broker_channel(address).close()


@pytest.mark.parametrize(
    "address",
    ["fb.example.com:50055", "10.0.0.5:50055", "127.0.0.1.example.com:50055", "[2001:db8::1]:443"],
)
def test_token_to_a_remote_host_uses_tls_and_not_plaintext(monkeypatch, address):
    monkeypatch.setenv("FOREVERBULL_TOKEN", "fb_secret")
    monkeypatch.delenv(CA_ENV, raising=False)
    plaintext: list[str] = []
    monkeypatch.setattr(credentials.grpc, "insecure_channel", lambda addr: plaintext.append(addr))
    broker_channel(address).close()
    assert plaintext == []


def test_tls_failure_does_not_fall_back_to_plaintext(monkeypatch):
    monkeypatch.setenv("FOREVERBULL_TOKEN", "fb_secret")
    monkeypatch.delenv(CA_ENV, raising=False)
    plaintext: list[str] = []

    def boom(address, creds):
        raise RuntimeError(f"tls setup failed for {address}")

    monkeypatch.setattr(credentials.grpc, "secure_channel", boom)
    monkeypatch.setattr(credentials.grpc, "insecure_channel", lambda addr: plaintext.append(addr))
    with pytest.raises(RuntimeError, match="tls setup failed for fb.example.com:443"):
        broker_channel("fb.example.com:443")
    assert plaintext == []


def test_loopback_stays_plaintext_when_a_private_ca_is_set(monkeypatch, tmp_path: Path):
    pem = tmp_path / "ca.pem"
    pem.write_bytes(b"-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n")
    monkeypatch.setenv("FOREVERBULL_TOKEN", "fb_secret")
    monkeypatch.setenv(CA_ENV, str(pem))
    secure: list[str] = []
    monkeypatch.setattr(credentials.grpc, "secure_channel", lambda *args, **kwargs: secure.append("called"))
    broker_channel("127.0.0.1:50055").close()
    assert secure == []


def test_private_ca_is_the_trust_anchor(monkeypatch, tmp_path: Path):
    pem = tmp_path / "ca.pem"
    body = b"-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"
    pem.write_bytes(body)
    monkeypatch.setenv("FOREVERBULL_TOKEN", "fb_secret")
    monkeypatch.setenv(CA_ENV, str(pem))
    seen: dict[str, object] = {}

    def creds(root_certificates=None, private_key=None, certificate_chain=None):
        seen["roots"] = root_certificates
        return "creds"

    def secure(address, channel_credentials, options=None, compression=None):
        seen["address"] = address
        seen["credentials"] = channel_credentials
        return credentials.grpc.insecure_channel("127.0.0.1:1")

    monkeypatch.setattr(credentials.grpc, "ssl_channel_credentials", creds)
    monkeypatch.setattr(credentials.grpc, "secure_channel", secure)
    broker_channel("fb.example.com:443").close()
    assert seen["roots"] == body
    assert seen["address"] == "fb.example.com:443"
    assert seen["credentials"] == "creds"


def test_missing_private_ca_refuses_the_token(monkeypatch, tmp_path: Path):
    missing = tmp_path / "missing.pem"
    monkeypatch.setenv("FOREVERBULL_TOKEN", "fb_secret")
    monkeypatch.setenv(CA_ENV, str(missing))
    plaintext: list[str] = []
    monkeypatch.setattr(credentials.grpc, "insecure_channel", lambda addr: plaintext.append(addr))
    with pytest.raises(InsecureTransportError) as raised:
        broker_channel("fb.example.com:443")
    assert str(raised.value) == (
        f"fbull won't send your token to fb.example.com without encryption. {CA_ENV} at {missing} can't be read."
    )
    assert plaintext == []


def test_a_private_ca_that_is_not_a_certificate_refuses_the_token(monkeypatch, tmp_path: Path):
    pem = tmp_path / "ca.pem"
    pem.write_bytes(b"not a certificate\n")
    monkeypatch.setenv("FOREVERBULL_TOKEN", "fb_secret")
    monkeypatch.setenv(CA_ENV, str(pem))
    plaintext: list[str] = []
    monkeypatch.setattr(credentials.grpc, "insecure_channel", lambda addr: plaintext.append(addr))
    with pytest.raises(InsecureTransportError) as raised:
        broker_channel("fb.example.com:443")
    assert str(raised.value) == (
        f"fbull won't send your token to fb.example.com without encryption. {CA_ENV} at {pem} is not a PEM certificate."
    )
    assert plaintext == []


def test_empty_private_ca_refuses_the_token(monkeypatch, tmp_path: Path):
    pem = tmp_path / "ca.pem"
    pem.write_bytes(b" \n")
    monkeypatch.setenv("FOREVERBULL_TOKEN", "fb_secret")
    monkeypatch.setenv(CA_ENV, str(pem))
    with pytest.raises(InsecureTransportError) as raised:
        broker_channel("fb.example.com:443")
    assert str(raised.value) == (
        f"fbull won't send your token to fb.example.com without encryption. {CA_ENV} at {pem} is empty."
    )


@pytest.mark.parametrize(
    "address",
    [
        "127.0.0.1:50055@evil.com",
        "127.0.0.1:50055/evil",
        "127.0.0.1:50055 evil.com",
        "user@127.0.0.1:50055",
        "127.0.0.1:",
        "127.0.0.1:0",
        "127.0.0.1:65536",
        "localhost:50055:1",
        "[::1]:50055x",
        "[::1",
        "",
    ],
)
def test_a_malformed_grpc_address_is_refused(monkeypatch, address):
    monkeypatch.setenv("FOREVERBULL_TOKEN", "fb_secret")
    with pytest.raises(InsecureTransportError) as raised:
        broker_channel(address)
    assert str(raised.value) == (
        f"fbull won't send your token to `{address}` because it isn't a host:port address. "
        "Set FOREVERBULL_BROKER_ADDRESS or remote.broker_address to host:port, for example 127.0.0.1:50055."
    )


@pytest.mark.parametrize(
    ("address", "host"),
    [
        ("localhost", "localhost"),
        ("[::1]", "::1"),
        ("fb.example.com:443", "fb.example.com"),
        ("[fe80::1]:1", "fe80::1"),
    ],
)
def test_address_host_reads_host_and_port(address, host):
    assert credentials._address_host(address) == host


def test_the_token_is_written_through_an_unpredictable_temporary_file(tmp_path: Path):
    home = tmp_path / "home"
    path = token_path(home)
    path.parent.mkdir(mode=0o700, parents=True)
    decoy = tmp_path / "decoy"
    decoy.write_text("untouched")
    (path.parent / f".token.{os.getpid()}.tmp").symlink_to(decoy)
    save_token_file("fb_saved", force=False, home=home)
    assert decoy.read_text() == "untouched"
    assert path.read_text() == "fb_saved"
    assert sorted(p.name for p in path.parent.iterdir()) == [f".token.{os.getpid()}.tmp", "token"]


def test_remote_host_without_a_token_uses_tls_and_not_plaintext(monkeypatch, tmp_path: Path):
    monkeypatch.delenv("FOREVERBULL_TOKEN", raising=False)
    monkeypatch.delenv(CA_ENV, raising=False)
    monkeypatch.setenv("HOME", str(tmp_path))
    seen: dict[str, object] = {}
    issued: dict[str, object] = {}
    real_creds = credentials.grpc.ssl_channel_credentials

    def creds(*args, **kwargs):
        issued["credentials"] = real_creds(*args, **kwargs)
        return issued["credentials"]

    def secure(address, channel_credentials, options=None, compression=None):
        seen["address"] = address
        seen["credentials"] = channel_credentials

        class _Channel:
            def close(self):
                return None

        return _Channel()

    monkeypatch.setattr(credentials.grpc, "ssl_channel_credentials", creds)
    monkeypatch.setattr(credentials.grpc, "secure_channel", secure)
    plaintext: list[str] = []
    monkeypatch.setattr(credentials.grpc, "insecure_channel", lambda addr: plaintext.append(addr))
    broker_channel("fb.example.com:50055").close()
    assert plaintext == []
    assert seen["address"] == "fb.example.com:50055"
    assert seen["credentials"] is issued["credentials"]


def test_a_remote_failure_without_a_token_does_not_mention_the_token(monkeypatch, tmp_path: Path):
    monkeypatch.delenv("FOREVERBULL_TOKEN", raising=False)
    monkeypatch.setenv("HOME", str(tmp_path))
    missing = tmp_path / "missing.pem"
    monkeypatch.setenv(CA_ENV, str(missing))
    with pytest.raises(InsecureTransportError) as raised:
        broker_channel("fb.example.com:443")
    assert str(raised.value) == (
        "fbull is refusing to connect to remote broker fb.example.com without TLS. "
        f"{CA_ENV} at {missing} can't be read."
    )
    with pytest.raises(InsecureTransportError) as raised:
        broker_channel("not a host")
    assert str(raised.value) == (
        "fbull is refusing to connect to `not a host` without TLS because it isn't a host:port address. "
        "Set FOREVERBULL_BROKER_ADDRESS or remote.broker_address to host:port, for example 127.0.0.1:50055."
    )


def test_broker_url_order(tmp_path: Path):
    assert resolve_broker_url(cwd=tmp_path, home=tmp_path, environ={}) == "http://localhost:8080"
    assert (
        resolve_broker_url(cwd=tmp_path, home=tmp_path, environ={"FOREVERBULL_BROKER_URL": "https://fb.example.com/"})
        == "https://fb.example.com"
    )
    project = tmp_path / ".foreverbull" / "config.yaml"
    project.parent.mkdir()
    project.write_text("cluster: remote\nremote:\n  broker_address: fb.example.com:50055\n")
    with pytest.raises(ValueError, match="remote.broker_url"):
        resolve_broker_url(cwd=tmp_path, home=tmp_path, environ={})
    project.write_text("cluster: remote\nremote:\n  broker_url: https://fb.example.com\n")
    assert resolve_broker_url(cwd=tmp_path, home=tmp_path, environ={}) == "https://fb.example.com"


def test_unreadable_meta_names_the_file(tmp_path: Path):
    home = tmp_path / "home"
    save_sign_in("fb_signed_in", META, home=home)
    meta = token_meta_path(home)
    meta.write_text("not json")
    with pytest.raises(TokenFileError) as raised:
        read_token_meta(home)
    assert str(raised.value) == f"fbull won't use the token: {meta} can't be read. Remove it, then run `fbull login`."
    meta.write_text('{"expires_at": "2026-12-04T06:00:00Z", "client": "cli@laptop"}')
    with pytest.raises(TokenFileError) as raised:
        read_token_meta(home)
    assert str(raised.value) == f"fbull won't use the token: {meta} has no broker. Remove it, then run `fbull login`."


def test_a_directory_in_place_of_the_token_file_is_refused(tmp_path: Path):
    home = tmp_path / "home"
    path = token_path(home)
    path.mkdir(parents=True, mode=0o700)
    path.parent.chmod(0o700)
    with pytest.raises(TokenFileError) as raised:
        resolve_token(environ={}, home=home)
    assert str(raised.value) == (
        f"fbull won't use the token: {path} isn't a regular file. Remove it, then run `fbull login`."
    )
