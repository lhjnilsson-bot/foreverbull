#!/usr/bin/env bash
# Lint and template the platform chart. Does not talk to a cluster.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART="${HELM_CHART:-${SCRIPT_DIR}/foreverbull}"
RELEASE="${HELM_RELEASE:-foreverbull}"
NAMESPACE="${HELM_NAMESPACE:-foreverbull}"

if ! command -v helm >/dev/null 2>&1; then
  echo "helm is not installed. https://helm.sh/docs/intro/install/" >&2
  exit 1
fi

if [[ ! -f "${CHART}/Chart.yaml" ]]; then
  echo "Helm chart not found: ${CHART}" >&2
  exit 1
fi

helm lint "${CHART}"

rendered="$(mktemp)"
trap 'rm -f "${rendered}"' EXIT
helm template "${RELEASE}" "${CHART}" --namespace "${NAMESPACE}" > "${rendered}"
helm template "${RELEASE}" "${CHART}" --namespace "${NAMESPACE}" --set expose.hostPort=true >> "${rendered}"
# An installed release from before httpPort has no value. reuse-values must still render 8080.
helm template "${RELEASE}" "${CHART}" --namespace "${NAMESPACE}" --set expose.hostPort=true --set broker.httpPort=null >> "${rendered}"

# The three app images share global.imageRepository; third-party images do not.
for app_image in foreverbull zipline fb-grafana; do
  if ! grep -q "image: ghcr.io/lhjnilsson/${app_image}:" "${rendered}"; then
    echo "rendered chart must default ${app_image} to the ghcr.io/lhjnilsson repository" >&2
    exit 1
  fi
done

# Default imageTag is latest → app images must Always-pull (nightlies must not stick).
# Three app Deployments × three template passes appended above → expect >= 3.
always_count="$(grep -c "imagePullPolicy: Always" "${rendered}" || true)"
if [[ "${always_count}" -lt 3 ]]; then
  echo "expected >=3 imagePullPolicy Always for broker/zipline/grafana (floating latest), got ${always_count}" >&2
  exit 1
fi

prefixed="$(mktemp)"
trap 'rm -f "${rendered}" "${prefixed}"' EXIT
helm template "${RELEASE}" "${CHART}" --namespace "${NAMESPACE}" --set global.imageRepository=example.invalid/fb > "${prefixed}"
for app_image in foreverbull zipline fb-grafana; do
  if ! grep -q "image: example.invalid/fb/${app_image}:" "${prefixed}"; then
    echo "rendered chart must put ${app_image} under global.imageRepository" >&2
    exit 1
  fi
done

# sha-<shortsha> is an immutable registry tag and must stay tag syntax.
# sha256:<64 hex> is a digest pin (repository@sha256:…), per image.
sha_tag_render="$(mktemp)"
helm template "${RELEASE}" "${CHART}" --namespace "${NAMESPACE}" --set global.imageTag=sha-abcdef1 > "${sha_tag_render}"
if ! grep -q "image: ghcr.io/lhjnilsson/foreverbull:sha-abcdef1" "${sha_tag_render}"; then
  echo "a sha- tag must stay repository:tag" >&2
  rm -f "${sha_tag_render}"
  exit 1
fi
rm -f "${sha_tag_render}"

pin_a="$(printf 'a%.0s' {1..64})"
pin_b="$(printf 'b%.0s' {1..64})"
pin_c="$(printf 'c%.0s' {1..64})"
digest_render="$(mktemp)"
helm template "${RELEASE}" "${CHART}" --namespace "${NAMESPACE}" \
  --set global.imageRepository=ghcr.io/example \
  --set-string "images.broker.tag=sha256:${pin_a}" \
  --set-string "images.zipline.tag=sha256:${pin_b}" \
  --set-string "images.grafana.tag=sha256:${pin_c}" > "${digest_render}"
for pinned in \
  "image: ghcr.io/example/foreverbull@sha256:${pin_a}" \
  "image: ghcr.io/example/zipline@sha256:${pin_b}" \
  "image: ghcr.io/example/fb-grafana@sha256:${pin_c}" \
  "image: postgres:13.3-alpine"
do
  if ! grep -Fq "${pinned}" "${digest_render}"; then
    echo "rendered chart missing digest pin: ${pinned}" >&2
    rm -f "${digest_render}"
    exit 1
  fi
