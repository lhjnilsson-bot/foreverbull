#!/usr/bin/env bash
# Checks product-e2e disk cleanup: toolchain list, image preservation,
# tarball deletion only after the local tags exist, and workflow order.
# Uses a fake docker and a DISK_ROOT. Does not touch the host Docker daemon.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "${here}/../.." && pwd)"
script="${here}/product_e2e_disk.sh"
workflow="${root}/.github/workflows/on_commit.yaml"
values="${root}/deploy/helm/foreverbull/values.yaml"
original_path="${PATH}"
failures=0

ok() {
  echo "ok   $1"
}

bad() {
  echo "FAIL $1" >&2
  failures=$((failures + 1))
}

# Run in a subshell so set -e inside a case is not cancelled by `if`.
assert_ok() {
  local desc="$1"
  shift
  if (set -euo pipefail; "$@"); then
    ok "${desc}"
  else
    bad "${desc}"
  fi
}

# shellcheck disable=SC1090
source "${script}"

exact_lines() {
  local want="$1"
  local got="$2"
  if [[ "${want}" == "${got}" ]]; then
    return 0
  fi
  echo "want:" >&2
  printf '%s\n' "${want}" >&2
  echo "got:" >&2
  printf '%s\n' "${got}" >&2
  return 1
}

order_contains() {
  local file="$1"
  shift
  local prev=0 line needle
  for needle in "$@"; do
    line="$(grep -n -F -m 1 -- "${needle}" "${file}" | cut -d: -f1 || true)"
    if [[ -z "${line}" ]]; then
      echo "missing ${needle}" >&2
      return 1
    fi
    if [[ "${line}" -le "${prev}" ]]; then
      echo "out of order ${needle} at ${line} after ${prev}" >&2
      return 1
    fi
    prev="${line}"
  done
}

write_fake_docker() {
  local bin="$1"
  cat >"${bin}" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
state="${DOCKER_STATE:?}"
printf '%s\n' "$*" >>"${state}/commands.log"
cmd="$1"
shift
images="${state}/images.tsv"
containers="${state}/containers.tsv"
touch "${images}" "${containers}"

id_used_by_container() {
  local id="$1"
  awk -F '\t' -v id="${id}" '$2 == id { found = 1 } END { exit found ? 0 : 1 }' "${containers}"
}

case "${cmd}" in
  system)
    [[ "${1:-}" == "df" ]]
    echo "TYPE            TOTAL     ACTIVE    SIZE      RECLAIMABLE"
    ;;
  ps)
    [[ "${1:-}" == "-aq" && "${2:-}" == "--no-trunc" ]]
    awk -F '\t' '{ print $1 }' "${containers}"
    ;;
  inspect)
    [[ "${1:-}" == "--format" && "${2:-}" == "{{.Image}}" && -n "${3:-}" ]]
    awk -F '\t' -v cid="$3" '$1 == cid { print $2; found = 1 } END { exit found ? 0 : 1 }' "${containers}"
    ;;
  rmi)
    [[ -n "${1:-}" ]]
    if [[ -f "${state}/rmi-fail" ]]; then
      echo "rmi failed: $1" >&2
      exit 1
    fi
    ref="$1"
    tag="${ref##*:}"
    repo="${ref%:*}"
    tmp="$(mktemp)"
    awk -F '\t' -v repo="${repo}" -v tag="${tag}" 'BEGIN { OFS = "\t" } !($2 == repo && $3 == tag) { print }' \
      "${images}" >"${tmp}"
    if cmp -s "${images}" "${tmp}"; then
      echo "rmi: image not found: ${ref}" >&2
      rm -f "${tmp}"
      exit 1
    fi
    mv "${tmp}" "${images}"
    ;;
  image)
    sub="${1:-}"
    shift || true
    case "${sub}" in
      ls)
        [[ "${1:-}" == "--no-trunc" && "${2:-}" == "--format" ]]
        [[ "$3" == "$(printf '%s\t%s\t%s' '{{.ID}}' '{{.Repository}}' '{{.Tag}}')" ]]
        cat "${images}"
        ;;
      prune)
        for arg in "$@"; do
          if [[ "${arg}" == "-a" || "${arg}" == "--all" || "${arg}" == "system" ]]; then
            echo "refusing broad prune: ${arg}" >&2
            exit 1
          fi
        done
        [[ "${1:-}" == "--force" ]]
        tmp="$(mktemp)"
        while IFS=$'\t' read -r id repo tag || [[ -n "${id}" ]]; do
          [[ -z "${id}" ]] && continue
          if [[ "${repo}" == "<none>" || "${tag}" == "<none>" ]]; then
            if id_used_by_container "${id}"; then
              printf '%s\t%s\t%s\n' "${id}" "${repo}" "${tag}" >>"${tmp}"
            fi
            continue
          fi
          printf '%s\t%s\t%s\n' "${id}" "${repo}" "${tag}" >>"${tmp}"
        done <"${images}"
        mv "${tmp}" "${images}"
        ;;
      inspect)
        ref="${1:?}"
        if [[ "${ref}" == sha256:* ]]; then
          awk -F '\t' -v id="${ref}" '$1 == id { found = 1 } END { exit found ? 0 : 1 }' "${images}" >/dev/null
        else
          tag="${ref##*:}"
          repo="${ref%:*}"
          awk -F '\t' -v repo="${repo}" -v tag="${tag}" '$2 == repo && $3 == tag { found = 1 } END { exit found ? 0 : 1 }' \
            "${images}" >/dev/null
        fi
        ;;
      *)
        echo "unexpected docker image ${sub}" >&2
        exit 1
        ;;
    esac
    ;;
  *)
    echo "unexpected docker ${cmd}" >&2
    exit 1
    ;;
