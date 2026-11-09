#!/usr/bin/env bash
# Free disk on the product-e2e runner before Kind loads images.
#
# `kind load docker-image` runs `docker save` of the three :local tags and
# writes that tarball under /tmp. The artifact tarballs are still on the same
# disk, and the hosted runner also ships toolchains this job never runs.
# `free` removes those toolchains and Docker images this job does not load.
# `drop-tarballs` deletes the artifact tarballs only after the three :local
# tags inspect. Kind's node, its volumes, and the chart images are not removed.
#
# A missing toolchain directory is logged and skipped: runner images do not
# all ship the same trees. A failed removal, a missing required image, or a
# missing artifact tarball exits non-zero. There is no fallback that continues
# into Kind after that.
#
# DISK_ROOT, when set, prefixes toolchain and tarball paths so tests can run
# without touching the machine. DOCKER, when set, is the docker executable.

# Lists below are the contract. test_product_e2e_disk.sh compares them with
# the workflow and the chart.

toolchain_paths() {
  printf '%s\n' \
    /usr/local/lib/android \
    /usr/share/dotnet \
    /opt/ghc \
    /usr/local/.ghcup \
    /usr/share/swift \
    /opt/hostedtoolcache/CodeQL
}

# Repositories kind load or the chart still need. Any tag of these stays.
# A container's image stays even when its repository is not in this list,
# so a running buildkit or a Kind node is not removed by image id.
preserved_repositories() {
  printf '%s\n' \
    ghcr.io/lhjnilsson/foreverbull \
    ghcr.io/lhjnilsson/zipline \
    ghcr.io/lhjnilsson/fb-grafana \
    kindest/node \
    postgres \
    nats \
    pgsty/silo
}

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

df_target() {
  local root
  root="$(disk_root)"
  if [[ -n "${root}" ]]; then
    printf '%s\n' "${root}"
  else
    printf '%s\n' /
  fi
}

using_sudo() {
  [[ -z "$(disk_root)" && "$(id -u)" -ne 0 ]]
}

docker_bin() {
  printf '%s\n' "${DOCKER:-docker}"
}

require_docker() {
  local bin
  bin="$(docker_bin)"
  if [[ -x "${bin}" ]]; then
    return 0
  fi
  if command -v "${bin}" >/dev/null 2>&1; then
    return 0
  fi
  echo "docker is not available: ${bin}" >&2
  exit 1
}

run_docker() {
  local bin
  bin="$(docker_bin)"
  "${bin}" "$@"
}

normalize_id() {
  local id="$1"
  id="${id//$'\r'/}"
  id="${id#sha256:}"
  printf '%s\n' "${id}"
}

log_disk() {
  local label="$1"
  local path avail
  path="$(df_target)"
  avail="$(df -B1 -P "${path}" | awk 'NR==2 {print $4}')"
  if [[ -z "${avail}" ]]; then
    echo "disk ${label}: df did not report available bytes for ${path}" >&2
    exit 1
  fi
  echo "disk ${label} avail_bytes=${avail} path=${path}"
  df -hP "${path}"
}

