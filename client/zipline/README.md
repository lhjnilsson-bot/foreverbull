# foreverbull-zipline

Zipline-Reloaded engine with a gRPC API for the Foreverbull broker. In the Helm chart this is the `zipline` workload (`lhjnilsson/zipline:{tag}`), reached in-cluster at `zipline:50055`.

It is a uv workspace member (`foreverbull-zipline`). Do not run it as a host process for the platform install; Kind/Helm starts the image. Unit tests run the engine in-process.

## Layout

| Path | What it is |
| --- | --- |
| `src/foreverbull_zipline/service.py` | gRPC `Engine` + calendar + health (`grpc_server`, default `:50055`) |
| `src/foreverbull_zipline/session_service.py` | Per-session engine RPCs |
| `src/foreverbull_zipline/engine.py` | Zipline `TradingAlgorithm` process |
| `src/foreverbull_zipline/data_bundles/foreverbull.py` | SQL ingest from Foreverbull Postgres |
| `Dockerfile` | Image used by `task client:docker:zipline` |
| `base.Dockerfile` | `lhjnilsson/zipline_base` (Zipline-Reloaded + TA-Lib) |

Entry point: `python -m foreverbull_zipline` (`__main__.py`). The image health check probes gRPC `:50055`.

## Bundle alignment

Ingested bars are keyed on Zipline session labels (midnight of the trading day), and each ingest is stamped with that alignment version inside its ingest directory, so the stamp travels with the bundle tarball. A bundle ingested before the alignment fix carries provider stamps inside the session (Yahoo's 14:30 UTC open) instead. On its first run after the fix the engine rebuilds such a bundle once, from its own stored bars, before the algorithm starts; a bundle stamped with the current version is checked and left alone, so later runs never pay a re-ingest. No delete-and-recreate is needed for an old backtest.

## Build the image

From `client/` (needs a built `foreverbull` wheel in `client/dist/`):

```bash
uv build
docker build -t lhjnilsson/zipline:local -f zipline/Dockerfile .
```

From the repo root: `task client:docker:zipline` (builds the wheel, then the image). `task build` builds that image with the others. `fbenv create` loads the tag into Kind. `fbenv start` runs the workloads.

## Tests

From `client/` after `uv sync`. These use Testcontainers Postgres via `foreverbull-testing`; they do not start Kind.

```bash
uv run pytest zipline/
```

Go backtest integration tests (`task backtest:test:integration`) need this local image first.

## In-cluster

Helm sets `DATABASE_URL` to the chart Postgres and `ZIPLINE_CONTAINER_NAME=zipline` on the broker. The broker does not talk to the Docker API; it looks up the Zipline pod/Service through the Kubernetes API.