esac
EOF
  chmod 755 "${bin}"
}

write_fake_sudo() {
  local bin="$1"
  cat >"${bin}" <<'EOF'
#!/usr/bin/env bash
echo "sudo invoked: $*" >&2
exit 99
EOF
  chmod 755 "${bin}"
}

write_fake_rm() {
  local bin="$1"
  local real
  real="$(command -v rm)"
  cat >"${bin}" <<EOF
#!/usr/bin/env bash
for arg in "\$@"; do
  case "\${arg}" in
    *"/usr/local/lib/android"*)
      echo "rm refused \${arg}" >&2
      exit 1
      ;;
  esac
done
exec ${real} "\$@"
EOF
  chmod 755 "${bin}"
}

image_row() {
  local file="$1" id="$2" repo="$3" tag="$4"
  printf '%s\t%s\t%s\n' "${id}" "${repo}" "${tag}" >>"${file}"
}

container_row() {
  local file="$1" cid="$2" id="$3"
  printf '%s\t%s\n' "${cid}" "${id}" >>"${file}"
}

has_image() {
  local file="$1" id="$2" repo="$3" tag="$4"
  awk -F '\t' -v id="${id}" -v repo="${repo}" -v tag="${tag}" \
    '$1 == id && $2 == repo && $3 == tag { found = 1 } END { exit found ? 0 : 1 }' "${file}"
}

seed_images() {
  local file="$1"
  : >"${file}"
  image_row "${file}" sha256:fb ghcr.io/lhjnilsson/foreverbull local
  image_row "${file}" sha256:fb ghcr.io/lhjnilsson/foreverbull on_commit
  image_row "${file}" sha256:zip ghcr.io/lhjnilsson/zipline local
  image_row "${file}" sha256:graf ghcr.io/lhjnilsson/fb-grafana local
  image_row "${file}" sha256:kind kindest/node v1.35.0
  image_row "${file}" sha256:pg postgres 13.3-alpine
  image_row "${file}" sha256:nats nats 2.10-alpine
  image_row "${file}" sha256:silo pgsty/silo RELEASE.2026-09-16T00-00-00Z
  image_row "${file}" sha256:node node 20
  image_row "${file}" sha256:ubuntu ubuntu 24.04
  image_row "${file}" sha256:build moby/buildkit latest
  image_row "${file}" sha256:dangle "<none>" "<none>"
  image_row "${file}" sha256:held "<none>" "<none>"
}

seed_containers() {
  local file="$1"
  : >"${file}"
  container_row "${file}" cid-buildkit sha256:build
  container_row "${file}" cid-held sha256:held
}