refuse_toolchain_path() {
  local rel="$1"
  case "${rel}" in
    /*) ;;
    *)
      echo "refusing toolchain path: ${rel}" >&2
      exit 1
      ;;
  esac
  case "${rel}" in
    *..*)
      echo "refusing toolchain path: ${rel}" >&2
      exit 1
      ;;
  esac
  case "${rel}" in
    /|/usr|/usr/bin|/usr/local|/usr/local/bin|/usr/share|/opt|/opt/hostedtoolcache|/var|/var/lib|/var/lib/docker|/var/lib/containerd|/tmp|/tmp/images|/home)
      echo "refusing toolchain path: ${rel}" >&2
      exit 1
      ;;
  esac
}

remove_toolchain() {
  local rel="$1"
  local path
  refuse_toolchain_path "${rel}"
  path="$(disk_path "${rel}")"
  if [[ ! -e "${path}" && ! -L "${path}" ]]; then
    echo "absent toolchain ${rel}"
    return 0
  fi
  echo "removing toolchain ${rel}"
  # Same privilege as the removal. A root-owned tree can fail du for the
  # runner user; that must not skip the delete or hide the error.
  if using_sudo; then
    sudo du -sh -- "${path}" || exit 1
    sudo rm -rf -- "${path}" || exit 1
  else
    du -sh -- "${path}" || exit 1
    rm -rf -- "${path}" || exit 1
  fi
  if [[ -e "${path}" || -L "${path}" ]]; then
    echo "toolchain still present: ${rel}" >&2
    exit 1
  fi
}

repo_preserved() {
  local repo="$1"
  local item
  while IFS= read -r item; do
    if [[ "${item}" == "${repo}" ]]; then
      return 0
    fi
  done < <(preserved_repositories)
  return 1
}

remove_unused_images() {
  local ps_file images_file
  local cid image_id repo tag key ref
  declare -A keep_ids=()
  local -a keep_keys=()
  local -a remove_refs=()

  require_docker
  echo "docker before"
  run_docker system df

  ps_file="$(mktemp)"
  images_file="$(mktemp)"
  run_docker ps -aq --no-trunc >"${ps_file}"
  while IFS= read -r cid || [[ -n "${cid}" ]]; do
    cid="${cid//$'\r'/}"
    [[ -z "${cid}" ]] && continue
    image_id="$(run_docker inspect --format '{{.Image}}' "${cid}")"
    key="$(normalize_id "${image_id}")"
    keep_ids["${key}"]=1
  done <"${ps_file}"

  run_docker image ls --no-trunc --format $'{{.ID}}\t{{.Repository}}\t{{.Tag}}' >"${images_file}"
  while IFS= read -r line || [[ -n "${line}" ]]; do
    line="${line//$'\r'/}"
    [[ -z "${line}" ]] && continue
    IFS=$'\t' read -r image_id repo tag <<<"${line}"
    if [[ -z "${image_id}" || -z "${repo}" || -z "${tag}" ]]; then
      echo "unexpected docker image list line: ${line}" >&2
      exit 1
    fi
    key="$(normalize_id "${image_id}")"
    if repo_preserved "${repo}" || [[ -n "${keep_ids[${key}]:-}" ]]; then
      keep_ids["${key}"]=1
    fi
  done <"${images_file}"

  while IFS= read -r line || [[ -n "${line}" ]]; do
    line="${line//$'\r'/}"
    [[ -z "${line}" ]] && continue
    IFS=$'\t' read -r image_id repo tag <<<"${line}"
    key="$(normalize_id "${image_id}")"
    if [[ -n "${keep_ids[${key}]:-}" ]]; then
      if [[ "${repo}" != "<none>" && "${tag}" != "<none>" ]]; then
        echo "kept image ${repo}:${tag}"
      else
        echo "kept image id sha256:${key}"
      fi
      continue
    fi
    if [[ "${repo}" == "<none>" || "${tag}" == "<none>" ]]; then
      continue
    fi
    remove_refs+=("${repo}:${tag}")
  done <"${images_file}"

  for key in "${!keep_ids[@]}"; do
    keep_keys+=("${key}")
  done

  if ((${#remove_refs[@]} > 0)); then
    for ref in "${remove_refs[@]}"; do
      echo "removing image ${ref}"
      run_docker rmi "${ref}" || exit 1
    done
  fi

  # Dangling images only. --all would delete tagged images Kind has not
  # started yet, including the three :local tags.
  run_docker image prune --force || exit 1

  for key in "${keep_keys[@]}"; do
    run_docker image inspect "sha256:${key}" >/dev/null || exit 1
  done

  echo "docker after"
  run_docker system df
  rm -f -- "${ps_file}" "${images_file}"
}

cmd_free() {
  local rel
  log_disk before
  while IFS= read -r rel; do
    [[ -z "${rel}" ]] && continue
    remove_toolchain "${rel}"
  done < <(toolchain_paths)
  remove_unused_images
  log_disk after
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
  log_disk before
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
  log_disk after
}

usage() {
  echo "usage: product_e2e_disk.sh free|drop-tarballs" >&2
  exit 2
}

case "${1:-}" in
  free) cmd_free ;;
  drop-tarballs) cmd_drop_tarballs ;;
  *) usage ;;
esac