done
for tagged in \
  "image: ghcr.io/example/foreverbull:" \
  "image: ghcr.io/example/zipline:" \
  "image: ghcr.io/example/fb-grafana:"
do
  if grep -Fq "${tagged}" "${digest_render}"; then
    echo "digest pin must not also render a tag: ${tagged}" >&2
    rm -f "${digest_render}"
    exit 1
  fi
done
rm -f "${digest_render}"

if grep -F '/var/run/docker.sock' "${rendered}"; then
  echo "chart must not mount /var/run/docker.sock" >&2
  exit 1
fi

for kind in ServiceAccount Role RoleBinding; do
  if ! grep -q "^kind: ${kind}$" "${rendered}"; then
    echo "rendered chart is missing ${kind}" >&2
    exit 1
  fi
done

if ! grep -q 'name: GRAFANA_URL' "${rendered}"; then
  echo "broker must set GRAFANA_URL" >&2
  exit 1
fi

if ! grep -q 'name: FOREVERBULL_WEB_URL' "${rendered}"; then
  echo "grafana must set FOREVERBULL_WEB_URL" >&2
  exit 1
fi

if ! grep -A1 'name: FOREVERBULL_WEB_URL' "${rendered}" | grep -q 'value: ""'; then
  echo "FOREVERBULL_WEB_URL must default to empty" >&2
  exit 1
fi

broker_manifest="$(mktemp)"
trap 'rm -f "${rendered}" "${prefixed}" "${broker_manifest}"' EXIT
awk '
  function flush() {
    if (doc ~ /(^|\n)kind: Deployment(\n|$)/ && doc ~ /(^|\n)  name: foreverbull(\n|$)/) {
      printf "%s", doc
    }
  }
  $0 == "---" { flush(); doc = ""; next }
  { doc = doc $0 "\n" }
  END { flush() }
' "${rendered}" > "${broker_manifest}"

if [[ ! -s "${broker_manifest}" ]]; then
  echo "rendered chart is missing the broker Deployment" >&2
  exit 1
fi

if grep -q 'name: AUTH_ENABLED' "${broker_manifest}"; then
  echo "default broker manifest must not set AUTH_ENABLED" >&2
  exit 1
fi

for forbidden in \
  ALPACA_MARKETS_API_KEY \
  SAXO_ACCESS_TOKEN \
  IG_PASSWORD \
  IBKR_ACCESS_TOKEN \
  api.alpaca.markets
do
  if grep -F "${forbidden}" "${broker_manifest}"; then
    echo "broker manifest must not contain ${forbidden}" >&2
    exit 1
  fi
done

# The broker has no default for these and stops at startup without them.
for env_name in POSTGRES_URL NATS_URL MINIO_URL MINIO_ACCESS_KEY MINIO_SECRET MINIO_SECURE; do
  if ! grep -qE "^ +- name: ${env_name}$" "${broker_manifest}"; then
    echo "broker must set ${env_name}" >&2
    exit 1
  fi
done

# Boolean settings accept only true/false/1/0; anything else stops the pod.
for env_name in TRADING_ENABLED AUTH_ENABLED AUTH_COOKIE_SECURE AUTH_TRUST_PROXY MINIO_SECURE; do
  if grep -A1 -E "^ +- name: ${env_name}$" "${rendered}" | grep -E '^ +value:' | grep -vqE '^ +value: "(true|false)"$'; then
    echo "${env_name} must render as \"true\" or \"false\"" >&2
    exit 1
  fi
done

for case in 'broker.trading.enabled=yes|broker.trading.enabled must be true or false, got "yes"' \
  'broker.logLevel=verbose|broker.logLevel must be debug, info, warning, or error, got "verbose"'; do
  setting="${case%%|*}"
  message="${case#*|}"
  if output="$(helm template "${RELEASE}" "${CHART}" --namespace "${NAMESPACE}" --set-string "${setting}" 2>&1)"; then
    echo "chart must refuse ${setting}" >&2
    exit 1
  fi
  if ! grep -Fq "${message}" <<< "${output}"; then
    echo "refusal of ${setting} must say: ${message}" >&2
    exit 1
  fi
done

