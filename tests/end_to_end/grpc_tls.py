"""Dial broker gRPC through Traefik TLS on the Kind cluster the demo started."""

import json
import os
import pathlib
import re
import socket
import subprocess
import tempfile
import time

import grpc

from foreverbull.broker.credentials import CA_ENV
from foreverbull.broker.credentials import broker_channel
from foreverbull.broker.credentials import set_cli_token


KIND_CONTEXT = "kind-foreverbull"
NAMESPACE = "foreverbull"
LOCAL_PORT = 18443
TOKEN = "fb_e2e_tls"
HEALTH = "/grpc.health.v1.Health/Check"
# HealthCheckResponse status SERVING is field 1, varint 1.
SERVING = b"\x08\x01"
REPO_ROOT = pathlib.Path(__file__).resolve().parents[2]


def exercise_broker_grpc_tls() -> None:
    """Install the route, trust its CA, and check health. Plaintext to that host must fail."""
    ca_dir = pathlib.Path(tempfile.mkdtemp(prefix="foreverbull-grpc-tls-"))
    ca_path = ca_dir / "ca.pem"
    script = REPO_ROOT / "deploy" / "kind" / "grpc-tls.sh"
    completed = subprocess.run(
        ["bash", str(script), str(ca_path)],
        capture_output=True,
        text=True,
        timeout=900,
        check=False,
    )
    if completed.returncode != 0:
        raise AssertionError(completed.stdout + "\n" + completed.stderr)
    if not ca_path.is_file() or ca_path.stat().st_size == 0:
        raise AssertionError("grpc tls setup did not write a CA")

    route = _json("get", "ingressroute", "foreverbull-grpc", "-n", NAMESPACE, "-o", "json")
    match = route["spec"]["routes"][0]["match"]
    found = re.fullmatch(r"Host\(`([^`]+)`\)", match)
    if found is None:
        raise AssertionError(f"unexpected route match {match}")
    host = found.group(1)
    service = route["spec"]["routes"][0]["services"][0]
    if service.get("scheme") != "h2c":
        raise AssertionError(f"route scheme is {service.get('scheme')}, want h2c")

    _ensure_hosts(host)
    service_port = _websecure_port()
    forward = subprocess.Popen(
        [
            "kubectl",
            "--context",
            KIND_CONTEXT,
            "-n",
            "traefik",
            "port-forward",
            "svc/traefik",
            f"{LOCAL_PORT}:{service_port}",
        ],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    previous_token = os.environ.get("FOREVERBULL_TOKEN")
    previous_ca = os.environ.get(CA_ENV)
    set_cli_token(None)
    os.environ["FOREVERBULL_TOKEN"] = TOKEN
    os.environ[CA_ENV] = str(ca_path)
    try:
        _wait_local(LOCAL_PORT, forward)
        address = f"{host}:{LOCAL_PORT}"
        deadline = time.monotonic() + 60
        last = "no response"
        while time.monotonic() < deadline:
            channel = broker_channel(address)
            try:
                response = _health(channel, 10)
            except grpc.RpcError as exc:
                last = f"{exc.code()}: {exc.details()}"
                time.sleep(2)
                continue
            finally:
                channel.close()
            if response == SERVING:
                break
            last = f"health bytes {response!r}"
            time.sleep(2)
        else:
            raise AssertionError(f"TLS health check did not report SERVING ({last})\n{_logs()}")
        insecure = grpc.insecure_channel(address)
        try:
            try:
                _health(insecure, 5)
            except Exception:
                pass
            else:
                raise AssertionError("plaintext gRPC reached the broker through Traefik")
        finally:
            insecure.close()
    finally:
        set_cli_token(None)
        if previous_token is None:
            os.environ.pop("FOREVERBULL_TOKEN", None)
        else:
            os.environ["FOREVERBULL_TOKEN"] = previous_token
        if previous_ca is None:
            os.environ.pop(CA_ENV, None)
        else:
            os.environ[CA_ENV] = previous_ca
        if forward.poll() is None:
            forward.terminate()
            try:
                forward.wait(timeout=5)
            except subprocess.TimeoutExpired:
                forward.kill()


def _health(channel: grpc.Channel, timeout: float) -> bytes:
    call = channel.unary_unary(
        HEALTH,
        request_serializer=lambda payload: payload,
        response_deserializer=lambda payload: payload,
    )
    return call(b"", timeout=timeout)


def _ensure_hosts(host: str) -> None:
    text = pathlib.Path("/etc/hosts").read_text(encoding="utf-8")
    if re.search(rf"(^|\s){re.escape(host)}(\s|$)", text):
        return
    completed = subprocess.run(
        ["sudo", "-n", "tee", "-a", "/etc/hosts"],
        input=f"127.0.0.1 {host}\n",
        text=True,
        capture_output=True,
        check=False,
    )
    if completed.returncode != 0:
        raise AssertionError(f"cannot add {host} to /etc/hosts: {completed.stderr}")


def _websecure_port() -> int:
    service = _json("get", "svc", "traefik", "-n", "traefik", "-o", "json")
    for port in service["spec"]["ports"]:
        if port.get("name") == "websecure":
            return int(port["port"])
    raise AssertionError("traefik service has no websecure port")


def _wait_local(port: int, proc: subprocess.Popen[str]) -> None:
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        if proc.poll() is not None:
            raise AssertionError(f"port-forward exited {proc.returncode}")
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=1):
                return
        except OSError:
            time.sleep(0.5)
    raise AssertionError(f"port-forward did not open 127.0.0.1:{port}")


def _json(*args: str) -> dict:
    return json.loads(_kubectl(*args))


def _kubectl(*args: str) -> str:
    completed = subprocess.run(
        ["kubectl", "--context", KIND_CONTEXT, *args],
        capture_output=True,
        text=True,
        timeout=60,
        check=False,
    )
    if completed.returncode != 0:
        raise AssertionError(completed.stdout + completed.stderr)
    return completed.stdout


def _logs() -> str:
    parts = []
    for args in (
        ["-n", "traefik", "logs", "deploy/traefik", "--tail=40"],
        ["-n", NAMESPACE, "logs", "deploy/foreverbull", "--tail=40"],
    ):
        completed = subprocess.run(
            ["kubectl", "--context", KIND_CONTEXT, *args],
            capture_output=True,
            text=True,
            timeout=30,
            check=False,
        )
        parts.append(completed.stdout + completed.stderr)
    return "\n".join(parts)