make_tree() {
  local base="$1"
  mkdir -p \
    "${base}/usr/local/lib/android/sdk" \
    "${base}/usr/share/dotnet" \
    "${base}/opt/ghc" \
    "${base}/usr/local/.ghcup" \
    "${base}/usr/share/swift" \
    "${base}/opt/hostedtoolcache/CodeQL" \
    "${base}/opt/hostedtoolcache/Python/3.12.0/x64" \
    "${base}/usr/local/bin" \
    "${base}/usr/bin" \
    "${base}/var/lib/docker/volumes/kind" \
    "${base}/var/lib/containerd" \
    "${base}/tmp/images" \
    "${base}/home/runner"
  echo sdk >"${base}/usr/local/lib/android/sdk/package.xml"
  echo dotnet >"${base}/usr/share/dotnet/dotnet"
  echo ghc >"${base}/opt/ghc/ghc"
  echo ghcup >"${base}/usr/local/.ghcup/ghcup"
  echo swift >"${base}/usr/share/swift/swift"
  echo codeql >"${base}/opt/hostedtoolcache/CodeQL/codeql"
  echo python >"${base}/opt/hostedtoolcache/Python/3.12.0/x64/python"
  echo kind >"${base}/usr/local/bin/kind"
  echo docker >"${base}/usr/bin/docker"
  echo vol >"${base}/var/lib/docker/volumes/kind/data"
  echo containerd >"${base}/var/lib/containerd/keeper"
  echo foreverbull >"${base}/tmp/images/foreverbull.tar"
  echo zipline >"${base}/tmp/images/zipline.tar"
  echo grafana >"${base}/tmp/images/fb_grafana.tar"
  echo work >"${base}/home/runner/work"
}

new_case() {
  local tmp bin
  tmp="$(mktemp -d)"
  mkdir -p "${tmp}/bin" "${tmp}/state" "${tmp}/root"
  write_fake_docker "${tmp}/bin/docker"
  write_fake_sudo "${tmp}/bin/sudo"
  : >"${tmp}/state/commands.log"
  CASE_TMP="${tmp}"
  export DISK_ROOT="${tmp}/root"
  export DOCKER="${tmp}/bin/docker"
  export DOCKER_STATE="${tmp}/state"
  export PATH="${tmp}/bin:${original_path}"
  bin="$(command -v rm)"
  if [[ "${bin}" == "${tmp}/bin/rm" ]]; then
    echo "PATH still points at a previous fake rm" >&2
    exit 1
  fi
}

run_free() {
  bash "${script}" free >"${CASE_TMP}/stdout" 2>"${CASE_TMP}/stderr"
}

check_contract() {
  local want got
  want="$(printf '%s\n' \
    /usr/local/lib/android \
    /usr/share/dotnet \
    /opt/ghc \
    /usr/local/.ghcup \
    /usr/share/swift \
    /opt/hostedtoolcache/CodeQL)"
  got="$(toolchain_paths)"
  exact_lines "${want}" "${got}"

  want="$(printf '%s\n' \
    ghcr.io/lhjnilsson/foreverbull \
    ghcr.io/lhjnilsson/zipline \
    ghcr.io/lhjnilsson/fb-grafana \
    kindest/node \
    postgres \
    nats \
    pgsty/silo)"
  got="$(preserved_repositories)"
  exact_lines "${want}" "${got}"

  want="$(printf '%s\n' \
    ghcr.io/lhjnilsson/foreverbull:local \
    ghcr.io/lhjnilsson/zipline:local \
    ghcr.io/lhjnilsson/fb-grafana:local)"
  got="$(local_image_refs)"
  exact_lines "${want}" "${got}"

  want="$(printf '%s\n' \
    foreverbull.tar \
    zipline.tar \
    fb_grafana.tar)"
  got="$(artifact_tarballs)"
  exact_lines "${want}" "${got}"
}

check_script_rejects_broad_cleanup() {
  local forbidden
  if grep -n -E '\|\|[[:space:]]*true|\|\|[[:space:]]*:|set \+e|system prune|docker[[:space:]]+volume|prune[[:space:]]+--all|prune[[:space:]]+-a|rmi[[:space:]]+-f|rmi[[:space:]]+--force' "${script}"; then
    echo "cleanup script contains a broad or hidden removal" >&2
    return 1
  fi
  for forbidden in /usr/local/bin /usr/bin /opt/hostedtoolcache/Python /var/lib/docker /var/lib/containerd /tmp/images /home; do
    if toolchain_paths | grep -qx "${forbidden}"; then
      echo "toolchain list includes ${forbidden}" >&2
      return 1
    fi
  done
}