# Credentials come from Secret foreverbull-credentials, never a plain env value.
for env_name in POSTGRES_PASSWORD MINIO_ROOT_PASSWORD MINIO_SECRET; do
  if grep -A1 -E "^ +- name: ${env_name}$" "${rendered}" | grep -qE '^ +value:'; then
    echo "${env_name} must come from a secretKeyRef, not a plain value" >&2
    exit 1
  fi
done

# hostPort is networking only: the public defaults must stop the
# install with or without it, unless allowDefaults is explicit.
for hostport in false true; do
  for default_value in postgres.password=foreverbull minio.secretKey=minioadmin; do
    if output="$(helm template "${RELEASE}" "${CHART}" --namespace "${NAMESPACE}" --set "expose.hostPort=${hostport}" --set "${default_value}" 2>&1)"; then
      echo "chart must refuse ${default_value} with expose.hostPort=${hostport}" >&2
      exit 1
    fi
    if ! grep -Fq 'credentials.allowDefaults=true' <<< "${output}"; then
      echo "refusal of ${default_value} must name credentials.allowDefaults=true" >&2
      exit 1
    fi
  done
done
helm template "${RELEASE}" "${CHART}" --namespace "${NAMESPACE}" \
  --set postgres.password=foreverbull --set minio.secretKey=minioadmin --set credentials.allowDefaults=true >/dev/null

# A set password is expanded raw into POSTGRES_URL/DATABASE_URL, so a
# value that is not URL-safe must stop the install with this message.
for case in 'postgres.password|postgres.password=p@ss/word' 'minio.secretKey|minio.secretKey=p@ss:word'; do
  key="${case%%|*}"
  setting="${case#*|}"
  if output="$(helm template "${RELEASE}" "${CHART}" --namespace "${NAMESPACE}" --set "${setting}" 2>&1)"; then
    echo "chart must refuse non-URL-safe ${key}" >&2
    exit 1
  fi
  if ! grep -Fq "${key} must be URL-safe (letters, digits and -._~ are safe)" <<< "${output}"; then
    echo "chart must explain ${key} must be URL-safe, got: ${output}" >&2
    exit 1
  fi
done
# URL-safe set values render.
helm template "${RELEASE}" "${CHART}" --namespace "${NAMESPACE}" \
  --set 'postgres.password=S3cure.pass_word-1~x' --set 'minio.secretKey=S3cure.pass_word-1~x' >/dev/null

# Empty credentials with hostPort alone must generate, not fall back
# to the public defaults; only allowDefaults selects the defaults.
hostport_render="$(mktemp)"
helm template "${RELEASE}" "${CHART}" --namespace "${NAMESPACE}" --set expose.hostPort=true > "${hostport_render}"
if grep -Fq 'postgres-password: "Zm9yZXZlcmJ1bGw="' "${hostport_render}" \
  || grep -Fq 'minio-secretKey: "bWluaW9hZG1pbg=="' "${hostport_render}"; then
  echo "expose.hostPort alone must not select the public default credentials" >&2
  exit 1
fi
rm -f "${hostport_render}"
defaults_render="$(mktemp)"
helm template "${RELEASE}" "${CHART}" --namespace "${NAMESPACE}" --set credentials.allowDefaults=true > "${defaults_render}"
if ! grep -Fq 'postgres-password: "Zm9yZXZlcmJ1bGw="' "${defaults_render}" \
  || ! grep -Fq 'minio-secretKey: "bWluaW9hZG1pbg=="' "${defaults_render}"; then
  echo "credentials.allowDefaults=true must select the public defaults" >&2
  exit 1
fi
rm -f "${defaults_render}"

# Fail-fast migration guard. lookup is empty under helm template, so
# the shipped wiring is guarded statically below, and the factored
# condition is exercised on a copied chart whose extra template calls
# it with synthetic inputs (the copy is never installed).
helpers_for_guard="${CHART}/templates/_helpers.tpl"
for pattern in \
  'lookup "v1" "Secret" .Release.Namespace' \
  'lookup "v1" "PersistentVolumeClaim" .Release.Namespace "postgres"' \
  'lookup "apps/v1" "Deployment" .Release.Namespace "postgres"' \
  'foreverbull.requireCredentialMigration'; do
  if ! grep -Fq "${pattern}" "${helpers_for_guard}"; then
    echo "migration guard wiring is missing: ${pattern}" >&2
    exit 1
  fi
