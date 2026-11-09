#!/usr/bin/env bash
# Offline checks for the server release scripts. No cluster and no registry.
# shellcheck disable=SC2030,SC2031,SC2016
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=deploy/server/lib.sh
source "${ROOT}/deploy/server/lib.sh"

failed=0

ok() {
  echo "ok $1"
}

bad() {
  echo "FAIL $1" >&2
  failed=1
}

assert_ok() {
  local desc="$1"
  shift
  if "$@"; then
    ok "${desc}"
  else
    bad "${desc}"
  fi
}

assert_fail() {
  local desc="$1"
  shift
  if "$@"; then
    bad "${desc}"
  else
    ok "${desc}"
  fi
}

pin() {
  python3 -c 'import sys; print(sys.argv[1] * 64)' "$1"
}

for script in "${ROOT}/deploy/server/"*.sh; do
  bash -n "${script}" || bad "bash -n ${script}"
done
ok "bash -n"

if grep -R -n -E 'docker[[:space:]]+push|docker[[:space:]]+buildx[[:space:]]+build|docker[[:space:]]+build[[:space:]]' "${ROOT}/deploy/server"; then
  bad "server scripts must not build or docker-push an image"
else
  ok "no image build in server scripts"
fi
if grep -R -n 'manifests/latest' "${ROOT}/deploy/server" --exclude self-test.sh; then
  bad "deleting the latest manifest would delete the shared sha tag"
else
  ok "latest manifest is not deleted"
fi
grep -q 'only fb-nightly is served' "${ROOT}/deploy/server/retarget.sh" || bad "retarget must refuse other namespaces"
grep -q 'imagetools create --tag "${image}:latest"' "${ROOT}/deploy/server/ghcr.sh" || bad "latest must be moved with imagetools"

A="$(pin a)"
B="$(pin b)"
C="$(pin c)"

assert_ok "digest" require_digest "sha256:${A}"
assert_fail "short digest" require_digest "sha256:abc"
assert_fail "upper digest" require_digest "sha256:${A^^}"
assert_ok "sha tag" require_sha_tag "sha-abc1234"
assert_fail "latest tag" require_sha_tag "latest"
assert_fail "short sha tag" require_sha_tag "sha-abc"
assert_ok "ghcr prefix" require_ghcr_repo "ghcr.io/lhjnilsson"
assert_fail "ghcr image as prefix" require_ghcr_repo "ghcr.io/lhjnilsson/foreverbull"
assert_fail "docker hub prefix" require_ghcr_repo "docker.io/lhjnilsson"
assert_ok "ghcr image" require_ghcr_image "ghcr.io/lhjnilsson/foreverbull"
assert_ok "ghcr grafana image" require_ghcr_image "ghcr.io/lhjnilsson/fb-grafana"
assert_ok "ghcr zipline image" require_ghcr_image "ghcr.io/lhjnilsson/zipline"
assert_fail "ghcr prefix as image" require_ghcr_image "ghcr.io/lhjnilsson"
assert_fail "ghcr nested image" require_ghcr_image "ghcr.io/lhjnilsson/foreverbull/extra"
assert_fail "ghcr tagged image" require_ghcr_image "ghcr.io/lhjnilsson/foreverbull:sha-abc1234"
assert_fail "ghcr digest image" require_ghcr_image "ghcr.io/lhjnilsson/foreverbull@sha256:${A}"
assert_fail "docker hub image" require_ghcr_image "docker.io/lhjnilsson/foreverbull"
assert_fail "uppercase ghcr image" require_ghcr_image "ghcr.io/LhjNilsson/foreverbull"
assert_fail "ghcr image newline" require_ghcr_image $'ghcr.io/lhjnilsson/foreverbull\nlatest'
assert_ok "nightly namespace" require_release_namespace "fb-nightly"
assert_ok "e2e namespace" require_release_namespace "e2e-abc1234"
assert_fail "other namespace" require_release_namespace "kube-system"
assert_ok "grafana origin" require_grafana_url "https://nightly.foreverbull.com"
assert_fail "grafana http" require_grafana_url "http://nightly.foreverbull.com"
assert_fail "grafana path" require_grafana_url "https://nightly.foreverbull.com/d"

got="$(digest_from_ref "ghcr.io/example/foreverbull@sha256:${A}")"
[[ "${got}" == "sha256:${A}" ]] || bad "digest_from_ref printed ${got}"
assert_fail "tag ref" digest_from_ref "ghcr.io/example/foreverbull:sha-abc1234"

