#!/usr/bin/env bash
# Turn on the chart's broker gRPC route in the Kind cluster the product demo
# already installed. cert-manager's self-signed issuer stands in for the
# nightly HTTP-01 Let's Encrypt issuer. Traefik serves TLS and dials the
# broker as h2c. Writes the CA the client must trust to the path in $1.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

CA_OUT="${1:-}"
CONTEXT="${KUBE_CONTEXT:-kind-foreverbull}"
NAMESPACE="${HELM_NAMESPACE:-foreverbull}"
RELEASE="${HELM_RELEASE:-foreverbull}"
CHART="${HELM_CHART:-${ROOT}/deploy/helm/foreverbull}"
HOST="${GRPC_TLS_HOST:-grpc.foreverbull.test}"
ISSUER="${GRPC_TLS_ISSUER:-foreverbull-ca}"
CERT_MANAGER_VERSION="${CERT_MANAGER_VERSION:-v1.21.3}"
TRAEFIK_CHART="${TRAEFIK_CHART:-https://traefik.github.io/charts/traefik/traefik-42.3.1.tgz}"
WAIT="${GRPC_TLS_WAIT:-10m}"

if [[ -z "${CA_OUT}" ]]; then
  echo "usage: deploy/kind/grpc-tls.sh <ca-output-path>" >&2
  exit 1
fi
if [[ ! "${HOST}" =~ ^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$ || ! "${ISSUER}" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]]; then
  echo "refusing host ${HOST} or issuer ${ISSUER}" >&2
  exit 1
fi
if ! command -v helm >/dev/null 2>&1 || ! command -v kubectl >/dev/null 2>&1; then
  echo "helm and kubectl are required" >&2
  exit 1
fi
if [[ ! -d "${CHART}" ]]; then
  echo "Helm chart not found: ${CHART}" >&2
  exit 1
fi

dump() {
  echo "grpc tls setup failed" >&2
  kubectl --context "${CONTEXT}" get pods -A || true
  kubectl --context "${CONTEXT}" -n cert-manager describe certificate foreverbull-ca || true
  kubectl --context "${CONTEXT}" -n "${NAMESPACE}" describe certificate foreverbull-grpc || true
  kubectl --context "${CONTEXT}" -n "${NAMESPACE}" describe ingressroute foreverbull-grpc || true
  kubectl --context "${CONTEXT}" -n traefik logs deploy/traefik --tail=80 || true
  kubectl --context "${CONTEXT}" -n cert-manager logs deploy/cert-manager --tail=40 || true
}
on_err() {
  local status=$?
  trap - ERR
  dump
  exit "${status}"
}
trap on_err ERR

kube() {
  kubectl --context "${CONTEXT}" "$@"
}

echo "install cert-manager ${CERT_MANAGER_VERSION}"
kube apply -f "https://github.com/cert-manager/cert-manager/releases/download/${CERT_MANAGER_VERSION}/cert-manager.yaml"
for name in cert-manager cert-manager-cainjector cert-manager-webhook; do
  kube -n cert-manager wait --for=condition=Available "deployment/${name}" --timeout="${WAIT}"
done

echo "install traefik ${TRAEFIK_CHART}"
helm upgrade --install traefik "${TRAEFIK_CHART}" \
  --kube-context "${CONTEXT}" \
  --namespace traefik \
  --create-namespace \
  --set service.spec.type=ClusterIP \
  --wait \
  --timeout "${WAIT}"

kube get crd certificates.cert-manager.io >/dev/null
kube get crd ingressroutes.traefik.io >/dev/null

issuer="$(mktemp)"
cat >"${issuer}" <<'EOF'
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: foreverbull-selfsigned
spec:
  selfSigned: {}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: foreverbull-ca
  namespace: cert-manager
spec:
  isCA: true
  commonName: foreverbull-kind-ca
  secretName: foreverbull-ca
  privateKey:
    algorithm: ECDSA
    size: 256
  issuerRef:
    name: foreverbull-selfsigned
    kind: ClusterIssuer
EOF

attempt=0
until kube apply -f "${issuer}"; do
  attempt=$((attempt + 1))
  if [[ "${attempt}" -ge 30 ]]; then
    echo "cert-manager did not accept the self-signed issuer" >&2
    exit 1
  fi
  sleep 2
done
rm -f "${issuer}"
kube -n cert-manager wait --for=condition=Ready certificate/foreverbull-ca --timeout="${WAIT}"

ca_issuer="$(mktemp)"
cat >"${ca_issuer}" <<EOF
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: ${ISSUER}
spec:
  ca:
    secretName: foreverbull-ca
EOF
kube apply -f "${ca_issuer}"
rm -f "${ca_issuer}"

echo "helm upgrade ${RELEASE} ingress.tls host=${HOST} issuer=${ISSUER}"
helm upgrade "${RELEASE}" "${CHART}" \
  --kube-context "${CONTEXT}" \
  --namespace "${NAMESPACE}" \
  --reuse-values \
  --set ingress.tls=true \
  --set "ingress.host=${HOST}" \
  --set "ingress.clusterIssuer=${ISSUER}" \
  --set ingress.entryPoint=websecure \
  --wait \
  --timeout "${WAIT}"

kube -n "${NAMESPACE}" wait --for=condition=Ready certificate/foreverbull-grpc --timeout="${WAIT}"
scheme="$(kube -n "${NAMESPACE}" get ingressroute foreverbull-grpc -o jsonpath='{.spec.routes[0].services[0].scheme}')"
if [[ "${scheme}" != "h2c" ]]; then
  echo "IngressRoute scheme is ${scheme}, want h2c" >&2
  exit 1
fi

mkdir -p "$(dirname "${CA_OUT}")"
b64="$(kube -n "${NAMESPACE}" get secret foreverbull-grpc-tls -o 'jsonpath={.data.ca\.crt}')"
if [[ -z "${b64}" ]]; then
  echo "secret foreverbull-grpc-tls has no ca.crt" >&2
  exit 1
fi
printf '%s' "${b64}" | base64 -d >"${CA_OUT}"
if [[ ! -s "${CA_OUT}" ]]; then
  echo "ca.crt decoded empty" >&2
  exit 1
fi

trap - ERR
echo "grpc tls route ready host=${HOST}"