done
migration_chart="$(mktemp -d)"
trap 'rm -rf "${migration_chart}"' EXIT
cp -R "${CHART}/." "${migration_chart}/"
cat > "${migration_chart}/templates/zz-migration-check.yaml" <<'EOF'
{{- $mc := .Values.migrationCheck | default dict }}
{{- include "foreverbull.requireCredentialMigration" (list ($mc.existingPostgres | default false) ($mc.hasSecret | default false) ($mc.migrated | default false)) }}
EOF
if migration_output="$(helm template "${RELEASE}" "${migration_chart}" --namespace "${NAMESPACE}" --set migrationCheck.existingPostgres=true 2>&1)"; then
  echo "chart must fail a pre-Secret upgrade that was not migrated" >&2
  exit 1
fi
for required in 'Secret foreverbull-credentials' 'credentials.allowDefaults=true' 'postgres.password' 'minio.accessKey' 'minio.secretKey' 'rotate the password inside Postgres'; do
  if ! grep -Fq "${required}" <<< "${migration_output}"; then
    echo "migration error must mention: ${required}" >&2
    exit 1
  fi
done
helm template "${RELEASE}" "${migration_chart}" --namespace "${NAMESPACE}" --set migrationCheck.existingPostgres=true --set migrationCheck.migrated=true >/dev/null
helm template "${RELEASE}" "${migration_chart}" --namespace "${NAMESPACE}" --set migrationCheck.existingPostgres=true --set migrationCheck.hasSecret=true >/dev/null
helm template "${RELEASE}" "${migration_chart}" --namespace "${NAMESPACE}" >/dev/null
rm -rf "${migration_chart}"
trap - EXIT

# Generated credentials must stay stable across helm upgrade. The
# credential helper looks up the existing Secret in the release
# namespace and reuses its data before falling back to randAlphaNum.
# lookup only talks to a live cluster, so offline template renders
# cannot exercise it directly: guard the lookup pattern here, and
# simulate the upgrade's second pass by feeding the first render's
# generated values back as set values (what lookup returns live).
helpers="${CHART}/templates/_helpers.tpl"
if ! grep -Fq 'lookup "v1" "Secret" $root.Release.Namespace' "${helpers}" \
  || ! grep -Fq 'b64dec' "${helpers}" \
  || ! grep -Fq 'randAlphaNum 32' "${helpers}"; then
  echo "credential helper must reuse the existing Secret (lookup) before generating" >&2
  exit 1
fi
first_render="$(mktemp)"
second_render="$(mktemp)"
trap 'rm -f "${rendered}" "${broker_manifest}" "${first_render}" "${second_render}"' EXIT
helm template "${RELEASE}" "${CHART}" --namespace "${NAMESPACE}" > "${first_render}"
secret_value() {
  grep -E "^  $1: " "$2" | head -n 1 | sed -E 's/^[^:]+: "?(.*)"?$/\1/' | tr -d '"'
}
generated_pg="$(secret_value postgres-password "${first_render}")"
generated_minio="$(secret_value minio-secretKey "${first_render}")"
if [[ -z "${generated_pg}" || -z "${generated_minio}" ]]; then
  echo "first render must generate postgres and MinIO credentials" >&2
  exit 1
fi
generated_pg_clear="$(printf '%s' "${generated_pg}" | base64 -d)"
generated_minio_clear="$(printf '%s' "${generated_minio}" | base64 -d)"
if [[ ! "${generated_pg_clear}" =~ ^[-A-Za-z0-9._~]+$ || ! "${generated_minio_clear}" =~ ^[-A-Za-z0-9._~]+$ ]]; then
  echo "generated credentials must be URL-safe" >&2
  exit 1
fi
helm template "${RELEASE}" "${CHART}" --namespace "${NAMESPACE}" \
  --set "postgres.password=${generated_pg_clear}" --set "minio.secretKey=${generated_minio_clear}" > "${second_render}"
if [[ "$(secret_value postgres-password "${second_render}")" != "${generated_pg}" \
  || "$(secret_value minio-secretKey "${second_render}")" != "${generated_minio}" ]]; then
  echo "credentials fed back on upgrade must render the same Secret" >&2
  exit 1
fi

if ! grep -q '^kind: NetworkPolicy$' "${first_render}"; then
  echo "chart must include a NetworkPolicy for service pods" >&2
  exit 1
