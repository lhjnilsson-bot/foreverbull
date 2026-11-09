import shutil
import subprocess

from pathlib import Path

import pytest
import yaml


CHART_RELATIVE = Path("deploy") / "helm" / "foreverbull"
WORKLOADS = ("postgres", "nats", "minio", "zipline", "foreverbull", "grafana")


def _chart_path() -> Path:
    start = Path(__file__).resolve()
    for path in [start, *start.parents]:
        chart = path / CHART_RELATIVE
        if (chart / "Chart.yaml").is_file():
            return chart
    raise FileNotFoundError("Helm chart not found: deploy/helm/foreverbull")


helm = shutil.which("helm")
pytestmark = pytest.mark.skipif(helm is None, reason="helm is not installed")


def _run_helm(*args: str) -> subprocess.CompletedProcess[str]:
    assert helm is not None
    return subprocess.run([helm, *args], check=False, capture_output=True, text=True)


def _load_docs(rendered: str) -> list[dict]:
    docs = []
    for doc in yaml.safe_load_all(rendered):
        if isinstance(doc, dict) and doc.get("kind"):
            docs.append(doc)
    return docs


def _docs_of_kind(docs: list[dict], kind: str) -> list[dict]:
    return [doc for doc in docs if doc.get("kind") == kind]


def _broker_mounts() -> list[dict]:
    return [
        {"name": "trading-profiles", "mountPath": "/etc/foreverbull", "readOnly": True},
        {"name": "admin-password", "mountPath": "/etc/foreverbull-admin", "readOnly": True},
    ]


def _broker_volumes() -> list[dict]:
    return [
        {
            "name": "trading-profiles",
            "secret": {
                "secretName": "foreverbull-trading",
                "optional": True,
                "items": [{"key": "profiles.yaml", "path": "profiles.yaml"}],
            },
        },
        {
            "name": "admin-password",
            "secret": {
                "secretName": "foreverbull-admin",
                "optional": True,
                "items": [{"key": "password", "path": "password"}],
            },
        },
    ]


@pytest.fixture(scope="module")
def chart() -> Path:
    return _chart_path()


APP_RUN_AS_USER = {
    "foreverbull": 10001,
    "zipline": 10001,
    "grafana": 472,
}
INFRA_FS_GROUP = {
    "postgres": 70,
    "nats": 1000,
    "minio": 1000,
}


def _assert_runtime_hardening(docs: list[dict]) -> None:
    deployments = {doc["metadata"]["name"]: doc for doc in _docs_of_kind(docs, "Deployment")}
    for name, uid in APP_RUN_AS_USER.items():
        spec = deployments[name]["spec"]["template"]["spec"]
        pod_context = spec["securityContext"]
        assert pod_context["runAsNonRoot"] is True
        assert pod_context["runAsUser"] == uid
        assert pod_context["seccompProfile"] == {"type": "RuntimeDefault"}
        container = spec["containers"][0]["securityContext"]
        assert container["runAsNonRoot"] is True
        assert container["runAsUser"] == uid
        assert container["allowPrivilegeEscalation"] is False
        assert container["capabilities"] == {"drop": ["ALL"]}
        assert container["seccompProfile"] == {"type": "RuntimeDefault"}
        assert container.get("privileged") is not True
        assert "hostPID" not in spec
        assert "hostNetwork" not in spec
    assert deployments["foreverbull"]["spec"]["template"]["spec"]["automountServiceAccountToken"] is True
    assert deployments["grafana"]["spec"]["template"]["spec"]["securityContext"]["fsGroup"] == 472
    for name, fs_group in INFRA_FS_GROUP.items():
        spec = deployments[name]["spec"]["template"]["spec"]
        pod_context = spec.get("securityContext") or {}
        assert pod_context.get("fsGroup") == fs_group
        assert "runAsNonRoot" not in pod_context
        assert "runAsUser" not in pod_context
        container_context = spec["containers"][0].get("securityContext") or {}
        assert "runAsNonRoot" not in container_context
        assert "capabilities" not in container_context


def test_helm_lint(chart: Path):
    result = _run_helm("lint", str(chart))
    assert result.returncode == 0, result.stdout + result.stderr
    assert "0 chart(s) failed" in result.stdout or "1 chart(s) linted" in result.stdout


