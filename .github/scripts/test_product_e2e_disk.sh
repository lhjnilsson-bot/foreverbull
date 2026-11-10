#!/usr/bin/env bash
# Checks product-e2e tarball deletion and host/Kind disk diagnostics.
# Uses a fake docker and a fake df. Does not touch the host Docker daemon.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "${here}/../.." && pwd)"
script="${here}/product_e2e_disk.sh"
workflow="${root}/.github/workflows/on_commit.yaml"
failures=0

ok() {
  echo "ok   $1"
}

bad() {
  echo "FAIL $1" >&2
  failures=$((failures + 1))
}

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
    line="$(awk -v start="${prev}" -v needle="${needle}" 'NR > start && index($0, needle) { print NR; exit }' "${file}")"
    if [[ -z "${line}" ]]; then
      echo "missing ${needle} after line ${prev}" >&2
      return 1
    fi
    prev="${line}"
  done
}

write_fake_df() {
  local bin="$1"
  local mode="${2:-ok}"
  cat >"${bin}" <<EOF
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "\$*" >>"${CASE_TMP}/df.log"
if [[ "${mode}" == fail ]]; then
  echo "df refused \$*" >&2
  exit 1
fi
path="\${*: -1}"
if [[ "\$*" == *"-iP"* ]]; then
  printf '%s\n' "Filesystem     Inodes  IUsed  IFree IUse% Mounted on"
  printf '%s\n' "/dev/fake       10000   1000   9000   10% \${path}"
elif [[ "\$*" == *"-B1"* ]]; then
  printf '%s\n' "Filesystem     1B-blocks      Used Available Capacity Mounted on"
  printf '%s\n' "/dev/fake       100000     10000     90000      10% \${path}"
else
  printf '%s\n' "Filesystem      Size  Used Avail Use% Mounted on"
  printf '%s\n' "/dev/fake       145G   57G   88G  40% \${path}"
fi
EOF
  chmod 755 "${bin}"
}

write_fake_docker() {
  local bin="$1"
  cat >"${bin}" <<EOF
#!/usr/bin/env bash
set -euo pipefail
state="${CASE_TMP}/docker-state"
mkdir -p "\${state}"
printf '%s\n' "\$*" >>"\${state}/commands.log"
cmd="\$1"
shift
case "\${cmd}" in
  inspect)
    [[ "\${1:-}" == "${KIND_NODE}" ]]
    [[ -f "\${state}/node-present" ]]
    ;;
  exec)
    [[ "\${1:-}" == "${KIND_NODE}" ]]
    shift
    if [[ -f "\${state}/exec-fail" ]]; then
      echo "exec df failed" >&2
      exit 1
    fi
    "${CASE_TMP}/bin/df" "\$@"
    ;;
  image)
    [[ "\${1:-}" == inspect ]]
    ref="\${2:?}"
    grep -qxF "\${ref}" "\${state}/images" 
    ;;
  *)
    echo "unexpected docker \${cmd}" >&2
    exit 1
    ;;
esac
EOF
  chmod 755 "${bin}"
}

new_case() {
  local tmp
  tmp="$(mktemp -d)"
  mkdir -p "${tmp}/bin" "${tmp}/root/tmp/images" "${tmp}/docker-state"
  CASE_TMP="${tmp}"
  export DISK_ROOT="${tmp}/root"
  export DOCKER="${tmp}/bin/docker"
  export DF="${tmp}/bin/df"
  export KIND_NODE="foreverbull-control-plane"
  export PATH="${tmp}/bin:${PATH}"
  write_fake_df "${tmp}/bin/df" ok
  write_fake_docker "${tmp}/bin/docker"
  printf '%s\n' \
    ghcr.io/lhjnilsson/foreverbull:local \
    ghcr.io/lhjnilsson/zipline:local \
    ghcr.io/lhjnilsson/fb-grafana:local >"${tmp}/docker-state/images"
  printf 'x\n' >"${tmp}/root/tmp/images/foreverbull.tar"
  printf 'x\n' >"${tmp}/root/tmp/images/zipline.tar"
  printf 'x\n' >"${tmp}/root/tmp/images/fb_grafana.tar"
  printf 'keep\n' >"${tmp}/root/usr-local-lib-android"
  mkdir -p "${tmp}/root/usr/local/lib/android"
  printf 'sdk\n' >"${tmp}/root/usr/local/lib/android/sdk"
}

