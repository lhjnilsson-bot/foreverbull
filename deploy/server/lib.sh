#!/usr/bin/env bash
# Shared checks for the server release scripts. Sourced, not executed.
if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  echo "source deploy/server/lib.sh; it is not a command" >&2
  exit 1
fi

die() {
  echo "deploy/server: $*" >&2
  exit 1
}

require_digest() {
  [[ "${1:-}" =~ ^sha256:[0-9a-f]{64}$ ]]
}

require_sha_tag() {
  [[ "${1:-}" =~ ^sha-[0-9a-f]{7,40}$ ]]
}

require_ghcr_repo() {
  # Registry prefix only: ghcr.io/<owner>. The chart appends /foreverbull, /zipline, /fb-grafana.
  [[ "${1:-}" =~ ^ghcr\.io/[a-z0-9]+([._-][a-z0-9]+)*$ ]]
}

require_ghcr_image() {
  # Package image ghcr.io/<owner>/<name>. No tag and no digest: callers append them.
  # Owner rules stay in require_ghcr_repo, so a Helm prefix cannot grow a package path.
  local image="${1:-}"
  local name="${image##*/}"
  local repo="${image%/*}"
  [[ "${image}" == */* ]] || return 1
  [[ "${name}" =~ ^[a-z0-9]+([._-][a-z0-9]+)*$ ]] || return 1
  require_ghcr_repo "${repo}"
}

require_namespace() {
  local ns="${1:-}"
  [[ ${#ns} -le 63 && "$ns" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]]
}

require_release_namespace() {
  local ns="${1:-}"
  [[ "$ns" == "fb-nightly" || "$ns" =~ ^e2e-[0-9a-f]{7,40}$ ]]
}

require_helm_release() {
  [[ "${1:-}" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]]
}

require_ssh_user() {
  [[ "${1:-}" =~ ^[A-Za-z0-9._-]+$ ]]
}

require_ssh_host() {
  [[ "${1:-}" =~ ^[A-Za-z0-9.-]+$ ]]
}

require_port() {
  local port="${1:-}"
  [[ "$port" =~ ^[0-9]+$ ]] && [[ "$port" -ge 1 && "$port" -le 65535 ]]
}

require_wait_timeout() {
  [[ "${1:-}" =~ ^[0-9]+[smh]$ ]]
}

require_grafana_url() {
  [[ "${1:-}" =~ ^https://[A-Za-z0-9.-]+$ ]]
}

# A public foreverbull page. localhost and a dollar sign are refused:
# Grafana would interpolate $, and a loopback address is not a public origin.
# write_pin_values calls this before it writes the values file. install.sh
# calls write_pin_values before helm or kubectl change the cluster.
require_public_web_url() {
  local url="${1:-}"
  local host
  if [[ "$url" == *'$'* || "$url" == *'@'* ]]; then
    refuse_public_web_url
    return 1
  fi
  if [[ ! "$url" =~ ^https://[A-Za-z0-9.-]+(:[0-9]+)?(/[A-Za-z0-9._~:/?#&=+-]*)?$ ]]; then
    refuse_public_web_url
    return 1
  fi
  host="${url#https://}"
  host="${host%%/*}"
  host="${host%%:*}"
  if [[ "$host" == "localhost" || "$host" == "127.0.0.1" ]]; then
    refuse_public_web_url
    return 1
  fi
}

refuse_public_web_url() {
  echo "FOREVERBULL_WEB_URL isn't a public https address foreverbull can link to from Grafana. Set it to the foreverbull page's https address, not localhost, or leave it empty to hide the links. Nothing was changed." >&2
}

kubeconfig_text_ok() {
  [[ "${1:-}" == *"apiVersion:"* && "${1:-}" == *"clusters:"* ]]
}

# Prints kubeconfig YAML. Accepts the raw file or a single base64 blob of that file.
normalize_kubeconfig() {
  local text="${1:-}"
  local decoded
  if kubeconfig_text_ok "$text"; then
    printf '%s\n' "$text"
    return 0
  fi
  decoded="$(printf '%s' "$text" | tr -d '[:space:]' | base64 -d 2>/dev/null || true)"
  if kubeconfig_text_ok "$decoded"; then
    printf '%s\n' "$decoded"
    return 0
  fi
  return 1
}

# The five values the e2e environment must hold before any publish or deploy.
require_deploy_secrets() {
  local missing=""
  local name
  for name in E2E_KUBECONFIG E2E_SSH_PRIVATE_KEY E2E_SSH_KNOWN_HOSTS E2E_SSH_HOST E2E_SSH_USER; do
    if [[ -z "${!name:-}" ]]; then
      missing="${missing} ${name}"
    fi
  done
  if [[ -n "$missing" ]]; then
    echo "e2e environment is missing:${missing}. Nothing was published or deployed." >&2
    return 1
  fi
}

# Prints the digest when REF is repository@sha256:…, otherwise returns 1.
digest_from_ref() {
  local ref="${1:-}"
  if [[ "$ref" =~ @(sha256:[0-9a-f]{64})$ ]]; then
    printf '%s\n' "${BASH_REMATCH[1]}"
    return 0
  fi
  return 1
}

image_suffix_for_deploy() {
  case "${1:-}" in
    foreverbull) printf 'foreverbull\n' ;;
    zipline) printf 'zipline\n' ;;
    grafana) printf 'fb-grafana\n' ;;
    *) return 1 ;;
  esac
}

# Helm values for one release. Digests are per image; global.imageTag is not a
# moving tag, so a missed override cannot follow latest.
write_pin_values() {
  local path="$1"
  local grafana=""
  local web_url=""
  require_ghcr_repo "${IMAGE_REPOSITORY:-}" || return 1
  require_digest "${FOREVERBULL_DIGEST:-}" || return 1
  require_digest "${ZIPLINE_DIGEST:-}" || return 1
  require_digest "${GRAFANA_DIGEST:-}" || return 1
  if [[ -n "${GRAFANA_URL:-}" ]]; then
    require_grafana_url "${GRAFANA_URL}" || return 1
    grafana="${GRAFANA_URL}"
  fi
  if [[ -n "${FOREVERBULL_WEB_URL:-}" ]]; then
    require_public_web_url "${FOREVERBULL_WEB_URL}" || return 1
    web_url="${FOREVERBULL_WEB_URL}"
  fi
  {
    echo "global:"
    echo "  imageRepository: ${IMAGE_REPOSITORY}"
    echo "  imageTag: pinned"
    echo "  imagePullPolicy: IfNotPresent"
    echo "images:"
    echo "  broker:"
    echo "    tag: ${FOREVERBULL_DIGEST}"
    echo "  zipline:"
    echo "    tag: ${ZIPLINE_DIGEST}"
    echo "  grafana:"
    echo "    tag: ${GRAFANA_DIGEST}"
    echo "expose:"
    echo "  hostPort: false"
    echo "replicaCount: 1"
    echo "broker:"
    echo "  trading:"
    echo "    enabled: false"
    echo "grafana:"
    if [[ -n "$grafana" ]]; then
      echo "  url: ${grafana}"
    fi
    if [[ -n "$web_url" ]]; then
      echo "  webUrl: ${web_url}"
    else
      echo '  webUrl: ""'
    fi
  } >"$path"
}

# Reads helm history JSON on stdin. Prints the revision to roll back to.
# A failed newest revision restores the last good one. A deployed newest
# revision restores the one before it.
select_previous_revision() {
  jq -r '
    def bad:
      .status == "failed"
      or .status == "pending-install"
      or .status == "pending-upgrade"
      or .status == "pending-rollback"
      or .status == "uninstalling"
      or .status == "uninstalled";
    def newest_failed:
      .status == "failed"
      or .status == "pending-install"
      or .status == "pending-upgrade"
      or .status == "pending-rollback";
    if type != "array" or length == 0 then
      error("helm history is empty")
    else
      .
    end
    | (max_by(.revision)) as $latest
    | (map(select(bad | not)) | sort_by(.revision)) as $good
    | if ($latest | newest_failed) then
        if ($good | length) < 1 then
          error("no revision to restore")
        else
          $good[-1].revision
        end
      elif ($good | length) < 2 then
        error("no previous revision")
      else
        $good[-2].revision
      end
  '
}

# Reads `helm get manifest` on stdin. Prints "<deployment> <image>" for the
# three app deployments. Postgres, NATS, and Minio are ignored.
parse_release_images() {
  awk '
    function trimq(s) {
      gsub(/^"/, "", s)
      gsub(/"$/, "", s)
      return s
    }
    BEGIN { kind = ""; meta = 0; dep = "" }
    /^---[[:space:]]*$/ { kind = ""; meta = 0; dep = ""; next }
    /^kind:[[:space:]]+/ { kind = $2; meta = 0; dep = ""; next }
    /^metadata:[[:space:]]*$/ { if (kind == "Deployment") meta = 1; next }
    /^spec:[[:space:]]*$/ { meta = 0; next }
    meta && /^  name:[[:space:]]+/ {
      dep = $2
      gsub(/"/, "", dep)
      next
    }
    dep ~ /^(foreverbull|zipline|grafana)$/ && /^[[:space:]]+image:[[:space:]]+/ {
      if (!(dep in seen)) {
        img = trimq($2)
        seen[dep] = img
        print dep, img
      }
    }
  '
}

# Prints "previous" when all three digest inputs are empty, or "explicit"
# when all three are valid digests. A partial set is refused.
classify_rollback_inputs() {
  local n=0
  [[ -n "${FOREVERBULL_DIGEST:-}" ]] && n=$((n + 1))
  [[ -n "${ZIPLINE_DIGEST:-}" ]] && n=$((n + 1))
  [[ -n "${GRAFANA_DIGEST:-}" ]] && n=$((n + 1))
  if [[ "$n" -eq 0 ]]; then
    printf 'previous\n'
    return 0
  fi
  if [[ "$n" -ne 3 ]]; then
    echo "set all three digests, or leave all three empty" >&2
    return 1
  fi
  require_digest "${FOREVERBULL_DIGEST}" || return 1
  require_digest "${ZIPLINE_DIGEST}" || return 1
  require_digest "${GRAFANA_DIGEST}" || return 1
  printf 'explicit\n'
  return 0
}