fi
policy="$(mktemp)"
awk '
  function flush() {
    if (doc ~ /(^|\n)kind: NetworkPolicy(\n|$)/) {
      printf "%s", doc
    }
  }
  $0 == "---" { flush(); doc = ""; next }
  { doc = doc $0 "\n" }
  END { flush() }
' "${first_render}" > "${policy}"
if grep -E 'port: 4222|port: 9000' "${policy}"; then
  echo "service pods must not be allowed to reach NATS or MinIO" >&2
  exit 1
fi
if ! grep -q 'app.kubernetes.io/name: foreverbull' "${policy}" \
  || ! grep -q 'app.kubernetes.io/name: postgres' "${policy}"; then
  echo "service pods must be allowed to reach the broker and Postgres" >&2
  exit 1
fi
grafana_policy="$(mktemp)"
awk '
  function flush() {
    if (doc ~ /(^|\n)kind: NetworkPolicy(\n|$)/ && doc ~ /(^|\n)  name: grafana(\n|$)/) {
      printf "%s", doc
    }
  }
  $0 == "---" { flush(); doc = ""; next }
  { doc = doc $0 "\n" }
  END { flush() }
' "${first_render}" > "${grafana_policy}"
if ! grep -q 'app.kubernetes.io/name: grafana' "${grafana_policy}" \
  || ! grep -q 'port: 50055' "${grafana_policy}"; then
  echo "grafana must be allowed to reach broker gRPC" >&2
  exit 1
fi
if grep -E 'port: 4222|port: 9000|port: 5432|name: postgres' "${grafana_policy}"; then
  echo "grafana must not reach NATS, MinIO, or Postgres" >&2
  exit 1
fi
rm -f "${policy}" "${grafana_policy}"
for secret_name in foreverbull-nats foreverbull-worker; do
  if ! grep -q "name: ${secret_name}$" "${first_render}"; then
    echo "chart must create Secret ${secret_name}" >&2
    exit 1
  fi
done
for env_name in NATS_PASSWORD WORKER_DB_PASSWORD; do
  if grep -A1 -E "^ +- name: ${env_name}$" "${first_render}" | grep -qE '^ +value:'; then
    echo "${env_name} must come from a secretKeyRef, not a plain value" >&2
    exit 1
  fi
done
if ! grep -Fq 'postgres://foreverbull_worker:$(WORKER_DB_PASSWORD)@' "${first_render}"; then
  echo "broker must pass the reader URL, not a raw worker password" >&2
  exit 1