check_contract() {
  local want got
  want="$(printf '%s\n' \
    ghcr.io/lhjnilsson/foreverbull:local \
    ghcr.io/lhjnilsson/zipline:local \
    ghcr.io/lhjnilsson/fb-grafana:local)"
  got="$(local_image_refs)"
  exact_lines "${want}" "${got}"
  want="$(printf '%s\n' foreverbull.tar zipline.tar fb_grafana.tar)"
  got="$(artifact_tarballs)"
  exact_lines "${want}" "${got}"
  want="$(printf '%s\n' / /var/lib/containerd)"
  got="$(kind_node_paths)"
  exact_lines "${want}" "${got}"
}

check_no_broad_cleanup() {
  if grep -n -E 'toolchain|android|dotnet|ghcup|CodeQL|docker[[:space:]]+rmi|image prune|system prune|docker[[:space:]]+volume|\|\|[[:space:]]*true|set \+e' "${script}"; then
    echo "cleanup script still has broad removal or a hidden failure" >&2
    return 1
  fi
  if grep -n 'product_e2e_disk.sh free' "${workflow}"; then
    echo "workflow still reclaims hosted toolchains" >&2
    return 1
  fi
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
    "docker load -i /tmp/images/foreverbull.tar" \
    "docker tag ghcr.io/lhjnilsson/foreverbull:on_commit ghcr.io/lhjnilsson/foreverbull:local" \
    "bash .github/scripts/product_e2e_disk.sh drop-tarballs" \
    "bash .github/scripts/product_e2e_disk.sh host" \
    "uv run pytest ../tests/end_to_end/test_demo.py -vs" \
    "bash .github/scripts/product_e2e_disk.sh kind-node"
  grep -q 'id: kind-backtest' "${jobfile}"
  grep -q "steps.kind-backtest.outcome == 'failure'" "${jobfile}"
  if grep -n 'product_e2e_disk.sh' "${jobfile}" | grep -E '\|\|'; then
    echo "disk command has a fallback" >&2
    rm -f "${jobfile}"
    return 1
  fi
  if grep -R -n 'product_e2e_disk.sh' "${root}/.github/workflows" --exclude on_commit.yaml; then
    echo "disk script is referenced outside product-e2e" >&2
    rm -f "${jobfile}"
    return 1
  fi
  local job dests loads
  job="$(cat "${jobfile}")"
  dests="$(printf '%s\n' "${job}" | awk '/docker tag / { print $NF }' | sort)"
  loads="$(printf '%s\n' "${job}" | awk '/docker load -i \/tmp\/images\// { sub(".*/", "", $NF); print $NF }' | sort)"
  exact_lines "$(local_image_refs | sort)" "${dests}"
  exact_lines "$(artifact_tarballs | sort)" "${loads}"
  rm -f "${jobfile}"
}

check_drop_tarballs() {
  local base out
  new_case
  base="${DISK_ROOT}"
  bash "${script}" drop-tarballs >"${CASE_TMP}/stdout" 2>"${CASE_TMP}/stderr"
  out="${CASE_TMP}/stdout"
  order_contains "${out}" \
    "host disk avail_bytes=90000 avail_inodes=9000 path=/" \
    "required image present ghcr.io/lhjnilsson/foreverbull:local" \
    "required image present ghcr.io/lhjnilsson/zipline:local" \
    "required image present ghcr.io/lhjnilsson/fb-grafana:local" \
    "removing tarball ${base}/tmp/images/foreverbull.tar" \
    "removing tarball ${base}/tmp/images/zipline.tar" \
    "removing tarball ${base}/tmp/images/fb_grafana.tar" \
    "host disk avail_bytes=90000 avail_inodes=9000 path=/"
  [[ ! -e "${base}/tmp/images/foreverbull.tar" ]]
  [[ ! -e "${base}/tmp/images/zipline.tar" ]]
  [[ ! -e "${base}/tmp/images/fb_grafana.tar" ]]
  [[ -f "${base}/usr/local/lib/android/sdk" ]]
  if grep -q 'kind-node disk' "${out}"; then
    echo "tarball deletion printed a Kind node measurement" >&2
    return 1
  fi
  if grep -E 'rmi|prune|volume' "${CASE_TMP}/docker-state/commands.log"; then
    echo "tarball deletion removed docker data" >&2
    return 1
  fi
}

