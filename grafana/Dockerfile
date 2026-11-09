# syntax=docker/dockerfile:1
# Plugin go.mod requires Go 1.27.3. The replace directive points at the repo
# module two directories above the plugin, passed as the foreverbull build
# context.
#
# js-builder and go-builder stay on the host architecture (BUILDPLATFORM).
# The frontend is JavaScript, so one npm build is copied into every target.
# Mage cross-compiles only that target's linux binary (CGO_ENABLED=0). An
# arm64 image does not run npm or mage under QEMU. Mage's default BuildAll
# also emits windows and darwin binaries this image never runs, so the
# Dockerfile calls build:linux or build:linuxARM64 instead.
#
# Module download stays in the go-builder layer so a registry or GitHub
# Actions cache can restore it. The npm and Go build caches are mounts:
# they speed a warm local builder and are not part of the image.
#
# The final stage is grafana/grafana-oss:13.0.2 on the target platform and
# only copies plugin artifacts. USER 472 matches the chart runAsUser.
#
# From grafana/:
#   docker build -f Dockerfile --build-context foreverbull=.. .
# Multi-arch:
#   docker buildx build --platform linux/amd64,linux/arm64 \
#     -f Dockerfile --build-context foreverbull=.. .

FROM --platform=$BUILDPLATFORM node:26 AS js-builder

WORKDIR /app

COPY lhjnilsson-foreverbull-datasource/package.json lhjnilsson-foreverbull-datasource/package-lock.json lhjnilsson-foreverbull-datasource/.npmrc ./
RUN --mount=type=cache,target=/root/.npm,id=fb-grafana-npm \
    npm ci --cache /root/.npm

COPY lhjnilsson-foreverbull-datasource/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine AS go-builder

RUN apk add --no-cache git

WORKDIR /src/grafana/lhjnilsson-foreverbull-datasource

COPY --from=foreverbull go.mod go.sum /src/
COPY --from=foreverbull pkg /src/pkg

COPY lhjnilsson-foreverbull-datasource/go.mod lhjnilsson-foreverbull-datasource/go.sum ./
RUN go mod download

# Mage is the host-arch driver. Its linux targets set GOOS and GOARCH for
# the plugin binary, so this binary must stay on BUILDPLATFORM.
RUN go install github.com/magefile/mage@v1.17.2

COPY lhjnilsson-foreverbull-datasource/ ./

# Declared after the shared layers so both target arches reuse the module
# download and the copied sources. Each arch then compiles natively.
ARG TARGETOS
ARG TARGETARCH

RUN --mount=type=cache,target=/root/.cache/go-build,id=fb-grafana-go-build \
    if [ "${TARGETOS}" != "linux" ]; then \
      echo "unsupported TARGETOS ${TARGETOS}" >&2; \
      exit 1; \
    fi; \
    case "${TARGETARCH}" in \
      amd64) mage -v build:linux ;; \
      arm64) mage -v build:linuxARM64 ;; \
      *) echo "unsupported TARGETARCH ${TARGETARCH}" >&2; exit 1 ;; \
    esac

# Same target OS and arch as the plugin binary. The Grafana stage does not
# have a Go toolchain, and a build-platform binary would not run there.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -o /out/apply-web-links ./cmd/apply-web-links

FROM grafana/grafana-oss:13.0.2

ARG TARGETOS
ARG TARGETARCH

COPY --from=js-builder /app/dist/ /var/lib/grafana/plugins/lhjnilsson-foreverbull-datasource/
COPY --from=go-builder /src/grafana/lhjnilsson-foreverbull-datasource/dist/gpx_foreverbull_${TARGETOS}_${TARGETARCH} /var/lib/grafana/plugins/lhjnilsson-foreverbull-datasource/gpx_foreverbull_${TARGETOS}_${TARGETARCH}
COPY --from=go-builder /src/grafana/lhjnilsson-foreverbull-datasource/dist/go_plugin_build_manifest /var/lib/grafana/plugins/lhjnilsson-foreverbull-datasource/go_plugin_build_manifest
# The base image is already uid 472, so a later RUN cannot chown or chmod.
# Provisioning is owned by 472 so the entrypoint can rewrite the dashboard
# files. The two binaries stay root-owned and executable.
COPY --chown=472:472 lhjnilsson-foreverbull-datasource/provisioning/ /etc/grafana/provisioning
COPY --chmod=755 --from=go-builder /out/apply-web-links /usr/local/bin/apply-web-links
COPY --chmod=755 entrypoint.sh /usr/local/bin/foreverbull-entrypoint

# Enable unsigned plugins
ENV GF_PLUGINS_ALLOW_LOADING_UNSIGNED_PLUGINS=lhjnilsson-foreverbull-datasource
ENV GF_AUTH_ANONYMOUS_ENABLED=true
ENV GF_AUTH_DISABLE_LOGIN_FORM=true
ENV GF_AUTH_ANONYMOUS_ORG_ROLE=Admin
ENV GF_AUTH_BASIC_ENABLED=false
ENV GF_AUTH_DISABLE_SIGNOUT_MENU=true

# Expose Grafana port
EXPOSE 3000

# Chart runAsUser is 472. The OSS image user is that uid; pin it so the
# published image stays non-root after the plugin layers.
USER 472

ENTRYPOINT ["/usr/local/bin/foreverbull-entrypoint"]
