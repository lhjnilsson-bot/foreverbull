# Agent notes

Foreverbull is a Go trading platform with a Python client workspace. Work from the repository root unless a command says otherwise. Python is managed with **uv** (workspace in `client/`).

Install [Task](https://taskfile.dev/) (`go-task`) to run the root `taskfile.yaml` (it includes `client/taskfile.yaml` and `grafana/taskfile.yaml`).

## Style

Go and Python follow [style.md](style.md). The order there is safety, then performance, then developer experience. Read it before editing `cmd/`, `pkg/`, `internal/`, or `client/`. Domain contracts still name fields and behavior when they disagree with a style rule: [pkg/backtest/universe.md](pkg/backtest/universe.md), [pkg/backtest/reproducibility.md](pkg/backtest/reproducibility.md), [pkg/finance/profiles.md](pkg/finance/profiles.md), [pkg/finance/trading.md](pkg/finance/trading.md), [pkg/strategy/implementation.md](pkg/strategy/implementation.md), [pkg/finance/broker-door.md](pkg/finance/broker-door.md).

## Customer experience and dependencies

📝 Customer experience is essential. The connection is client ↔ Foreverbull broker ↔ Zipline, and the client and the user's own code get as much freedom as possible.

- **No version pins on the client side.** Do not add upper bounds or exact pins to the runtime dependencies of `foreverbull` (`client/pyproject.toml`) or `foreverbull-cli`. Users install these next to their own strategy code, and a pin there decides which pandas, numpy, or SQLAlchemy they may use.
- **Pin on the server side.** When a dependency needs a version constraint, put it in the image that runs the server-side component: `client/zipline/base.Dockerfile` (or `client/zipline/Dockerfile`) for Zipline, and the broker's Go modules or `docker/Dockerfile` for the broker.
- **Client code must work across versions.** If client code depends on version-specific library behaviour, make it explicit in code (for example, naming the database driver in a URL) rather than constraining the version.

Lockfiles (`client/uv.lock`, `requirements*.lock`) are for development and CI only. They are not a constraint on users.

## Fail, don't recover

📝 Foreverbull prefers failing to recovering. When something is missing, unexpected, or failed (a symbol with no prices, a failed download, an unmapped position, an invalid config), stop the operation, save nothing, and return an error that names every cause found. Do not silently skip, substitute, fall back to a default, swallow an error or only log it, save partial results, or continue with partial data.

- **Collect, then fail.** Gather every failure of the same kind before failing, so the user can fix them in one pass.
- **Recovery is opt-in.** Continuing past a failure is only allowed behind an explicit user choice, such as a `--skip-missing` flag. It is never the default, and the result records and shows what was left out.
- **No hidden retries.** Only the bounded retries a supplier already documents. A retry that still fails is a failure.
- **Ask first.** If a design seems to need a recovery path, raise it before building it.
- **Named exceptions.** These are deliberate and must not be "fixed": a symbol the provider confirms as delisted is closed at its last price (see [`universe.md` §8](pkg/backtest/universe.md#8-zipline-auto_close_date)), and per-symbol bars that were validated and stored before a price import failed are kept as a cache. The import still fails, and nothing runs on the partial set. Best-effort progress messages are also exempt: they are display only and never touch data, so a failed progress update does not fail the operation.

This complements [Programmer errors and operating errors](style.md#programmer-errors-and-operating-errors): an operating error is returned to the caller, not absorbed.

## Commits

Follow [Commit messages](style.md#commit-messages) in `style.md` whenever you create a commit. **The subject starts with one gitmoji**, then `type(scope): outcome` in the imperative. The emoji is the intent (✨ `feat`, 🐛 `fix`, ⚡️ `perf`, 📝 `docs`, ✅ `test`; a narrower match such as 💚 `fix(ci)` or 🚨 for a linter beats the generic one). The body says why. One logical change per commit. Every commit lands on `main` as written (Rebase and merge only), so autosquash `fixup!` and `squash!` commits before a PR leaves draft. Leave generated cookiecutter project trees out.

📝 Documentation uses that same emoji, once per page, on the heading or callout a reader would be wrong to skip. The rest of the page stays plain. See [In documentation](style.md#in-documentation).

## Layout

| Path | What it is |
| --- | --- |
| `cmd/server/` | Go broker/server entrypoint |
| `pkg/` | Go domains: `backtest`, `finance`, `service`, `strategy` |
| `pkg/pb/` | Generated Go protobuf (plus a few hand-written helpers) |
| `internal/` | Shared Go internals (env, grpc, storage, stream, Kubernetes container runtime, testcontainers helpers) |
| `proto/` | Protobuf **sources**. Edit these, then regenerate. |
| `client/` | uv workspace: `foreverbull`, `foreverbull-cli`, `foreverbull-testing`, `foreverbull-zipline` |
| `client/src/foreverbull/` | Python client library |
| `client/cli/` | `fbull` and `fbenv` CLIs (`fbenv` is Kind/Helm, not a Docker network) |
| `client/zipline/` | Zipline gRPC engine + Docker image |
| `client/testing/` | Pytest plugin/fixtures (`fb_database`, etc.) |
| `tests/end_to_end/` | Kind demo: `fbenv` and `fbull`. `task test:integration` |
| `deploy/` | Kind config, Helm chart, cluster-config example |
| `docker/Dockerfile` | Local `ghcr.io/lhjnilsson/foreverbull` image |
| `grafana/` | Grafana OSS image + `lhjnilsson-foreverbull-datasource` plugin. Contract: [grafana/ANALYSIS.md](grafana/ANALYSIS.md) |
| `internal/container/` | Kubernetes runtime for extra strategy-worker pods (no docker.sock) |

ETF-built universes are point-in-time from iShares as-of snapshots. Identity is ISIN; do not treat ticker as identity (FB then META is one asset). Stooq is not part of ETF membership. Contract: [pkg/backtest/universe.md](pkg/backtest/universe.md). A backtest's `provider` is `yahoo` (default when omitted), `stooq` (daily GetOHLC only; Yahoo stays for `GetAsset`, `SearchAsset`, and `GetIndex`), or `alpaca`. Bars are stored per provider. Trading credentials live only in the profiles file. Contract: [pkg/finance/profiles.md](pkg/finance/profiles.md). Re-running a backtest for comparison: [pkg/backtest/reproducibility.md](pkg/backtest/reproducibility.md).

## Prerequisites

- Docker (unit tests use Testcontainers; Kind also needs a Docker daemon; integration/e2e need local images)
- Kind, Helm, and kubectl (local `fbenv` / Kind cluster)
- Go 1.27.3 (see `go.mod`)
- Python 3.12+ and [uv](https://docs.astral.sh/uv/)
- Task
- For proto generation: [Buf](https://buf.build/docs/installation), `protoc-gen-go`, `protoc-gen-go-grpc`, and `uv sync` in `client/` (pinned `buf`, `protoc-gen-py`, and `protoc-gen-grpc-py`)

```bash
cd client && uv sync
```

## Testing

Many Go and Python unit tests start Postgres/NATS/Minio via Testcontainers. Have Docker running. They are not live-trading tests. **Do not convert unit/component tests to Kind.** Testcontainers stay on Docker even though `fbenv` installs the platform with Kind/Helm.

### Go

Domain unit tests (no `integration` build tag):

```bash
task test:unit
# equivalent per domain:
task backtest:test:unit
task finance:test:unit
task service:test:unit
task strategy:test:unit
```

Those run `go test ./...` inside `pkg/backtest`, `pkg/finance`, `pkg/service`, and `pkg/strategy`. Extra CLI args pass through as `task backtest:test:unit -- -count=1`.

From the repo root you can also run:

```bash
go test ./pkg/... ./internal/...
```

Backtest **integration** tests are tagged `//go:build integration` (`pkg/backtest/module_test.go`, `pkg/backtest/stream/backtest_test.go`). They need the local Zipline image first:

```bash
task backtest:test:integration
```

That depends on `client:docker:zipline`, then runs `go test . -tags=integration` and `go test ./stream/ -tags=integration` in `pkg/backtest`.

Plain `go test ./...` does **not** run those files unless you pass `-tags=integration`.

### Python

All Python tests run from `client/` with uv. `client/pyproject.toml` default addopts ignore `tests/end_to_end` (Zipline container fixtures) and `tests/integration`. The Kind demo is `tests/end_to_end/` at the repository root.

```bash
cd client
uv sync
uv run pytest                 # unit tests across the workspace
uv run pytest tests/          # client library tests
uv run pytest zipline/        # foreverbull-zipline
uv run pytest cli/            # fbull / fbenv
uv run pytest testing/        # pytest plugin
```

From the repo root:

```bash
task client:test
task client:test -- tests/
task client:test -- zipline/
task client:test -- cli/
task client:test -- testing/
```

The `foreverbull-testing` package is a pytest plugin (`fb_database` and related fixtures). Several client and zipline tests expect it to be installed via `uv sync`.

### Docker / Zipline images

```bash
task client:docker:zipline    # build ghcr.io/lhjnilsson/zipline:local (also builds the foreverbull wheel)
task build:docker             # build ghcr.io/lhjnilsson/foreverbull:local
task grafana:build:docker     # build ghcr.io/lhjnilsson/fb-grafana:local
```

`task client:docker:zipline` is the Zipline image used by Go backtest integration tests.

Plugin-only rebuild (frontend + Go binary into `grafana/lhjnilsson-foreverbull-datasource/dist/`, then the image):

```bash
cd grafana/lhjnilsson-foreverbull-datasource
npm install
npm run build    # frontend
mage -v          # gpx_foreverbull_*  (go install github.com/magefile/mage@latest)
# from repository root:
task grafana:build:docker
```

The Dockerfile also runs `npm run build` and `mage` itself. Product Grafana is Kind/Helm (`ghcr.io/lhjnilsson/fb-grafana` on host port 3000). Plugin `docker-compose.yaml` is a leftover UI harness on port 3001; Kind/`fbenv` does **not** create a Docker network named `foreverbull`.

### Python integration / end-to-end

These are **not** part of default `uv run pytest`:

```bash
task test:integration
```

That builds Foreverbull, Zipline, and Grafana local images, then from `client/` runs the root demo:

```bash
VERSION=local ECHO=true uv run pytest ../tests/end_to_end/test_demo.py -vs
```

`client/tests/integration/` is a heavier Zipline/ingest suite; pass that path explicitly if you need it. Some fixtures skip unless `BACKTEST_IMAGE` is set.

## Protobuf

Sources live in `proto/`. Generated code:

- Go: `pkg/pb/**` (`*.pb.go`, `*_grpc.pb.go`) and `internal/stream/pb/`
- Python: `client/src/foreverbull/pb/` (`*_pb.py`, `*_pb_grpc.py`)

**Do not hand-edit generated protobuf.** Change `.proto` files and regenerate with Buf (`buf.gen.yaml`, `buf.gen.py.yaml`):

```bash
task proto                    # Go + Python (`buf generate`)
task proto:generate           # Go only
task client:proto:generate    # Python only
task proto:clean              # deletes generated *_pb.py, *_pb_grpc.py, leftover *_pb2*, and *.pb.go
```

Hand-written helpers next to generated code (safe to edit):

- `pkg/pb/timestamp.go`
- `client/src/foreverbull/pb/pb_utils.py`

`mock_*.go` files are from `task mock:generate` (mockery), not Buf.

## Local / dev environment

The platform install is **Kind + Helm**, not a Docker Compose / Docker-network stack. `fbenv` creates a Kind cluster named `foreverbull` (default), then `helm upgrade --install`s `deploy/helm/foreverbull` into namespace `foreverbull`. The broker pod does **not** mount `/var/run/docker.sock`. Extra strategy-worker pods are created with the in-cluster Kubernetes API (ServiceAccount RBAC). Do not add a docker.sock volume to the broker, and do not talk to the Docker API from the broker runtime.

Build local images and sync the client (does not start the cluster):

```bash
task build
```

That builds Zipline, Grafana, and Foreverbull `:local` images and runs `uv sync` in `client/`. `uv sync` installs `foreverbull-cli`, so `uv run fbenv` is the Kind/Helm installer in this repo. A `fbenv` already on `PATH` may be an older Docker-container installer. Do not call that one.

Then from `client/`:

```bash
VERSION=local uv run fbenv create   # Kind + Helm, workloads stopped
uv run fbenv start                  # scale deployments to 1
```

Useful `fbenv` commands (from `client/`):

```bash
uv run fbenv status
uv run fbenv use                         # print selected cluster
uv run fbenv use local                   # Kind (default); writes .foreverbull/config.yaml
uv run fbenv use remote --kubeconfig … --context … --namespace … --broker-address host:50055
VERSION=local uv run fbenv create        # Kind cluster + helm upgrade --install, workloads stopped
uv run fbenv start                       # scale deployments to 1 and wait until ready
uv run fbenv stop                        # scale to 0
uv run fbenv stop --remove               # helm uninstall
uv run fbenv stop --remove --purge       # uninstall; local also deletes the Kind cluster; deletes .foreverbull data except config.yaml
uv run fbenv update                      # reload images (local) and helm upgrade --reuse-values
```

CLI flags `--cluster`, `--kubeconfig`, `--context`, `--namespace`, `--broker-address` override the selected cluster for one command.

Published host ports (Kind `extraPortMappings` + chart `expose.hostPort=true`): Postgres `5432`, NATS `4222`, Minio `9000`, Foreverbull gRPC `50055`, Foreverbull HTTP `8080`, backtest `27000–27015`, Grafana `3000`. Persistence is cluster PVCs (`persistence.*`), not host binds under `.foreverbull/{postgres,minio,nats}`.

Python broker stubs resolve the address in this order: `FOREVERBULL_BROKER_ADDRESS` → `remote.broker_address` when `cluster: remote` → `localhost:50055` when `cluster: local`. A host other than `127.0.0.1`, `::1`, or `localhost` always uses TLS, with or without a token (`FOREVERBULL_BROKER_CA` is an optional PEM file; otherwise the system trust store). The token only adds the authorization metadata. There is no plaintext fallback. Grafana in-cluster uses `BROKER_URL=foreverbull:50055`. Host Grafana is `http://localhost:3000`.

### Grafana analysis

Grafana is how users inspect a backtest run and overlay strategy experiments. CLI `fbull strategy experiments` stays the non-Grafana compare table. Live trading UI is out. Contract: [grafana/ANALYSIS.md](grafana/ANALYSIS.md).

| UID | Dashboard | Who deep-links it |
| --- | --- | --- |
| `ee9ep4jxtwkcgf` | Backtest execution (`provisioning/dashboards/executions.json`) | CLI `execution_link`, `GetExperiment.grafana_path`. **Keep this UID.** |
| `fb-strategy-compare` | Strategy compare (`provisioning/dashboards/strategy-compare.json`) | Overlay two executions. Must not steal `ee9ep4jxtwkcgf`. |

Path contract: `{GRAFANA_URL}/d/ee9ep4jxtwkcgf?var-execution={execution_id}`. `GRAFANA_URL` is the origin only (default `http://localhost:3000`). Helm `grafana.url` sets broker `GRAFANA_URL`. CLI reads the same env. Remote clusters set `grafana.url` to the public Grafana origin. Do not point those links at compose port 3001. The Web page uses that URL and `{GRAFANA_URL}/d/fb-backtest-overview?var-backtest={name}`. Grafana links back with `FOREVERBULL_WEB_URL`. Helm `grafana.webUrl` sets it. The default is empty. Kind via `fbenv` uses `http://localhost:8080`. The execution dashboard opens `/backtests/{name}` when a backtest is selected and `/backtests` otherwise.

Query type `GetExecutionMetric` JSON is `{"execution":{"id":"<uuid>"},"metrics":[{"name":"portfolio_value"}]}`. Variable values are execution UUIDs; labels are `{backtest} {start}–{end}`. Plugin tests: `go test ./pkg/...` under the datasource (fakes/bufconn) and `npm run test:ci`. Do not add Kind e2e for Grafana.

### Cluster config

Users pick `cluster: local` (default) or `remote`. Canonical keys: `deploy/cluster-config.example.yaml`.

| File / env | Role |
| --- | --- |
| `.foreverbull/config.yaml` | Project config (cwd; do not commit) |
| `~/.foreverbull/config.yaml` | Optional user override |
| `FOREVERBULL_CONFIG` | Explicit path; wins over the files above |
| `FOREVERBULL_BROKER_ADDRESS` | Runtime broker `host:port`; highest priority for stubs/CLI |

Merge order (later wins): built-in defaults → user → project → `FOREVERBULL_CONFIG`. Remote requires `remote.broker_address` (or `--broker-address` / `FOREVERBULL_BROKER_ADDRESS`). Local always uses `localhost:50055` unless the env var is set.

### Helm chart checks (no cluster)

```bash
helm lint deploy/helm/foreverbull
bash deploy/helm/check-chart.sh
# equivalent: helm template … and assert RBAC exists and docker.sock is not mounted
```

Manual Kind/Helm: `task kind:create`, `task kind:load`, `task helm:install` (chart default `replicaCount` is 1). `fbenv create` installs the same chart with workloads stopped; `fbenv start` scales them to 1. Recipes: `deploy/README.md`.

### Environment variables

Go server defaults are in `internal/environment/env.go`. Empty optional vars are filled at process start. A required var has no default: when it is empty, the broker names it and stops at startup.

| Variable | Default / notes |
| --- | --- |
| `GRPC_PORT` | `50055` (broker API; no HTTP listener) |
| `SERVER_ADDRESS` | `foreverbull` (in-cluster Service DNS other pods use) |
| `ZIPLINE_CONTAINER_NAME` | Go default `foreverbull_zipline`. **Helm** sets `zipline` (Service DNS). `internal/container` looks up that name as a Pod or `app.kubernetes.io/name`, skipping Terminating pods. The broker dials `<name>:50055` for ingest, so the name must also resolve as the Service. Each session looks the pod up again and uses its IP, because session ports are per pod. An `Unavailable` error makes the next call resolve Zipline again |
| `BACKTEST_PORT_RANGE_START` / `END` | `27000` / `27015`, both inclusive. Session, trading, and socket listeners all scan this range. A value that is not a port, or a start above the end, stops the broker at startup |
| `LOG_LEVEL` | `warning` (`debug`/`info`/`warning`/`error`); local `fbenv` sets `debug` on the broker (`--set broker.logLevel=debug`) |
| `POSTGRES_URL` | Required, no default. **Helm** Postgres is user/db `foreverbull`; the password comes from Secret `foreverbull-credentials` (`foreverbull` on local Kind, generated on clusters without `expose.hostPort`), and the broker pod gets `postgres://foreverbull:$(POSTGRES_PASSWORD)@postgres:5432/foreverbull`. Host-side against local Kind: `postgres://foreverbull:foreverbull@localhost:5432/foreverbull?sslmode=disable`. The broker pings about a minute (20 tries, 3 s apart), then exits non-zero with an error naming the host and port |
| `NATS_URL` | Required, no default (in-cluster: `nats://nats:4222`) |
| `NATS_DURABLE` | `foreverbull` |
| `NATS_DELIVERY_POLICY` | `all` |
| `MINIO_URL` | Required, no default (in-cluster: `minio:9000`) |
| `MINIO_ACCESS_KEY` / `MINIO_SECRET` | Required, no default. **Helm** reads them from Secret `foreverbull-credentials` (same values on local Kind, generated secret key elsewhere). See [deploy/README.md](deploy/README.md#credentials) |
| `POD_NAMESPACE` / `KUBERNETES_NAMESPACE` | Broker chart sets `POD_NAMESPACE` from the pod namespace; `internal/container` uses it for the in-cluster client |
| `TRADING_ENABLED` | `false`. Kill switch for paper `PlaceOrder` (`deploy --no-dry-run`). Not a provider. Deploy mode LIVE is refused. Never put API keys, tokens, or passwords in the chart, Postgres, or `.foreverbull/config.yaml`. |
| `TRADING_PROFILES_FILE` | Profiles YAML. In-cluster path `/etc/foreverbull/profiles.yaml`, mounted from Secret `foreverbull-trading` key `profiles.yaml`. The Secret is optional. Absent file plus `TRADING_ENABLED=false` starts with an empty registry; `TRADING_ENABLED=true` and a missing or invalid file is a startup error. Local Kind: if `.foreverbull/trading-profiles.yaml` exists, `fbenv` applies it as that Secret and does not print the file. Example: `deploy/trading-profiles.example.yaml` (empty credentials, no `baseUrl`). A profile names an adapter (`alpaca`, `saxo`, `ibkr`, or `ig`), currency (default `USD`), and credentials. Two deployments on one profile share that account. `GetPortfolio` still returns `ErrCurrency` when the account currency differs. |
| `TRADING_LISTINGS_FILE` | Optional replacement for the embedded ISIN listing sample when a profile does not set its own listings path. |
| `AUTH_ENABLED` | `false`. Empty is false. Helm and `fbenv` do not set it. Chart `broker.auth.enabled` stays `false`. While false, today's handlers, primary keys, and session listeners stay, and `task test:integration` does not need `FOREVERBULL_TOKEN`. `true` refuses to serve until `AUTH_ADMIN_USERNAME` is set and, when that account is missing, `AUTH_ADMIN_PASSWORD_FILE` can be read. That boot assigns existing rows to the admin, moves `service` and `backtest` onto uuid primary keys, copies MinIO ingestion objects to the id key, and then requires a credential on HTTP, broker gRPC, and session ports `27000`–`27015`. After `owner_id` is `NOT NULL` on `service`, `backtest`, or `strategy`, a start with the flag false exits. That is not a rollback. |
| `AUTH_ADMIN_USERNAME` | Platform admin login. Required only when `AUTH_ENABLED=true`. Empty means it is not configured. The enable path inserts that account as `admin` when it has no password row. If the username already exists and the role is not `admin`, startup exits and does not change the role. |
| `AUTH_ADMIN_PASSWORD_FILE` | Path of the admin password file. The password is not an environment variable, not a CLI flag, and not a key in `.foreverbull/config.yaml`. Helm mounts Secret `foreverbull-admin` key `password` at `/etc/foreverbull-admin/password`, not inside the profiles mount. The broker does not log the file or the hash. Once that password row exists, a later restart does not replace the hash from the file. |
| `AUTH_COOKIE_SECURE` | `false`. Set `true` when TLS terminates in front of the broker. The chart sets it when `ingress.tls` is true. That flag also publishes broker gRPC through Traefik (HTTP/2 on `ingress.entryPoint`, h2c to the broker) using cert-manager ClusterIssuer `ingress.clusterIssuer`. Default `ingress.tls` is false, so local Kind stays plaintext. |
| `OHLC_PROVIDER` | Set on a worker pod from the backtest `provider` when the pod is created. Python and Zipline default it to `yahoo` (`DEFAULT_PROVIDER` in `foreverbull.models.finance`) only when unset or blank. Not a broker-wide price switch. |
| `GRAFANA_URL` | Grafana origin for CLI `execution_link`, broker `GetExperiment.grafana_path`, and the Web links (`GET /api/config`). Default `http://localhost:3000` (Kind host port). Helm `grafana.url` sets it on the broker. Execution path is `/d/ee9ep4jxtwkcgf?var-execution=`. Overview path is `/d/fb-backtest-overview?var-backtest=` |
| `FOREVERBULL_WEB_URL` | Public foreverbull origin for Grafana "Open in foreverbull". Helm `grafana.webUrl` sets it on the Grafana pod. The chart default is empty. `fbenv` passes `FOREVERBULL_BROKER_URL`, else local `http://localhost:8080`, else `remote.broker_url`. An empty value hides the links; an unusable one stops Grafana from starting. Execution dashboard opens `/backtests/{name}` when a backtest is selected and `/backtests` when none is; overview opens `/backtests`. Do not put credentials or `$` in the value |
| `VERSION` | Image tag for `fbenv` / chart `global.imageTag` (`ghcr.io/lhjnilsson/{foreverbull,zipline,fb-grafana}`; prefix is CLI `IMAGE_REPOSITORY`, passed as chart `global.imageRepository`). Unset or `latest`: published nightly channel on public GHCR. `local`: images from `task build`. `fbenv` refuses any other value. Image-pull fails-before-changes is local-Kind only; remote nodes pull after Helm runs |
| `FOREVERBULL_BROKER_ADDRESS` | Python stubs/CLI broker `host:port` (highest priority) |
| `FOREVERBULL_BROKER_CA` | PEM file trusted for remote broker gRPC. Unset uses the system trust store. Remote gRPC is always TLS. `127.0.0.1`, `::1`, and `localhost` stay plaintext |
| `FOREVERBULL_CONFIG` | Explicit cluster config YAML path |
| `FOREVERBULL_BROKER_URL` | Broker HTTP origin for `fbull login` / `logout`. Default `http://localhost:8080` on local Kind; remote reads `remote.broker_url`. Must be `https://` unless the host is `127.0.0.1` or `localhost` |
| `DATABASE_URL` | Zipline ingest and the pytest plugin (Testcontainers). Strategy workers do not receive it |
| `BROKER_HOSTNAME` | Worker/NNG hostname (default `127.0.0.1`; in-cluster workers get `SERVER_ADDRESS`) |
| `BROKER_PORT` | Worker broker gRPC port; in-cluster workers get `GRPC_PORT` (`50055`) |
| `NAMESPACE_PORT` | Required for the worker namespace socket |
| `LOGGING_LEVEL` | Python client/CLI logging (`fbull -v/-vv/-vvv`) |
| `ECHO` | If set, CLI tests print stdout |
| `BACKTEST_IMAGE` / `BROKER_IMAGE` | Override images in some Python fixtures |

`MARKET_DATA_PROVIDER`, `TRADING_PROVIDER`, `TRADING_ALLOW_LIVE_URL`, `TRADING_CURRENCY`, and broker base-URL or credential variables are not read. Paper and demo URLs are constants in the adapters. There is no live URL switch.

Python Minio client (`Storage.from_environment`) also reads `STORAGE_ENDPOINT` (required, no default), `STORAGE_ACCESS_KEY` and `STORAGE_SECRET_KEY` (required, no default), and `STORAGE_SECURE` (`false` when unset; `true`, `false`, `1` or `0`).

Package docs: [client/README.md](client/README.md), [client/cli/README.md](client/cli/README.md), [client/zipline/README.md](client/zipline/README.md), [client/testing/README.md](client/testing/README.md), [deploy/README.md](deploy/README.md). Strategy → backtest → results walkthrough: [docs/strategy-run-results.md](docs/strategy-run-results.md). Cookiecutter templates: `client/templates/simple` and `client/templates/advanced` (root `cookiecutter.json`).

## CLI and fbenv

Both live in `client/cli/` (`foreverbull-cli`). Entry points in `client/cli/pyproject.toml`:

- `fbull` → `foreverbull_cli.bull.cli:cli` — backtests, strategy lifecycle, and trading against a running broker (`fbull backtest …`, `fbull strategy …`, `fbull trading …`)
- `fbenv` → `foreverbull_cli.env.cli:cli` — Kind/Helm environment (`local` Kind cluster or `remote` kubeconfig)

After `uv sync` in `client/`:

```bash
uv run fbull --help
uv run fbenv --help
```

### Strategy lifecycle

`fbull strategy` is register → experiment on existing backtests → compare period stats → paper deploy. Live is off. The same service version is runnable against different universes via separate `--backtest` experiments. Contracts: [pkg/strategy/implementation.md](pkg/strategy/implementation.md), [pkg/strategy/research-to-paper.md](pkg/strategy/research-to-paper.md).

```bash
uv run fbull strategy create mean-rev
uv run fbull strategy version add mean-rev --image lhjnilsson/my-algo:local
uv run fbull strategy experiment mean-rev --backtest spy
uv run fbull strategy experiment mean-rev --backtest eu-small
uv run fbull strategy experiments mean-rev
uv run fbull strategy deploy mean-rev --mode paper --dry-run
uv run fbull strategy deploy mean-rev --mode paper --no-dry-run --backend alpaca-paper --yes
uv run fbull strategy status mean-rev
uv run fbull strategy stop mean-rev
```

`fbull backtest create` reads optional `provider` and `profile` from the YAML. `--provider` overrides the file. An omitted provider is sent as `yahoo`. Create prints `Using provider: <name> (<source>)`, with source `default`, `from --provider`, or `from <YAML file name>`. The printed backtest includes the provider. `profile` is used only when `provider` is `alpaca`.

Default `deploy` is paper + `--dry-run` (Process, no PlaceOrder). Dry-run may omit `--backend`. Paper PlaceOrder needs `--no-dry-run`, `--backend <profile>`, and `--yes` or `--confirm`, plus `TRADING_ENABLED=true`. The profile is a name in the profiles file. `--mode live` is refused. Local-file hacking still uses `fbull backtest run`.

## Python lint

Ruff is pinned to **0.17.0** in `.pre-commit-config.yaml`, `.github/workflows/on_commit.yaml`, and `client/taskfile.yaml`. The commit hook and CI both run `ruff check` and `ruff format --check` on the whole `client/` tree whenever a client file is staged.

```bash
task client:lint              # uvx ruff@0.17.0 check
task client:format            # uvx ruff@0.17.0 format
```

Ruff ignores generated `client/src/foreverbull/pb/`, cookiecutter `templates/`, and local cookiecutter output `client/simple_foreverbull/` and `client/advanced_foreverbull/`.
