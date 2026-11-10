#!/usr/bin/env bash
# Disk notes for the product-e2e job.
#
# Kind loads the three :local tags by saving them on the host and importing
# that stream inside the Kind node. The downloaded artifact tarballs are a
# second copy of those images. drop-tarballs deletes only those files, and
# only after every :local tag inspects. It does not remove runner packages,
# images, or Kind data.
#
# host prints capacity and inodes for the runner filesystem. kind-node prints
# capacity and inodes from df inside the control-plane container. A missing
# node or a failed df exits non-zero and does not substitute the other
# measurement.
#
# DISK_ROOT prefixes tarball paths in tests. DF and DOCKER select those
# executables. KIND_NODE is the control-plane container name.

local_image_refs() {
  printf '%s\n' \
    ghcr.io/lhjnilsson/foreverbull:local \
    ghcr.io/lhjnilsson/zipline:local \
    ghcr.io/lhjnilsson/fb-grafana:local
}

artifact_tarballs() {
  printf '%s\n' \
    foreverbull.tar \
    zipline.tar \
    fb_grafana.tar
}

kind_node_paths() {
  printf '%s\n' \
    / \
    /var/lib/containerd
}

if [[ "${BASH_SOURCE[0]}" != "${0}" ]]; then
  return 0
fi

set -euo pipefail

disk_root() {
  local root="${DISK_ROOT:-}"
  root="${root%/}"
  if [[ -n "${root}" && "${root}" != /* ]]; then
    echo "DISK_ROOT must be an absolute path" >&2
    exit 1
  fi
  printf '%s\n' "${root}"
}

disk_path() {
  local rel="$1"
  local root
  root="$(disk_root)"
  case "${rel}" in
    /*) ;;
    *)
      echo "path must be absolute: ${rel}" >&2
      exit 1
      ;;
  esac
  if [[ -n "${root}" ]]; then
    printf '%s%s\n' "${root}" "${rel}"
  else
    printf '%s\n' "${rel}"
  fi
}

df_bin() {
  printf '%s\n' "${DF:-df}"
}

docker_bin() {
  printf '%s\n' "${DOCKER:-docker}"
}

kind_node_name() {
  printf '%s\n' "${KIND_NODE:-foreverbull-control-plane}"
}

require_docker() {
  local bin
  bin="$(docker_bin)"
  if [[ -x "${bin}" ]] || command -v "${bin}" >/dev/null 2>&1; then
    return 0
  fi
  echo "docker is not available: ${bin}" >&2
  exit 1
}

run_docker() {
  "$(docker_bin)" "$@"
}

# Print one df table and return the free-count field. scope and path name
# the filesystem the command actually queried.
df_free() {
  local scope="$1"
  local path="$2"
  shift 2
  local bin table free
  bin="$(df_bin)"
  if ! table="$("${bin}" "$@" "${path}")"; then
    echo "${scope} disk: df failed for ${path}" >&2
    exit 1
  fi
  printf '%s\n' "${table}"
  free="$(awk 'NR==2 { print $4 }' <<<"${table}")"
  if [[ ! "${free}" =~ ^[0-9]+$ ]]; then
    echo "${scope} disk: df did not report a free count for ${path}" >&2
    exit 1
  fi
  printf '%s\n' "${free}"
}

# df_free prints the table and then the count. Split them.
read_free() {
  local output="$1"
  printf '%s\n' "${output}" | head -n -1
  FREE_COUNT="$(printf '%s\n' "${output}" | tail -n 1)"
}

measure() {
  local scope="$1"
  local path="$2"
  local bytes_out inode_out
  local avail_bytes avail_inodes
  echo "${scope} disk table path=${path}"
  bytes_out="$(df_free "${scope}" "${path}" -B1 -P)"
  read_free "${bytes_out}"
  avail_bytes="${FREE_COUNT}"
  inode_out="$(df_free "${scope}" "${path}" -iP)"
  read_free "${inode_out}"
  avail_inodes="${FREE_COUNT}"
  df_bin_h="$(df_bin)"
  "${df_bin_h}" -hP "${path}" || exit 1
  echo "${scope} disk avail_bytes=${avail_bytes} avail_inodes=${avail_inodes} path=${path}"
}

measure_node() {
  local node="$1"
  local path="$2"
  local bytes_out inode_out human
  local avail_bytes avail_inodes
  echo "kind-node disk table node=${node} path=${path}"
  if ! bytes_out="$(run_docker exec "${node}" df -B1 -P "${path}")"; then
    echo "kind-node disk: df failed node=${node} path=${path}" >&2
    exit 1
  fi
  printf '%s\n' "${bytes_out}"
  avail_bytes="$(awk 'NR==2 { print $4 }' <<<"${bytes_out}")"
  if [[ ! "${avail_bytes}" =~ ^[0-9]+$ ]]; then
    echo "kind-node disk: df did not report available bytes node=${node} path=${path}" >&2
    exit 1
  fi
  if ! inode_out="$(run_docker exec "${node}" df -iP "${path}")"; then
    echo "kind-node disk: inode df failed node=${node} path=${path}" >&2
    exit 1
  fi
  printf '%s\n' "${inode_out}"
  avail_inodes="$(awk 'NR==2 { print $4 }' <<<"${inode_out}")"
  if [[ ! "${avail_inodes}" =~ ^[0-9]+$ ]]; then
    echo "kind-node disk: df did not report free inodes node=${node} path=${path}" >&2
    exit 1
  fi
  if ! human="$(run_docker exec "${node}" df -hP "${path}")"; then
    echo "kind-node disk: human df failed node=${node} path=${path}" >&2
    exit 1
  fi
  printf '%s\n' "${human}"
  echo "kind-node disk avail_bytes=${avail_bytes} avail_inodes=${avail_inodes} node=${node} path=${path}"
}

cmd_host() {
  measure host /
}

cmd_kind_node() {
  local node path
  node="$(kind_node_name)"
  require_docker
  if ! run_docker inspect "${node}" >/dev/null; then
    echo "kind-node disk unavailable: ${node} is not a local container" >&2
    exit 1
  fi
  while IFS= read -r path; do
    [[ -z "${path}" ]] && continue
    measure_node "${node}" "${path}"
  done < <(kind_node_paths)
}

require_local_images() {
  local image
  require_docker
  while IFS= read -r image; do
    [[ -z "${image}" ]] && continue
    if ! run_docker image inspect "${image}" >/dev/null; then
      echo "required image is not loaded: ${image}" >&2
      exit 1
    fi
    echo "required image present ${image}"
  done < <(local_image_refs)
}

cmd_drop_tarballs() {
  local name path
  local -a paths=()
  cmd_host
  require_local_images
  while IFS= read -r name; do
    [[ -z "${name}" ]] && continue
    path="$(disk_path "/tmp/images/${name}")"
    if [[ ! -f "${path}" ]]; then
      echo "missing image tarball: ${path}" >&2
      exit 1
    fi
    paths+=("${path}")
  done < <(artifact_tarballs)
  for path in "${paths[@]}"; do
    echo "removing tarball ${path}"
    rm -f -- "${path}" || exit 1
    if [[ -e "${path}" ]]; then
      echo "tarball still present: ${path}" >&2
      exit 1
    fi
  done
  require_local_images
  cmd_host
}

usage() {
  echo "usage: product_e2e_disk.sh host|kind-node|drop-tarballs" >&2
  exit 2
}

case "${1:-}" in
  host) cmd_host ;;
  kind-node) cmd_kind_node ;;
  drop-tarballs) cmd_drop_tarballs ;;
  *) usage ;;
esac