check_chart_and_workflow_names() {
  local job dests loads
  grep -q 'imageRepository: ghcr.io/lhjnilsson' "${values}"
  grep -q 'repository: postgres$' "${values}"
  grep -q 'repository: nats$' "${values}"
  grep -q 'repository: pgsty/silo$' "${values}"

  job="$(awk '
    $0 == "  product-e2e:" { capture = 1; next }
    capture && /^  [^ ]/ { exit }
    capture { print }
  ' "${workflow}")"
  dests="$(printf '%s\n' "${job}" | awk '/docker tag / { print $NF }' | sort)"
  loads="$(printf '%s\n' "${job}" | awk '/docker load -i \/tmp\/images\// { sub(".*/", "", $NF); print $NF }' | sort)"
  exact_lines "$(local_image_refs | sort)" "${dests}"
  exact_lines "$(artifact_tarballs | sort)" "${loads}"
}

check_workflow_order() {
  local jobfile
  jobfile="$(mktemp)"
  awk '
    $0 == "  product-e2e:" { capture = 1; next }
    capture && /^  [^ ]/ { exit }
    capture { print }
  ' "${workflow}" >"${jobfile}"
  order_contains "${jobfile}" \
    "bash .github/scripts/test_product_e2e_disk.sh" \
    "bash .github/scripts/product_e2e_disk.sh free" \
    "docker load -i /tmp/images/foreverbull.tar" \
    "docker tag ghcr.io/lhjnilsson/foreverbull:on_commit ghcr.io/lhjnilsson/foreverbull:local" \
    "bash .github/scripts/product_e2e_disk.sh drop-tarballs" \
    "uv run pytest ../tests/end_to_end/test_demo.py -vs"
  if grep -n 'product_e2e_disk.sh' "${jobfile}" | grep -E '\|\|'; then
    echo "cleanup command has a fallback" >&2
    rm -f "${jobfile}"
    return 1
  fi
  if grep -R -n 'product_e2e_disk.sh' "${root}/.github/workflows" --exclude on_commit.yaml; then
    echo "disk cleanup is referenced outside product-e2e" >&2
    rm -f "${jobfile}"
    return 1
  fi
  rm -f "${jobfile}"
}

check_free_preserves_images() {
  local base images out
  new_case
  base="${DISK_ROOT}"
  images="${DOCKER_STATE}/images.tsv"
  make_tree "${base}"
  seed_images "${images}"
  seed_containers "${DOCKER_STATE}/containers.tsv"
  # One toolchain is not on this fixture. That must be reported, not fail.
  rm -rf "${base}/usr/share/swift"
  run_free
  out="${CASE_TMP}/stdout"
  order_contains "${out}" \
    "disk before avail_bytes=" \
    "removing toolchain /usr/local/lib/android" \
    "absent toolchain /usr/share/swift" \
    "docker before" \
    "kept image ghcr.io/lhjnilsson/foreverbull:local" \
    "kept image ghcr.io/lhjnilsson/foreverbull:on_commit" \
    "kept image kindest/node:v1.35.0" \
    "kept image postgres:13.3-alpine" \
    "kept image nats:2.10-alpine" \
    "kept image pgsty/silo:RELEASE.2026-09-16T00-00-00Z" \
    "kept image moby/buildkit:latest" \
    "removing image node:20" \
    "removing image ubuntu:24.04" \
    "docker after" \
    "disk after avail_bytes="
  [[ ! -e "${base}/usr/local/lib/android" ]]
  [[ ! -e "${base}/usr/share/dotnet" ]]
  [[ ! -e "${base}/opt/ghc" ]]
  [[ ! -e "${base}/usr/local/.ghcup" ]]
  [[ ! -e "${base}/opt/hostedtoolcache/CodeQL" ]]
  [[ -f "${base}/opt/hostedtoolcache/Python/3.12.0/x64/python" ]]
  [[ -f "${base}/usr/local/bin/kind" ]]
  [[ -f "${base}/usr/bin/docker" ]]
  [[ -f "${base}/var/lib/docker/volumes/kind/data" ]]
  [[ -f "${base}/var/lib/containerd/keeper" ]]
  [[ -f "${base}/tmp/images/foreverbull.tar" ]]
  [[ -f "${base}/tmp/images/zipline.tar" ]]
  [[ -f "${base}/tmp/images/fb_grafana.tar" ]]
  [[ -f "${base}/home/runner/work" ]]
  has_image "${images}" sha256:fb ghcr.io/lhjnilsson/foreverbull local
  has_image "${images}" sha256:fb ghcr.io/lhjnilsson/foreverbull on_commit
  has_image "${images}" sha256:zip ghcr.io/lhjnilsson/zipline local
  has_image "${images}" sha256:graf ghcr.io/lhjnilsson/fb-grafana local
  has_image "${images}" sha256:kind kindest/node v1.35.0
  has_image "${images}" sha256:pg postgres 13.3-alpine
  has_image "${images}" sha256:nats nats 2.10-alpine
  has_image "${images}" sha256:silo pgsty/silo RELEASE.2026-09-16T00-00-00Z
  has_image "${images}" sha256:build moby/buildkit latest
  has_image "${images}" sha256:held "<none>" "<none>"
  if has_image "${images}" sha256:node node 20; then
    echo "node image was kept" >&2
    return 1
  fi
  if has_image "${images}" sha256:ubuntu ubuntu 24.04; then
    echo "ubuntu image was kept" >&2
    return 1
  fi
  if has_image "${images}" sha256:dangle "<none>" "<none>"; then
    echo "dangling image was kept" >&2
    return 1
  fi
  if grep -E 'volume|system prune|prune --all|prune -a' "${DOCKER_STATE}/commands.log"; then
    echo "cleanup invoked a broad docker removal" >&2
    return 1
  fi
  if [[ -s "${CASE_TMP}/stderr" ]] && grep -q 'sudo invoked' "${CASE_TMP}/stderr"; then
    echo "sudo ran under DISK_ROOT" >&2
    return 1
  fi
}

