# Foreverbull Helm chart

Installs the six platform workloads into the release namespace (default
`foreverbull`). See [deploy/README.md](../../README.md) for the cluster
contract, value keys, and Kind host-port mappings.

```bash
# Local Kind
helm upgrade --install foreverbull deploy/helm/foreverbull \
  --namespace foreverbull \
  --create-namespace \
  --set expose.hostPort=true \
  --set credentials.allowDefaults=true \
  --set global.imageTag=local

# Remote
helm upgrade --install foreverbull deploy/helm/foreverbull \
  --kubeconfig /path/to/kubeconfig \
  --kube-context my-context \
  --namespace my-namespace \
  --create-namespace \
  --set expose.hostPort=false
```

`ingress.tls` defaults to false, so this install does not publish a Traefik
route. Set `ingress.tls=true` together with `ingress.host` and
`ingress.clusterIssuer` when the cluster already has Traefik and a
cert-manager ClusterIssuer. See [deploy/README.md](../../README.md).

Generated Postgres/MinIO credentials live in Secret
`foreverbull-credentials` in the release namespace and are reused on
upgrade via a live-cluster lookup. Read them back with:

```bash
kubectl get secret foreverbull-credentials -n <namespace> -o jsonpath='{.data.postgres-password}' | base64 -d; echo
kubectl get secret foreverbull-credentials -n <namespace> -o jsonpath='{.data.minio-accessKey}' | base64 -d; echo
kubectl get secret foreverbull-credentials -n <namespace> -o jsonpath='{.data.minio-secretKey}' | base64 -d; echo
```

Upgrading a release installed before Secret `foreverbull-credentials`
existed: follow
[Upgrading an existing remote install](../../README.md#upgrading-an-existing-remote-install)
first. The chart fails that upgrade until the old credentials are
migrated; never apply offline `helm template` output, which generates
new random credentials on every render.

Do not pass broker credentials as chart values. `broker.trading.enabled` stays false.
Optional Secret `foreverbull-trading` key `profiles.yaml` is mounted at `/etc/foreverbull/profiles.yaml`.

## 🔒️ Workload user

Broker and Zipline run as uid `10001`. Grafana runs as uid `472`, with
`fsGroup: 472`. Each of those containers sets `runAsNonRoot`,
`seccompProfile: RuntimeDefault`, `allowPrivilegeEscalation: false`,
and drops every capability. The policy is part of the template for
every install, including local Kind (`expose.hostPort=true`). Postgres,
NATS, and MinIO keep their image users. Details:
[Container security](../../README.md#container-security).

Report a vulnerability through [SECURITY.md](../../../SECURITY.md).