check_drop_refuses_missing_image() {
  local base
  new_case
  base="${DISK_ROOT}"
  printf '%s\n' \
    ghcr.io/lhjnilsson/foreverbull:local \
    ghcr.io/lhjnilsson/fb-grafana:local >"${CASE_TMP}/docker-state/images"
  if bash "${script}" drop-tarballs >"${CASE_TMP}/stdout" 2>"${CASE_TMP}/stderr"; then
    echo "drop-tarballs deleted tarballs without zipline" >&2
    return 1
  fi
  grep -q 'required image is not loaded: ghcr.io/lhjnilsson/zipline:local' "${CASE_TMP}/stderr"
  [[ -f "${base}/tmp/images/foreverbull.tar" ]]
  [[ -f "${base}/tmp/images/zipline.tar" ]]
  [[ -f "${base}/tmp/images/fb_grafana.tar" ]]
  if grep -q 'removing tarball ' "${CASE_TMP}/stdout"; then
    echo "a tarball was removed without every local tag" >&2
    return 1
  fi
  grep -q 'host disk avail_bytes=90000 avail_inodes=9000 path=/' "${CASE_TMP}/stdout"
}

check_drop_refuses_missing_tarball() {
  local base
  new_case
  base="${DISK_ROOT}"
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
}

check_host_disk() {
  new_case
  bash "${script}" host >"${CASE_TMP}/stdout" 2>"${CASE_TMP}/stderr"
  grep -q 'host disk table path=/' "${CASE_TMP}/stdout"
  grep -q 'host disk avail_bytes=90000 avail_inodes=9000 path=/' "${CASE_TMP}/stdout"
  if grep -q 'kind-node disk' "${CASE_TMP}/stdout"; then
    echo "host measurement was labeled as the Kind node" >&2
    return 1
  fi
}

check_host_df_failure() {
  new_case
  write_fake_df "${CASE_TMP}/bin/df" fail
  if bash "${script}" host >"${CASE_TMP}/stdout" 2>"${CASE_TMP}/stderr"; then
    echo "host disk succeeded after df failed" >&2
    return 1
  fi
  grep -q 'host disk: df failed for /' "${CASE_TMP}/stderr"
  if grep -q 'avail_bytes=' "${CASE_TMP}/stdout"; then
    echo "host disk reported capacity after df failed" >&2
    return 1
  fi
}

check_kind_node() {
  new_case
  : >"${CASE_TMP}/docker-state/node-present"
  # A host df invocation must not satisfy the node measurement.
  write_fake_df "${CASE_TMP}/bin/df" fail
  # exec calls this df. Replace it with one that succeeds only for exec by
  # letting the docker fake call df, so give docker its own success df and
  # leave PATH df failing.
  write_fake_df "${CASE_TMP}/bin/node-df" ok
  # Recreate docker so exec uses node-df, while DF stays the failing host df.
  cat >"${CASE_TMP}/bin/docker" <<EOF
#!/usr/bin/env bash
set -euo pipefail
state="${CASE_TMP}/docker-state"
printf '%s\n' "\$*" >>"\${state}/commands.log"
cmd="\$1"
shift
case "\${cmd}" in
  inspect)
    [[ "\${1:-}" == "${KIND_NODE}" ]]
    [[ -f "\${state}/node-present" ]]
    ;;
  exec)
    [[ "\${1:-}" == "${KIND_NODE}" ]]
    shift
    "${CASE_TMP}/bin/node-df" "\$@"
    ;;
  *)
    echo "unexpected docker \${cmd}" >&2
    exit 1
    ;;