raw=$'apiVersion: v1\nclusters:\n- name: local\n'
b64="$(printf '%s' "${raw}" | base64 -w 0)"
norm="$(normalize_kubeconfig "${b64}")"
[[ "${norm}" == *"apiVersion:"* && "${norm}" == *"clusters:"* ]] || bad "base64 kubeconfig"
assert_fail "kubeconfig junk" normalize_kubeconfig "not-a-kubeconfig"

(
  export E2E_KUBECONFIG=x E2E_SSH_PRIVATE_KEY=x E2E_SSH_KNOWN_HOSTS=x E2E_SSH_HOST=x E2E_SSH_USER=x
  require_deploy_secrets
) || bad "secrets present"
(
  export E2E_KUBECONFIG=x E2E_SSH_PRIVATE_KEY=x E2E_SSH_KNOWN_HOSTS=x E2E_SSH_HOST=x
  unset E2E_SSH_USER
  require_deploy_secrets
) && bad "secrets missing user"
ok "deploy secrets"

values="$(mktemp)"
(
  export IMAGE_REPOSITORY=ghcr.io/example
  export FOREVERBULL_DIGEST="sha256:${A}"
  export ZIPLINE_DIGEST="sha256:${B}"
  export GRAFANA_DIGEST="sha256:${C}"
  export GRAFANA_URL=https://nightly.foreverbull.com
  write_pin_values "${values}"
)
grep -Fq "tag: sha256:${A}" "${values}" || bad "broker pin"
grep -Fq "url: https://nightly.foreverbull.com" "${values}" || bad "grafana url"
grep -Fq 'webUrl: ""' "${values}" || bad "web url empty"
grep -Fq "imageTag: pinned" "${values}" || bad "fallback tag"
if grep -q 'localhost' "${values}"; then
  bad "pin values include localhost"
fi
if grep -Eq 'latest|allowDefaults: true|hostPort: true' "${values}"; then
  bad "pin values include a moving tag or local defaults"
else
  ok "pin values"
fi
rm -f "${values}"
assert_ok "public web origin" bash -c "
  set -euo pipefail
  source '${ROOT}/deploy/server/lib.sh'
  export IMAGE_REPOSITORY=ghcr.io/example
  export FOREVERBULL_DIGEST='sha256:${A}'
  export ZIPLINE_DIGEST='sha256:${B}'
  export GRAFANA_DIGEST='sha256:${C}'
  export FOREVERBULL_WEB_URL=https://app.example.com
  values=\$(mktemp)
  write_pin_values \"\${values}\"
  grep -Fq 'webUrl: https://app.example.com' \"\${values}\"
"
assert_fail "web localhost" bash -c "
  set -euo pipefail
  source '${ROOT}/deploy/server/lib.sh'
  export IMAGE_REPOSITORY=ghcr.io/example
  export FOREVERBULL_DIGEST='sha256:${A}'
  export ZIPLINE_DIGEST='sha256:${B}'
  export GRAFANA_DIGEST='sha256:${C}'
  export FOREVERBULL_WEB_URL=http://localhost:8080
  write_pin_values /tmp/foreverbull-pin-rejected
"
web_err="$(
  bash -c "
    set -euo pipefail
    source '${ROOT}/deploy/server/lib.sh'
    export IMAGE_REPOSITORY=ghcr.io/example
    export FOREVERBULL_DIGEST='sha256:${A}'
    export ZIPLINE_DIGEST='sha256:${B}'
    export GRAFANA_DIGEST='sha256:${C}'
    export FOREVERBULL_WEB_URL=http://localhost:8080
    write_pin_values /tmp/foreverbull-pin-rejected
  " 2>&1 >/dev/null || true
)"
web_want="FOREVERBULL_WEB_URL isn't a public https address foreverbull can link to from Grafana. Set it to the foreverbull page's https address, not localhost, or leave it empty to hide the links. Nothing was changed."
[[ "${web_err}" == "${web_want}" ]] || bad "web url message: ${web_err}"
ok "web url message"
assert_fail "web dollar" bash -c "
  set -euo pipefail
  source '${ROOT}/deploy/server/lib.sh'
  export IMAGE_REPOSITORY=ghcr.io/example
  export FOREVERBULL_DIGEST='sha256:${A}'
  export ZIPLINE_DIGEST='sha256:${B}'
  export GRAFANA_DIGEST='sha256:${C}'
  export FOREVERBULL_WEB_URL='https://app.example.com/\$path'
  write_pin_values /tmp/foreverbull-pin-rejected