fi
nats_conf_b64="$(awk '/^  nats.conf:/{print; exit}' "${first_render}" | sed -E 's/.*"([^"]+)".*/\1/')"
nats_conf="$(printf '%s' "${nats_conf_b64}" | base64 -d)"
if ! grep -q 'user: "broker"' <<< "${nats_conf}"; then
  echo "nats config must authenticate the broker user" >&2
  exit 1
fi

if grep -A2 -E '^ +- name: AUTH_COOKIE_SECURE$' "${first_render}" | grep -q 'value: "true"'; then
  echo "default chart must leave AUTH_COOKIE_SECURE unset" >&2
  exit 1
fi
tls_render="$(mktemp)"
helm template "${RELEASE}" "${CHART}" --namespace "${NAMESPACE}" --set ingress.tls=true > "${tls_render}"
if ! grep -A1 -E '^ +- name: AUTH_COOKIE_SECURE$' "${tls_render}" | grep -q 'value: "true"'; then
  echo "ingress.tls must set AUTH_COOKIE_SECURE" >&2
  exit 1
fi
rm -f "${tls_render}"

# Non-root policy for the three app Deployments. ${rendered} includes the
# default template and the Kind hostPort template, so each Deployment is
# checked more than once. Postgres, NATS, and MinIO stay on their image users.
# Documents are split to files: mawk does not pass a NUL record to read -d ''.
deploy_docs="$(mktemp -d)"
awk -v dir="${deploy_docs}" '
  function flush() {
    if (doc ~ /(^|\n)kind: Deployment(\n|$)/) {
      n++
      path = dir "/" n
      printf "%s", doc > path
      close(path)
    }
  }
  $0 == "---" { flush(); doc = ""; next }
  { doc = doc $0 "\n" }
  END { flush() }
' "${rendered}"
app_seen_foreverbull=0
app_seen_zipline=0
app_seen_grafana=0
infra_seen_postgres=0
infra_seen_nats=0
infra_seen_minio=0
for doc_path in "${deploy_docs}"/*; do
  [[ -f "${doc_path}" ]] || continue
  name="$(awk '/^  name: /{print $2; exit}' "${doc_path}")"
  case "${name}" in
    foreverbull|zipline|grafana)
      uid=10001
      if [[ "${name}" == "grafana" ]]; then
        uid=472
      fi
      for required in \
        "runAsNonRoot: true" \
        "runAsUser: ${uid}" \
        "allowPrivilegeEscalation: false" \
        "type: RuntimeDefault"
      do
        if ! grep -Fq "${required}" "${doc_path}"; then
          echo "${name} deployment must set ${required}" >&2
          exit 1
        fi
      done
      if ! grep -Eq '^[[:space:]]+- ALL$' "${doc_path}"; then
        echo "${name} deployment must drop capability ALL" >&2
        exit 1
      fi
      if grep -Eq 'privileged:[[:space:]]*true' "${doc_path}"; then
        echo "${name} deployment must not be privileged" >&2
        exit 1
      fi
      if [[ "${name}" == "grafana" ]] && ! grep -Fq 'fsGroup: 472' "${doc_path}"; then
        echo "grafana must keep fsGroup 472" >&2
        exit 1
      fi
      case "${name}" in
        foreverbull) app_seen_foreverbull=$((app_seen_foreverbull + 1)) ;;
        zipline) app_seen_zipline=$((app_seen_zipline + 1)) ;;
        grafana) app_seen_grafana=$((app_seen_grafana + 1)) ;;
      esac
      ;;
    postgres|nats|minio)
      if grep -Eq 'runAsNonRoot:|allowPrivilegeEscalation:|RuntimeDefault' "${doc_path}"; then
        echo "${name} stays on its image user in this slice" >&2
        exit 1
      fi
      case "${name}" in
        postgres) infra_seen_postgres=$((infra_seen_postgres + 1)) ;;
        nats) infra_seen_nats=$((infra_seen_nats + 1)) ;;
        minio) infra_seen_minio=$((infra_seen_minio + 1)) ;;
      esac
      ;;
  esac
done
rm -rf "${deploy_docs}"

# Zipline's Python storage client has no default endpoint or keys, so the
# chart sets every one; the keys come from the credentials Secret.
zipline_manifest="$(mktemp)"
awk '
  function flush() {
    if (doc ~ /(^|\n)kind: Deployment(\n|$)/ && doc ~ /(^|\n)  name: zipline(\n|$)/) {
      printf "%s", doc
    }
  }
  $0 == "---" { flush(); doc = ""; next }
  { doc = doc $0 "\n" }
  END { flush() }
' "${rendered}" > "${zipline_manifest}"
for env_name in STORAGE_ENDPOINT STORAGE_ACCESS_KEY STORAGE_SECRET_KEY STORAGE_SECURE; do
  if ! grep -qE "^ +- name: ${env_name}$" "${zipline_manifest}"; then
    echo "zipline must set ${env_name}" >&2
    exit 1
  fi
done
for env_name in STORAGE_ACCESS_KEY STORAGE_SECRET_KEY; do
  if grep -A1 -E "^ +- name: ${env_name}$" "${zipline_manifest}" | grep -qE '^ +value:'; then
    echo "${env_name} must come from a secretKeyRef, not a plain value" >&2
    exit 1
  fi
done
if ! grep -A1 -E '^ +- name: STORAGE_SECURE$' "${zipline_manifest}" | grep -qE '^ +value: "(true|false)"$'; then
  echo "STORAGE_SECURE must render as \"true\" or \"false\"" >&2
  exit 1
fi
rm -f "${zipline_manifest}"

for pair in \
  "foreverbull:${app_seen_foreverbull}" \
  "zipline:${app_seen_zipline}" \
  "grafana:${app_seen_grafana}" \
  "postgres:${infra_seen_postgres}" \
  "nats:${infra_seen_nats}" \
  "minio:${infra_seen_minio}"
do
  workload="${pair%%:*}"
  count="${pair##*:}"
  if [[ "${count}" -lt 1 ]]; then
    echo "rendered chart is missing Deployment ${workload}" >&2
    exit 1
  fi
done

echo "helm chart checks passed"
