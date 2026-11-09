# Foreverbull Grafana datasource

Backend Grafana datasource for Foreverbull backtest executions and strategy experiments (`lhjnilsson-foreverbull-datasource`).

Plugin ID: `lhjnilsson-foreverbull-datasource`. Grafana 10.4+. Product image is Grafana OSS 11.3.2 (`grafana/Dockerfile`).

The Go backend (`pkg/`) calls the Foreverbull gRPC broker (`BacktestServicer` and `StrategyServicer`). The React UI (`src/`) is a query editor: pick an execution and one or more metrics. Overlay is a second `GetExecutionMetric` query (refId B), not a second query type.

The image Helm installs is `lhjnilsson/fb-grafana`. In-cluster it sets `BROKER_URL=foreverbull:50055`. Local Kind publishes Grafana at `http://localhost:3000`. Contract: [grafana/ANALYSIS.md](../ANALYSIS.md).

## Prerequisites

- Node.js 22+ (see `engines` in `package.json`) and npm
- Go 1.27.3 (plugin `go.mod` and the image builder `golang:1.27.3-alpine`)
- [Mage](https://magefile.org/): `go install github.com/magefile/mage@latest`

For the platform Grafana pod: Docker, Kind, Helm, and the rest of the Foreverbull stack (`task build`, then `fbenv create` and `fbenv start`). That is **not** this `docker-compose.yaml`.

## Layout

| Path | What it is |
| --- | --- |
| `src/` | React / TypeScript UI (config editor, query editor, variable editor) |
| `pkg/` | Backend datasource (gRPC client, `/executions`, `/metrics`, `/strategies`, `/versions`, `/experiments`) |
| `provisioning/` | Datasource and dashboards baked into `lhjnilsson/fb-grafana` |
| `docker-compose.yaml` | Isolated Grafana 11 for plugin UI work (see below). **Not** Kind/`fbenv` |
| `Magefile.go` | Cross-compiles `gpx_foreverbull_*` into `dist/` |

The backend reads `BROKER_URL`. Helm sets `foreverbull:50055`. If unset, the Go code defaults to `localhost:50055`.

`go.mod` has `replace github.com/lhjnilsson/foreverbull => ../..` so the plugin uses this checkout’s protobuf clients.

## Rebuild

From this directory:

```bash
npm install
npm run build    # frontend → dist/
mage -v          # backend binaries → dist/gpx_foreverbull_*
```

Watch the frontend:

```bash
npm run dev
```

Rebuild Go after changes under `pkg/`:

```bash
mage -v
```

Platform image (from the **repository root**):

```bash
task grafana:build:docker    # lhjnilsson/fb-grafana:local
```

Equivalent from `grafana/`: `task build:docker`. Then load it with `uv run fbenv update` (or `task build`, then `uv run fbenv create` and `uv run fbenv start`). Signing is not required for local Kind.

Do not commit `node_modules`. Commit updated `dist/` only when the image build needs a frontend/backend you actually built and the Dockerfile copies it (the image build runs `npm run build` and `mage` itself).

## Dashboards

Provisioned under `provisioning/dashboards/`. Datasource name and uid: `foreverbull`.

| UID | File | Role |
| --- | --- | --- |
| `ee9ep4jxtwkcgf` | `executions.json` | Inspect one run. CLI and `GetExperiment` deep-link this |
| `fb-strategy-compare` | `strategy-compare.json` | Overlay two executions (strategy → version → left/right) |
| `fb-backtest-overview` | `backtest-overview.json` | Leaderboard of runs that overlap the time picker (`now-2y` to `now`). Rows link back with `var-execution` |

Do not change `ee9ep4jxtwkcgf`. If it ever moves, ship a redirect and update CLI `GRAFANA_DASHBOARD_UID` plus `GrafanaExecutionURL` in the same change.

Deep links: `{GRAFANA_URL}/d/ee9ep4jxtwkcgf?var-execution={id}`.

- `GRAFANA_URL` is the Grafana **origin**. Default `http://localhost:3000` (Kind host port).
- Helm `grafana.url` sets broker env `GRAFANA_URL` for `GetExperiment.grafana_path`.
- CLI `execution_link` reads `GRAFANA_URL` the same way.
- Remote clusters set `grafana.url` / `GRAFANA_URL` to the public origin. Do not change the UID or `var-execution`.
- The Web page uses that execution URL, and `{GRAFANA_URL}/d/fb-backtest-overview?var-backtest={name}` for one backtest. The variable name is `backtest`.
- `FOREVERBULL_WEB_URL` (Helm `grafana.webUrl`, default empty; Kind via `fbenv` is `http://localhost:8080`) is the public foreverbull origin. "Open in foreverbull" on the execution dashboard is `{origin}/backtests/{name}` when a backtest is selected and `{origin}/backtests` when none is. On the overview dashboard it is `{origin}/backtests`. An empty value hides the links; an unusable one stops Grafana from starting. Do not put credentials or a `$` in `grafana.webUrl`.
- Plugin-dev compose is host port **3001**. Do not point CLI or broker links there.

## Queries

Query type `GetExecutionMetric` loads one backtest execution by ID. Canonical JSON:

```json
{
  "queryType": "GetExecutionMetric",
  "execution": { "id": "<uuid>" },
  "metrics": [{ "name": "portfolio_value" }]
}
```

Do not write `executionId`. Template variable `$execution` interpolates onto `execution.id`.

Metrics include `portfolio_value`, `max_drawdown`, `alpha`, `beta`, `sharpe`, `sortino`, `capital_used`, `long_count`, `short_count`, `long_value`, `short_value`.

Execution picker **values** are UUIDs. **Labels** are `{backtest} {start}–{end} {short id}`. Variable queries can list `executions`, `strategies`, `versions`, or `experiments` (experiment values are still execution UUIDs).

`CheckHealth` dials the broker (`ListBacktests`). Save & test is not the Path / API Key scaffold.

## Plugin-only Grafana (compose)

`docker-compose.yaml` runs Grafana 11 on host port `3001` with this unsigned plugin mounted from `dist/`. It is **not** the Kind/`fbenv` stack and does **not** use a Kind Docker network.

Kind’s cluster is named `foreverbull`. That is a Kind cluster, not a Docker network. Compose still declares an **external** Docker network also named `foreverbull` (`networks.foreverbull.external: true`). `fbenv` does not create that network. `npm run server` fails unless you already created a Docker network named `foreverbull` and have a broker reachable as `foreverbull:50055` on it.

Product Grafana: Helm chart (`lhjnilsson/fb-grafana`) at `http://localhost:3000`.

```bash
npm run server    # docker compose up --build  — plugin UI only
```

Unsigned loading: `GF_PLUGINS_ALLOW_LOADING_UNSIGNED_PLUGINS=lhjnilsson-foreverbull-datasource`. Compose’s default image is Grafana Enterprise leftover; the product image is OSS 11.3.2.

## Tests

```bash
npm run test:ci      # Jest
npm run lint
npm run typecheck
go test ./pkg/...    # backend unit tests (fakes/bufconn, not a live broker)
```

Playwright (`npm run e2e`) expects `npm run server` and a reachable broker. Config-editor Playwright still asserts Path / API Key and is not a product test.

Live trading is out. Do not enable it from Grafana.