"
assert_fail "pin rejects bad repo" bash -c "
  set -euo pipefail
  source '${ROOT}/deploy/server/lib.sh'
  export IMAGE_REPOSITORY=\$'ghcr.io/example\nbroker:'
  export FOREVERBULL_DIGEST='sha256:${A}'
  export ZIPLINE_DIGEST='sha256:${B}'
  export GRAFANA_DIGEST='sha256:${C}'
  write_pin_values /tmp/foreverbull-pin-rejected
"

(
  export GITHUB_REPOSITORY_OWNER=LhjNilsson
  export GITHUB_SHA=0123456789abcdef0123456789abcdef01234567
  ref="$(bash "${ROOT}/deploy/server/image-ref.sh" foreverbull)"
  grep -qx 'IMAGE_REPOSITORY=ghcr.io/lhjnilsson' <<<"${ref}" || exit 1
  grep -qx 'SHORT_SHA=0123456' <<<"${ref}" || exit 1
  grep -qx 'SHA_TAG=sha-0123456' <<<"${ref}" || exit 1
  grep -qx 'IMAGE=ghcr.io/lhjnilsson/foreverbull' <<<"${ref}" || exit 1
) || bad "image ref"
ok "image ref"

# Workflow jobs pass IMAGE from image-ref.sh (ghcr.io/<owner>/<name>).
# A stub docker answers imagetools so the check stays offline. The command
# must get past argument checks and reach that stub. require_ghcr_repo still
# rejects a package path; that helper is the Helm prefix.
GHCR_STUB="$(mktemp -d)"
cat >"${GHCR_STUB}/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${DOCKER_LOG:?}"
if [[ "${1:-}" != "buildx" || "${2:-}" != "imagetools" ]]; then
  echo "unexpected docker invocation" >&2
  exit 1
fi
case "${3:-}" in
  inspect)
    if [[ "$*" == *"--format"* && "$*" == *":latest" ]]; then
      [[ -n "${STUB_DIGEST:-}" ]] || exit 1
      printf '%s\n' "${STUB_DIGEST}"
      exit 0
    fi
    echo "manifest unknown" >&2
    exit 1
    ;;
  create)
    exit 0
    ;;
  *)
    echo "unexpected imagetools command" >&2
    exit 1
    ;;
esac
EOF
chmod +x "${GHCR_STUB}/docker"
cat >"${GHCR_STUB}/cosign" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${COSIGN_LOG:?}"
if [[ "${COSIGN_FAIL:-}" == "1" ]]; then
  echo "stub refused to sign" >&2
  exit 1
fi
EOF
chmod +x "${GHCR_STUB}/cosign"
resolved="$(PATH="${GHCR_STUB}:${PATH}" command -v docker)"
[[ "${resolved}" == "${GHCR_STUB}/docker" ]] || bad "docker stub was not first on PATH"

GHCR_REVISION="0123456789abcdef0123456789abcdef01234567"
GHCR_SHA_TAG="sha-${GHCR_REVISION::7}"

run_ghcr() {
  local log="$1"
  shift
  : >"${log}"
  (
    export PATH="${GHCR_STUB}:${PATH}"
    export DOCKER_LOG="${log}"
    export COSIGN_LOG="${log}"
    export COSIGN_FAIL="${GHCR_COSIGN_FAIL:-}"
    export STUB_DIGEST="${GHCR_STUB_DIGEST:-}"
    bash "${ROOT}/deploy/server/ghcr.sh" "$@"
  )
}

expect_existing_absent() {
  local image="$1"
  local log out err got
  log="$(mktemp)"
  out="$(mktemp)"
  err="$(mktemp)"
  if ! run_ghcr "${log}" existing-digest "${image}" "${GHCR_SHA_TAG}" "${GHCR_REVISION}" >"${out}" 2>"${err}"; then
    bad "existing-digest ${image}: $(cat "${err}")"
    rm -f "${log}" "${out}" "${err}"
    return
  fi
  got="$(cat "${out}")"
  if [[ "${got}" != "absent" ]]; then
    bad "existing-digest ${image} printed ${got}"
  elif ! grep -F "imagetools inspect ${image}:${GHCR_SHA_TAG}" "${log}" >/dev/null; then
    bad "existing-digest ${image} did not reach imagetools"
  elif grep -F 'image must be ghcr.io/<owner> without a tag or digest' "${err}" >/dev/null; then
    bad "existing-digest ${image} rejected the package image as a registry prefix"
  else
    ok "existing-digest ${image}"
  fi
  rm -f "${log}" "${out}" "${err}"
}