def test_helm_template_has_rbac_and_no_docker_sock(chart: Path):
    result = _run_helm("template", "foreverbull", str(chart), "--namespace", "foreverbull")
    assert result.returncode == 0, result.stdout + result.stderr
    rendered = result.stdout
    assert "/var/run/docker.sock" not in rendered
    assert "docker.sock" not in rendered

    docs = _load_docs(rendered)
    kinds = {doc["kind"] for doc in docs}
    assert "ServiceAccount" in kinds
    assert "Role" in kinds
    assert "RoleBinding" in kinds

    roles = _docs_of_kind(docs, "Role")
    assert roles
    verbs: set[str] = set()
    resources: set[str] = set()
    for rule in roles[0].get("rules") or []:
        verbs.update(rule.get("verbs") or [])
        resources.update(rule.get("resources") or [])
    assert {"get", "list", "watch", "create", "delete"} <= verbs
    assert "pods" in resources

    bindings = _docs_of_kind(docs, "RoleBinding")
    assert bindings
    subjects = bindings[0].get("subjects") or []
    assert any(s.get("kind") == "ServiceAccount" and s.get("name") == "foreverbull" for s in subjects)

    deployments = {doc["metadata"]["name"]: doc for doc in _docs_of_kind(docs, "Deployment")}
    assert set(WORKLOADS) <= set(deployments)
    broker = deployments["foreverbull"]
    spec = broker["spec"]["template"]["spec"]
    assert spec.get("serviceAccountName") == "foreverbull"
    container = spec["containers"][0]
    env = {item["name"]: item.get("value") for item in container.get("env") or []}
    assert env.get("TRADING_ENABLED") == "false"
    assert env.get("TRADING_PROFILES_FILE") == "/etc/foreverbull/profiles.yaml"
    assert env.get("AUTH_ADMIN_PASSWORD_FILE") == "/etc/foreverbull-admin/password"
    assert "AUTH_ENABLED" not in env
    for forbidden in (
        "ALPACA_MARKETS_API_KEY",
        "ALPACA_MARKETS_API_SECRET",
        "SAXO_ACCESS_TOKEN",
        "IG_PASSWORD",
        "IBKR_ACCESS_TOKEN",
        "TRADING_PROVIDER",
        "TRADING_ALLOW_LIVE_URL",
        "api.alpaca.markets",
    ):
        assert forbidden not in env
        assert forbidden not in rendered
    mounts = container.get("volumeMounts") or []
    assert mounts == _broker_mounts()
    volumes = spec.get("volumes") or []
    assert volumes == _broker_volumes()

    grafana_env = {
        item["name"]: item.get("value")
        for item in deployments["grafana"]["spec"]["template"]["spec"]["containers"][0].get("env") or []
    }
    assert grafana_env.get("BROKER_URL") == "foreverbull:50055"
    assert grafana_env.get("FOREVERBULL_WEB_URL") == ""
    _assert_runtime_hardening(docs)


def test_helm_web_url_empty_or_subpath(chart: Path):
    empty = _run_helm(
        "template",
        "foreverbull",
        str(chart),
        "--namespace",
        "foreverbull",
        "--set-string",
        "grafana.webUrl=",
    )
    assert empty.returncode == 0, empty.stdout + empty.stderr
    assert _grafana_env(empty.stdout).get("FOREVERBULL_WEB_URL") == ""

    subpath = _run_helm(
        "template",
        "foreverbull",
        str(chart),
        "--namespace",
        "foreverbull",
        "--set-string",
        "grafana.webUrl=https://example.com/fb/",
    )
    assert subpath.returncode == 0, subpath.stdout + subpath.stderr
    assert _grafana_env(subpath.stdout).get("FOREVERBULL_WEB_URL") == "https://example.com/fb/"


def _grafana_env(rendered: str) -> dict[str, str]:
    docs = _load_docs(rendered)
    deployments = {doc["metadata"]["name"]: doc for doc in _docs_of_kind(docs, "Deployment")}
    env = deployments["grafana"]["spec"]["template"]["spec"]["containers"][0].get("env") or []
    return {item["name"]: item.get("value") for item in env}


def test_helm_template_hostport_still_has_no_docker_sock(chart: Path):
    result = _run_helm(
        "template",
        "foreverbull",
        str(chart),
        "--namespace",
        "foreverbull",
        "--set",
        "expose.hostPort=true",
        "--set",
        "global.imageTag=local",
    )
    assert result.returncode == 0, result.stdout + result.stderr
    assert "/var/run/docker.sock" not in result.stdout
    docs = _load_docs(result.stdout)
    broker = next(doc for doc in _docs_of_kind(docs, "Deployment") if doc["metadata"]["name"] == "foreverbull")
    ports = broker["spec"]["template"]["spec"]["containers"][0]["ports"]
    grpc = next(port for port in ports if port.get("name") == "grpc")
    assert grpc.get("hostPort") == 50055
    spec = broker["spec"]["template"]["spec"]
    volumes = spec.get("volumes") or []
    mounts = spec["containers"][0].get("volumeMounts") or []
    assert volumes == _broker_volumes()
    assert mounts == _broker_mounts()
    assert "/var/run/docker.sock" not in result.stdout
    _assert_runtime_hardening(docs)