check_rm_failure_stops() {
  local images
  new_case
  make_tree "${DISK_ROOT}"
  images="${DOCKER_STATE}/images.tsv"
  seed_images "${images}"
  seed_containers "${DOCKER_STATE}/containers.tsv"
  write_fake_rm "${CASE_TMP}/bin/rm"
  export PATH="${CASE_TMP}/bin:${PATH}"
  if run_free; then
    echo "free succeeded after rm failed" >&2
    return 1
  fi
  if grep -q 'disk after avail_bytes=' "${CASE_TMP}/stdout"; then
    echo "free reported completion after rm failed" >&2
    return 1
  fi
  if grep -q 'removing image ' "${CASE_TMP}/stdout"; then
    echo "image removal ran after toolchain removal failed" >&2
    return 1
  fi
  has_image "${images}" sha256:fb ghcr.io/lhjnilsson/foreverbull local
  has_image "${images}" sha256:node node 20
  [[ -d "${DISK_ROOT}/usr/local/lib/android" ]]
}

check_rmi_failure_stops() {
  local images
  new_case
  make_tree "${DISK_ROOT}"
  images="${DOCKER_STATE}/images.tsv"
  seed_images "${images}"
  seed_containers "${DOCKER_STATE}/containers.tsv"
  : >"${DOCKER_STATE}/rmi-fail"
  if run_free; then
    echo "free succeeded after docker rmi failed" >&2
    return 1
  fi
  if grep -q 'disk after avail_bytes=' "${CASE_TMP}/stdout"; then
    echo "free reported completion after docker rmi failed" >&2
    return 1
  fi
  has_image "${images}" sha256:fb ghcr.io/lhjnilsson/foreverbull local
  has_image "${images}" sha256:fb ghcr.io/lhjnilsson/foreverbull on_commit
  has_image "${images}" sha256:kind kindest/node v1.35.0
  has_image "${images}" sha256:build moby/buildkit latest
}

check_drop_tarballs() {
  local base
  new_case
  base="${DISK_ROOT}"
  make_tree "${base}"
  seed_images "${DOCKER_STATE}/images.tsv"
  seed_containers "${DOCKER_STATE}/containers.tsv"
  bash "${script}" drop-tarballs >"${CASE_TMP}/stdout" 2>"${CASE_TMP}/stderr"
  order_contains "${CASE_TMP}/stdout" \
    "disk before avail_bytes=" \
    "required image present ghcr.io/lhjnilsson/foreverbull:local" \
    "required image present ghcr.io/lhjnilsson/zipline:local" \
    "required image present ghcr.io/lhjnilsson/fb-grafana:local" \
    "removing tarball ${base}/tmp/images/foreverbull.tar" \
    "disk after avail_bytes="
  [[ ! -e "${base}/tmp/images/foreverbull.tar" ]]
  [[ ! -e "${base}/tmp/images/zipline.tar" ]]
  [[ ! -e "${base}/tmp/images/fb_grafana.tar" ]]
  [[ -d "${base}/usr/local/lib/android" ]]
  [[ -f "${base}/var/lib/docker/volumes/kind/data" ]]
  has_image "${DOCKER_STATE}/images.tsv" sha256:fb ghcr.io/lhjnilsson/foreverbull local
  has_image "${DOCKER_STATE}/images.tsv" sha256:zip ghcr.io/lhjnilsson/zipline local
  has_image "${DOCKER_STATE}/images.tsv" sha256:graf ghcr.io/lhjnilsson/fb-grafana local
  if grep -q ' rmi ' "${DOCKER_STATE}/commands.log"; then
    echo "drop-tarballs removed a docker image" >&2
    return 1
  fi
}