expect_move_latest() {
  local image="$1"
  local digest="$2"
  local log out err
  log="$(mktemp)"
  out="$(mktemp)"
  err="$(mktemp)"
  GHCR_STUB_DIGEST="${digest}"
  if ! run_ghcr "${log}" move-latest "${image}" "${digest}" >"${out}" 2>"${err}"; then
    bad "move-latest ${image}: $(cat "${err}")"
    rm -f "${log}" "${out}" "${err}"
    return
  fi
  if ! grep -F "latest -> ${image}@${digest}" "${out}" >/dev/null; then
    bad "move-latest ${image} printed $(cat "${out}")"
  elif ! grep -F "imagetools create --tag ${image}:latest ${image}@${digest}" "${log}" >/dev/null; then
    bad "move-latest ${image} did not reach imagetools"
  else
    ok "move-latest ${image}"
  fi
  rm -f "${log}" "${out}" "${err}"
}

refuse_ghcr_before_docker() {
  local desc="$1"
  shift
  local log out err
  log="$(mktemp)"
  out="$(mktemp)"
  err="$(mktemp)"
  if run_ghcr "${log}" "$@" >"${out}" 2>"${err}"; then
    bad "${desc} was accepted"
  elif [[ -s "${log}" ]]; then
    bad "${desc} called docker before rejecting the arguments"
  else
    ok "${desc}"
  fi
  rm -f "${log}" "${out}" "${err}"
}

for name in foreverbull zipline fb-grafana; do
  expect_existing_absent "ghcr.io/lhjnilsson/${name}"
done
expect_move_latest "ghcr.io/lhjnilsson/foreverbull" "sha256:${A}"
refuse_ghcr_before_docker "existing-digest prefix" \
  existing-digest "ghcr.io/lhjnilsson" "${GHCR_SHA_TAG}" "${GHCR_REVISION}"
refuse_ghcr_before_docker "existing-digest tagged image" \
  existing-digest "ghcr.io/lhjnilsson/foreverbull:${GHCR_SHA_TAG}" "${GHCR_SHA_TAG}" "${GHCR_REVISION}"
refuse_ghcr_before_docker "existing-digest digest image" \
  existing-digest "ghcr.io/lhjnilsson/foreverbull@sha256:${A}" "${GHCR_SHA_TAG}" "${GHCR_REVISION}"
refuse_ghcr_before_docker "existing-digest other registry" \
  existing-digest "docker.io/lhjnilsson/foreverbull" "${GHCR_SHA_TAG}" "${GHCR_REVISION}"
refuse_ghcr_before_docker "existing-digest uppercase owner" \
  existing-digest "ghcr.io/LhjNilsson/foreverbull" "${GHCR_SHA_TAG}" "${GHCR_REVISION}"
refuse_ghcr_before_docker "existing-digest latest tag" \
  existing-digest "ghcr.io/lhjnilsson/foreverbull" "latest" "${GHCR_REVISION}"
refuse_ghcr_before_docker "existing-digest short revision" \
  existing-digest "ghcr.io/lhjnilsson/foreverbull" "${GHCR_SHA_TAG}" "0123456"
refuse_ghcr_before_docker "move-latest prefix" \
  move-latest "ghcr.io/lhjnilsson" "sha256:${A}"
refuse_ghcr_before_docker "move-latest tagged image" \
  move-latest "ghcr.io/lhjnilsson/foreverbull:latest" "sha256:${A}"
refuse_ghcr_before_docker "move-latest bad digest" \
  move-latest "ghcr.io/lhjnilsson/foreverbull" "sha256:abc"

expect_sign() {
  local image="$1"
  local digest="$2"
  local log out err
  log="$(mktemp)"
  out="$(mktemp)"
  err="$(mktemp)"
  unset GHCR_COSIGN_FAIL
  if ! run_ghcr "${log}" sign "${image}" "${digest}" >"${out}" 2>"${err}"; then
    bad "sign ${image}: $(cat "${err}")"
    rm -f "${log}" "${out}" "${err}"
    return
  fi
  if ! grep -qx "signed ${image}@${digest}" "${out}"; then
    bad "sign ${image} printed $(cat "${out}")"
  elif ! grep -qx "sign --yes ${image}@${digest}" "${log}"; then
    bad "sign ${image} did not call cosign with the digest"
  elif grep -F 'imagetools' "${log}" >/dev/null; then
    bad "sign ${image} called imagetools"
  else
    ok "sign ${image}"
  fi
  rm -f "${log}" "${out}" "${err}"
}