esac
EOF
  chmod 755 "${CASE_TMP}/bin/docker"
  bash "${script}" kind-node >"${CASE_TMP}/stdout" 2>"${CASE_TMP}/stderr"
  grep -q 'kind-node disk avail_bytes=90000 avail_inodes=9000 node=foreverbull-control-plane path=/' "${CASE_TMP}/stdout"
  grep -q 'kind-node disk avail_bytes=90000 avail_inodes=9000 node=foreverbull-control-plane path=/var/lib/containerd' "${CASE_TMP}/stdout"
  if grep -q 'host disk' "${CASE_TMP}/stdout" "${CASE_TMP}/stderr"; then
    echo "kind-node measurement used the host label" >&2
    return 1
  fi
  if grep -q 'df refused' "${CASE_TMP}/stderr"; then
    echo "kind-node called the host df" >&2
    return 1
  fi
}

check_kind_node_missing() {
  new_case
  write_fake_df "${CASE_TMP}/bin/df" fail
  if bash "${script}" kind-node >"${CASE_TMP}/stdout" 2>"${CASE_TMP}/stderr"; then
    echo "kind-node succeeded without a container" >&2
    return 1
  fi
  grep -q 'kind-node disk unavailable: foreverbull-control-plane is not a local container' "${CASE_TMP}/stderr"
  if grep -q 'avail_bytes=' "${CASE_TMP}/stdout"; then
    echo "missing node still reported capacity" >&2
    return 1
  fi
  if grep -q 'host disk' "${CASE_TMP}/stdout"; then
    echo "missing node fell back to the host measurement" >&2
    return 1
  fi
}

check_kind_node_df_failure() {
  new_case
  : >"${CASE_TMP}/docker-state/node-present"
  : >"${CASE_TMP}/docker-state/exec-fail"
  write_fake_df "${CASE_TMP}/bin/df" fail
  if bash "${script}" kind-node >"${CASE_TMP}/stdout" 2>"${CASE_TMP}/stderr"; then
    echo "kind-node succeeded after df failed in the node" >&2
    return 1
  fi
  grep -q 'kind-node disk: df failed node=foreverbull-control-plane path=/' "${CASE_TMP}/stderr"
  if grep -q 'avail_bytes=' "${CASE_TMP}/stdout"; then
    echo "failed node df still reported capacity" >&2
    return 1
  fi
  if grep -q 'host disk' "${CASE_TMP}/stdout"; then
    echo "failed node df fell back to the host measurement" >&2
    return 1
  fi
}

check_usage() {
  local status=0
  new_case
  bash "${script}" >"${CASE_TMP}/stdout" 2>"${CASE_TMP}/stderr" || status=$?
  [[ "${status}" -eq 2 ]]
  status=0
  bash "${script}" free >"${CASE_TMP}/stdout" 2>"${CASE_TMP}/stderr" || status=$?
  [[ "${status}" -eq 2 ]]
}

bash -n "${script}"
bash -n "${here}/test_product_e2e_disk.sh"
ok "bash -n"

assert_ok "image and tarball contract" check_contract
assert_ok "no toolchain wipe or image prune" check_no_broad_cleanup
assert_ok "workflow order around kind load" check_workflow_order
assert_ok "tarballs drop only after every local tag" check_drop_tarballs
assert_ok "missing local image keeps the tarballs" check_drop_refuses_missing_image
assert_ok "missing tarball deletes nothing" check_drop_refuses_missing_tarball
assert_ok "host disk is labeled as the host" check_host_disk
assert_ok "host df failure stays visible" check_host_df_failure
assert_ok "kind node disk is measured inside the node" check_kind_node
assert_ok "missing kind node is not a host measurement" check_kind_node_missing
assert_ok "kind node df failure stays visible" check_kind_node_df_failure
assert_ok "removed free command exits 2" check_usage

if [[ "${failures}" -ne 0 ]]; then
  echo "${failures} case(s) failed" >&2
  exit 1
fi
echo "product-e2e disk checks passed"