check_drop_refuses_missing_image() {
  local base
  new_case
  base="${DISK_ROOT}"
  make_tree "${base}"
  seed_images "${DOCKER_STATE}/images.tsv"
  awk -F '\t' 'BEGIN { OFS = "\t" } !($1 == "sha256:zip") { print }' \
    "${DOCKER_STATE}/images.tsv" >"${CASE_TMP}/images.next"
  mv "${CASE_TMP}/images.next" "${DOCKER_STATE}/images.tsv"
  if bash "${script}" drop-tarballs >"${CASE_TMP}/stdout" 2>"${CASE_TMP}/stderr"; then
    echo "drop-tarballs deleted tarballs without zipline" >&2
    return 1
  fi
  grep -q 'required image is not loaded: ghcr.io/lhjnilsson/zipline:local' "${CASE_TMP}/stderr"
  [[ -f "${base}/tmp/images/foreverbull.tar" ]]
  [[ -f "${base}/tmp/images/zipline.tar" ]]
  [[ -f "${base}/tmp/images/fb_grafana.tar" ]]
  if grep -q 'removing tarball ' "${CASE_TMP}/stdout"; then
    echo "a tarball was removed without the image" >&2
    return 1
  fi
}

check_drop_refuses_missing_tarball() {
  local base
  new_case
  base="${DISK_ROOT}"
  make_tree "${base}"
  seed_images "${DOCKER_STATE}/images.tsv"
  rm -f "${base}/tmp/images/fb_grafana.tar"
  if bash "${script}" drop-tarballs >"${CASE_TMP}/stdout" 2>"${CASE_TMP}/stderr"; then
    echo "drop-tarballs succeeded with a tarball missing" >&2
    return 1
  fi
  grep -q 'missing image tarball:' "${CASE_TMP}/stderr"
  [[ -f "${base}/tmp/images/foreverbull.tar" ]]
  [[ -f "${base}/tmp/images/zipline.tar" ]]
  if grep -q 'removing tarball ' "${CASE_TMP}/stdout"; then
    echo "a tarball was removed when another was missing" >&2
    return 1
  fi
  has_image "${DOCKER_STATE}/images.tsv" sha256:graf ghcr.io/lhjnilsson/fb-grafana local
}

check_usage() {
  local status=0
  new_case
  bash "${script}" >"${CASE_TMP}/stdout" 2>"${CASE_TMP}/stderr" || status=$?
  [[ "${status}" -eq 2 ]]
  status=0
  bash "${script}" prune >"${CASE_TMP}/stdout" 2>"${CASE_TMP}/stderr" || status=$?
  [[ "${status}" -eq 2 ]]
}

bash -n "${script}"
bash -n "${here}/test_product_e2e_disk.sh"
ok "bash -n"

assert_ok "cleanup contract" check_contract
assert_ok "no broad or hidden removal" check_script_rejects_broad_cleanup
assert_ok "chart images and retag destinations" check_chart_and_workflow_names
assert_ok "workflow order before kind load" check_workflow_order
assert_ok "free keeps required images and kind storage" check_free_preserves_images
assert_ok "toolchain removal failure stops the step" check_rm_failure_stops
assert_ok "docker rmi failure stops the step" check_rmi_failure_stops
assert_ok "tarballs drop only after local tags exist" check_drop_tarballs
assert_ok "missing local image keeps the tarballs" check_drop_refuses_missing_image
assert_ok "missing tarball deletes nothing" check_drop_refuses_missing_tarball
assert_ok "unknown command exits 2" check_usage

if [[ "${failures}" -ne 0 ]]; then
  echo "${failures} case(s) failed" >&2
  exit 1
fi
echo "product-e2e disk cleanup checks passed"