for name in foreverbull zipline fb-grafana; do
  expect_sign "ghcr.io/lhjnilsson/${name}" "sha256:${A}"
done
refuse_ghcr_before_docker "sign prefix" \
  sign "ghcr.io/lhjnilsson" "sha256:${A}"
refuse_ghcr_before_docker "sign tagged image" \
  sign "ghcr.io/lhjnilsson/foreverbull:${GHCR_SHA_TAG}" "sha256:${A}"
refuse_ghcr_before_docker "sign digest image" \
  sign "ghcr.io/lhjnilsson/foreverbull@sha256:${A}" "sha256:${A}"
refuse_ghcr_before_docker "sign other registry" \
  sign "docker.io/lhjnilsson/foreverbull" "sha256:${A}"
refuse_ghcr_before_docker "sign bad digest" \
  sign "ghcr.io/lhjnilsson/foreverbull" "sha256:abc"
(
  log="$(mktemp)"
  err="$(mktemp)"
  if PATH="/usr/bin:/bin" bash "${ROOT}/deploy/server/ghcr.sh" sign "ghcr.io/lhjnilsson/foreverbull" "sha256:${A}" >"${log}" 2>"${err}"; then
    bad "sign without cosign was accepted"
  elif grep -q 'cosign is not on PATH' "${err}"; then
    ok "sign without cosign"
  else
    bad "sign without cosign printed $(cat "${err}")"
  fi
  rm -f "${log}" "${err}"
)
(
  log="$(mktemp)"
  err="$(mktemp)"
  GHCR_COSIGN_FAIL=1
  if run_ghcr "${log}" sign "ghcr.io/lhjnilsson/foreverbull" "sha256:${A}" >"${log}" 2>"${err}"; then
    bad "failed cosign was accepted"
  elif grep -q 'could not sign ghcr.io/lhjnilsson/foreverbull@sha256:' "${err}" \
    && grep -q 'Delete that tag before re-running this commit' "${err}"; then
    ok "sign failure names the retry"
  else
    bad "sign failure printed $(cat "${err}")"
  fi
  unset GHCR_COSIGN_FAIL
  rm -f "${log}" "${err}"
)
rm -rf "${GHCR_STUB}"
unset GHCR_STUB_DIGEST GHCR_COSIGN_FAIL

(
  unset FOREVERBULL_DIGEST ZIPLINE_DIGEST GRAFANA_DIGEST
  [[ "$(classify_rollback_inputs)" == "previous" ]]
) || bad "empty rollback"
(
  export FOREVERBULL_DIGEST="sha256:${A}"
  export ZIPLINE_DIGEST="sha256:${B}"
  export GRAFANA_DIGEST="sha256:${C}"
  [[ "$(classify_rollback_inputs)" == "explicit" ]]
) || bad "explicit rollback"
(
  export FOREVERBULL_DIGEST="sha256:${A}"
  unset ZIPLINE_DIGEST GRAFANA_DIGEST
  classify_rollback_inputs
) && bad "partial rollback"
ok "rollback inputs"

rev="$(
  select_previous_revision <<'JSON'
[
  {"revision": 1, "status": "superseded"},
  {"revision": 2, "status": "deployed"}
]
JSON
)"
[[ "${rev}" == "1" ]] || bad "previous of deployed was ${rev}"
rev="$(
  select_previous_revision <<'JSON'
[
  {"revision": 1, "status": "deployed"},
  {"revision": 2, "status": "failed"}
]
JSON
)"
[[ "${rev}" == "1" ]] || bad "previous of failed was ${rev}"
rev="$(
  select_previous_revision <<'JSON'
[
  {"revision": 1, "status": "superseded"},
  {"revision": 2, "status": "deployed"},
  {"revision": 3, "status": "failed"}
]
JSON
)"
[[ "${rev}" == "2" ]] || bad "last good after a failed upgrade was ${rev}"
if select_previous_revision >/dev/null <<'JSON'
[{"revision": 1, "status": "deployed"}]
JSON
then
  bad "sole revision must be refused"
else
  ok "sole revision"
fi

