import copy
import os

from collections.abc import Mapping
from pathlib import Path
from typing import Any

import yaml


DEFAULT_BROKER_ADDRESS = "localhost:50055"
DEFAULT_BROKER_PORT = 50055
BROKER_ADDRESS_ENV = "FOREVERBULL_BROKER_ADDRESS"
# The broker's HTTP origin, where `fbull login` opens the browser. Kind
# publishes the broker's HTTP port on localhost:8080.
DEFAULT_BROKER_URL = "http://localhost:8080"
BROKER_URL_ENV = "FOREVERBULL_BROKER_URL"
DEFAULT_NAMESPACE = "foreverbull"
DEFAULT_KIND_NAME = "foreverbull"
DEFAULT_KIND_CONFIG = "deploy/kind/cluster.yaml"

DEFAULT_CLUSTER_CONFIG: dict[str, Any] = {
    "cluster": "local",
    "namespace": DEFAULT_NAMESPACE,
    "kind": {
        "name": DEFAULT_KIND_NAME,
        "config": DEFAULT_KIND_CONFIG,
    },
    "remote": {
        "kubeconfig": "",
        "context": "",
        "namespace": "",
        "broker_address": "",
        "broker_url": "",
    },
}


def deep_merge(base: dict[str, Any], override: dict[str, Any]) -> dict[str, Any]:
    merged = dict(base)
    for key, value in override.items():
        if key in merged and isinstance(merged[key], dict) and isinstance(value, dict):
            merged[key] = deep_merge(merged[key], value)
        else:
            merged[key] = value
    return merged


def load_yaml_file(path: Path) -> dict[str, Any]:
    if not path.is_file():
        return {}
    with path.open() as fh:
        loaded = yaml.safe_load(fh)
    if loaded is None:
        return {}
    if not isinstance(loaded, dict):
        raise TypeError(f"Cluster config must be a mapping: {path}")
    return loaded


def load_cluster_config(
    *,
    cwd: Path | None = None,
    home: Path | None = None,
    environ: Mapping[str, str] | None = None,
) -> dict[str, Any]:
    environ = os.environ if environ is None else environ
    cwd = Path.cwd() if cwd is None else cwd
    home = Path.home() if home is None else home
    merged = copy.deepcopy(DEFAULT_CLUSTER_CONFIG)
    for path in (home / ".foreverbull" / "config.yaml", cwd / ".foreverbull" / "config.yaml"):
        merged = deep_merge(merged, load_yaml_file(path))
    explicit = environ.get("FOREVERBULL_CONFIG")
    if explicit:
        path = Path(explicit).expanduser()
        if not path.is_file():
            raise FileNotFoundError(f"FOREVERBULL_CONFIG not found: {path}")
        merged = deep_merge(merged, load_yaml_file(path))
    cluster = str(merged.get("cluster") or "local").lower()
    if cluster not in {"local", "remote"}:
        raise ValueError(f"cluster must be 'local' or 'remote', got {cluster!r}")
    merged["cluster"] = cluster
    return merged


def project_config_path(cwd: Path | None = None) -> Path:
    cwd = Path.cwd() if cwd is None else cwd
    return cwd / ".foreverbull" / "config.yaml"


def save_cluster_config(updates: dict[str, Any], *, cwd: Path | None = None, path: Path | None = None) -> Path:
    path = project_config_path(cwd) if path is None else path
    existing = load_yaml_file(path)
    merged = deep_merge(existing, updates)
    # Cluster config is not a credential store. A token key must not be saved here.
    merged.pop("token", None)
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w") as fh:
        yaml.safe_dump(merged, fh, default_flow_style=False, sort_keys=False)
    return path


def split_broker_address(address: str) -> tuple[str, int]:
    address = address.strip()
    if not address:
        raise ValueError("broker address is empty")
    if ":" not in address:
        return address, DEFAULT_BROKER_PORT
    host, port_s = address.rsplit(":", 1)
    if not host or not port_s.isdigit():
        raise ValueError(f"broker address must be host:port, got {address!r}")
    return host, int(port_s)


def resolve_broker_address(
    *,
    cwd: Path | None = None,
    home: Path | None = None,
    environ: Mapping[str, str] | None = None,
) -> str:
    environ = os.environ if environ is None else environ
    env_addr = str(environ.get(BROKER_ADDRESS_ENV) or "").strip()
    if env_addr:
        return env_addr
    data = load_cluster_config(cwd=cwd, home=home, environ=environ)
    if str(data.get("cluster") or "local") == "local":
        return DEFAULT_BROKER_ADDRESS
    addr = str((data.get("remote") or {}).get("broker_address") or "").strip()
    if not addr:
        raise ValueError(
            "cluster is remote but remote.broker_address is not set; "
            f"set it in .foreverbull/config.yaml or {BROKER_ADDRESS_ENV}"
        )
    return addr


def resolve_broker_url(
    *,
    cwd: Path | None = None,
    home: Path | None = None,
    environ: Mapping[str, str] | None = None,
) -> str:
    """The broker's HTTP origin: FOREVERBULL_BROKER_URL, then Kind's localhost:8080, then remote.broker_url."""
    environ = os.environ if environ is None else environ
    env_url = str(environ.get(BROKER_URL_ENV) or "").strip()
    if env_url:
        return env_url.rstrip("/")
    data = load_cluster_config(cwd=cwd, home=home, environ=environ)
    if str(data.get("cluster") or "local") == "local":
        return DEFAULT_BROKER_URL
    url = str((data.get("remote") or {}).get("broker_url") or "").strip()
    if not url:
        raise ValueError(
            "cluster is remote but remote.broker_url is not set; "
            f"set it in .foreverbull/config.yaml or {BROKER_URL_ENV}"
        )
    return url.rstrip("/")


def broker_host_port(
    hostname: str | None = None,
    port: int | None = None,
    *,
    cwd: Path | None = None,
    home: Path | None = None,
    environ: Mapping[str, str] | None = None,
) -> tuple[str, int]:
    resolved_host, resolved_port = split_broker_address(resolve_broker_address(cwd=cwd, home=home, environ=environ))
    return (
        resolved_host if hostname is None else hostname,
        resolved_port if port is None else port,
    )


def broker_host(
    *,
    cwd: Path | None = None,
    home: Path | None = None,
    environ: Mapping[str, str] | None = None,
) -> str:
    return broker_host_port(cwd=cwd, home=home, environ=environ)[0]


def broker_port(
    *,
    cwd: Path | None = None,
    home: Path | None = None,
    environ: Mapping[str, str] | None = None,
) -> int:
    return broker_host_port(cwd=cwd, home=home, environ=environ)[1]