manifest="$(mktemp)"
cat >"${manifest}" <<EOF
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: foreverbull
spec:
  template:
    spec:
      containers:
        - name: foreverbull
          image: ghcr.io/example/foreverbull@sha256:${A}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: postgres
spec:
  template:
    spec:
      containers:
        - name: postgres
          image: postgres:13.3-alpine
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: grafana
spec:
  template:
    spec:
      containers:
        - name: grafana
          image: "ghcr.io/example/fb-grafana@sha256:${B}"
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: zipline
spec:
  template:
    spec:
      containers:
        - name: zipline
          image: ghcr.io/example/zipline:sha-abc1234
EOF
parsed="$(parse_release_images <"${manifest}")"
rm -f "${manifest}"
grep -Fq "foreverbull ghcr.io/example/foreverbull@sha256:${A}" <<<"${parsed}" || bad "parse broker"
grep -Fq "grafana ghcr.io/example/fb-grafana@sha256:${B}" <<<"${parsed}" || bad "parse grafana quotes"
grep -Fq "zipline ghcr.io/example/zipline:sha-abc1234" <<<"${parsed}" || bad "parse zipline tag"
if grep -q 'postgres' <<<"${parsed}"; then
  bad "parse kept postgres"
else
  ok "parse release images"
fi

wf="${ROOT}/.github/workflows/e2e_server.yaml"
[[ -f "${wf}" ]] || bad "missing e2e_server workflow"
head_on="$(sed -n '1,45p' "${wf}")"
if grep -q 'pull_request:' <<<"${head_on}"; then
  bad "e2e_server must not run on pull_request"
else
  ok "trigger is not a pull request"
fi
grep -q -- '- main' <<<"${head_on}" || bad "trigger must include main"
if grep -E -q 'PYPI|DOCKERHUB|docker\.io|cron:' "${wf}"; then
  bad "e2e_server pulls in an out-of-scope publisher"
else
  ok "publisher scope"
fi
grep -q 'environment: e2e' "${wf}" || bad "workflow must use the e2e environment"
grep -q 'packages: write' "${wf}" || bad "publish jobs need packages:write"
grep -q 'fb-nightly' "${wf}" || bad "workflow must name fb-nightly"
grep -q 'image-ref.sh' "${wf}" || bad "workflow must name images through image-ref.sh"
grep -q 'existing-digest' "${wf}" || bad "workflow must reuse sha tags instead of overwriting them"
grep -q 'retarget.sh' "${wf}" || bad "workflow must move latest and the nightly release together"

sbom_n="$(grep -c 'sbom: true' "${wf}" || true)"
prov_n="$(grep -c 'provenance: mode=max' "${wf}" || true)"
token_n="$(grep -c 'id-token: write' "${wf}" || true)"
sign_n="$(grep -c 'ghcr.sh sign' "${wf}" || true)"
cosign_n="$(grep -c 'sigstore/cosign-installer@6f9f17788090df1f26f669e9d70d6ae9567deba6' "${wf}" || true)"
[[ "${sbom_n}" -eq 3 ]] || bad "publish jobs must set sbom: true (found ${sbom_n})"
[[ "${prov_n}" -eq 3 ]] || bad "publish jobs must set provenance mode=max (found ${prov_n})"
[[ "${token_n}" -eq 3 ]] || bad "publish jobs must request id-token: write (found ${token_n})"
[[ "${sign_n}" -eq 3 ]] || bad "publish jobs must sign through ghcr.sh (found ${sign_n})"
[[ "${cosign_n}" -eq 3 ]] || bad "cosign-installer must be pinned in each publish job (found ${cosign_n})"
gated=0
while IFS= read -r n; do
  start=$((n - 8))
  [[ "${start}" -lt 1 ]] && start=1
  window="$(sed -n "${start},$((n - 1))p" "${wf}")"
  if grep -q "steps.existing.outputs.state == 'absent'" <<<"${window}" \
    && grep -q 'steps.push.outputs.digest' <<<"${window}"; then
    gated=$((gated + 1))
  else
    bad "sign step at line ${n} is not limited to a newly pushed digest"
  fi
done < <(grep -n 'ghcr.sh sign' "${wf}" | cut -d: -f1)
[[ "${gated}" -eq 3 ]] || bad "expected 3 gated sign steps, found ${gated}"
ok "publish signs a new index only"

if [[ "${failed}" -ne 0 ]]; then
  echo "server release self-test failed" >&2
  exit 1
fi
echo "server release self-test passed"
