# Write a strategy, run a backtest, read the results

One linear path: write a strategy file, check it locally, run it as a backtest against a running Foreverbull install, then read the results and the errors. Every command is copy-paste and runs in order. Installing the platform is not part of this page; step 1 checks that it is already running.

## Conventions

- Commands are written for a clone of this repository, run from `client/` after `uv sync`, so the CLIs are `uv run fbull` and `uv run fbenv`.
- If you installed `foreverbull` and `foreverbull-cli` into your own virtualenv instead, activate it and drop the `uv run` prefix. Run the commands from any directory.
- Paths below are relative to that directory. The strategy lives in `my_strategy/`.

## 1. Prerequisites

You need:

- Python 3.12+ (tested on 3.12 and 3.14) and [uv](https://docs.astral.sh/uv/).
- A running Foreverbull environment (local via `fbenv`, or a remote install). To create one, follow [Your first backtest](../README.md#your-first-backtest) in the root README up to and including `fbenv start`, then come back here.

Check that the environment is up and the broker answers:

```bash
# clone: from the repository root. With your own virtualenv, skip these two lines and drop `uv run`.
cd client
uv sync

uv run fbenv status
uv run fbull backtest list
```

`fbenv status` lists the host checks and the six workloads (`postgres`, `nats`, `minio`, `zipline`, `foreverbull`, `grafana`). `fbull backtest list` prints a `Backtests` table, which may be empty. If it ends in a traceback with `StatusCode.UNAVAILABLE` and `failed to connect to all addresses`, the broker is not reachable; see [Common errors](#7-common-errors).

## 2. Create the backtest data

A backtest is a named universe, a date range, a benchmark, and the daily bars for them. `fbull backtest create` stores it and downloads the bars. A strategy runs against an existing backtest by name.

Write a small universe file and create it:

```bash
mkdir -p my_strategy
cat > my_strategy/backtest.yaml <<'YAML'
name: my-demo
benchmark: SPY
periods:
  - start_date: "2023-01-01"
    end_date: "2023-12-31"
    assets:
      - symbol: AAPL
        enabled: true
      - symbol: MSFT
        enabled: true
      - symbol: JPM
        enabled: true
      - symbol: XOM
        enabled: true
      - symbol: KO
        enabled: true
YAML

uv run fbull backtest create --file my_strategy/backtest.yaml
uv run fbull backtest get my-demo
```

`create` prints `Using provider: yahoo (default)`, a spinner for Creating, Downloading, Ingesting, and Storing, then a `Created Backtest` table. Yahoo is the default price source and needs no API key. To use Stooq or Alpaca bars, add `provider:` to the YAML or pass `--provider`; see [client/cli/README.md](../client/cli/README.md).

The name must be new. If `my-demo` already exists, either reuse it and skip `create`, or remove it with `uv run fbull backtest delete my-demo --force` and create it again. The repository's [examples/demo.yaml](../examples/demo.yaml) (backtest `demo`) works the same way.

## 3. Write the strategy

A strategy is one Python file that defines a module-level `Algorithm`. Write this file:

```bash
cat > my_strategy/sma_crossover.py <<'PY'
from foreverbull import Algorithm
from foreverbull import Assets
from foreverbull import Function
from foreverbull import Portfolio


def handle_data(
    assets: Assets,
    portfolio: Portfolio,
    short_window: int = 10,
    long_window: int = 30,
    max_positions: int = 10,
    cash_reserve: float = 0.05,
):
    selected: list[str] = []
    for asset in assets:
        close = asset.stock_data["close"]
        if len(close) < long_window:
            continue
        if close.tail(short_window).mean() > close.tail(long_window).mean():
            selected.append(asset.symbol)
    selected = selected[:max_positions]

    weight = portfolio.equal_weight_percent(len(selected), cash_reserve, max_positions)
    for symbol in selected:
        portfolio.order_target_percent(symbol, weight)

    for position in list(portfolio.positions):
        if position.symbol not in selected:
            portfolio.order_target(position.symbol, 0)


algo = Algorithm(functions=[Function(callable=handle_data)])
PY
```

What the client does with it:

- **The `Algorithm`.** `fbull backtest run` imports the file and uses the first module-level `Algorithm` instance it finds. The variable name does not matter. Keep the strategy in one file; it is loaded by path, not as a package.
- **The function is called once per trading day.** A parameter annotated `Assets` gets the whole universe for that day in one call. A parameter annotated `Asset` instead makes the function run once per symbol. Every function needs one of the two, plus `portfolio: Portfolio`.
- **Bars.** `asset.stock_data` is a pandas DataFrame with columns `symbol`, `time`, `open`, `high`, `low`, `close`, and `volume`, oldest first, ending on the current day. It holds at most the last 252 bars. Early in a backtest it has fewer rows, which is why the example skips a symbol until it has `long_window` bars.
- **Orders.** `portfolio.order_target_percent(symbol, percent)` buys or sells to hold `percent` of `portfolio_value` in that symbol, in whole shares (truncated toward zero). `portfolio.order_target(symbol, 0)` closes a position. `portfolio.equal_weight_percent(n, cash_reserve, max_positions)` is `(1 - cash_reserve) / min(n, max_positions)`; it raises when `cash_reserve` is outside `[0, 1)`. `portfolio.positions` iterates the held positions (`symbol`, `amount`) and supports `symbol in portfolio.positions`. Orders fill on the next bar.
- **Parameters.** Every other argument is a parameter, and each one needs a type annotation and a default. Use `int` and `float`. `fbull backtest run` runs with the defaults; to try another value, change the default and run again. The values used are stored with each execution (step 5).
- Size from `portfolio_value`, keep a cash reserve, cap the number of names, rebalance the names you keep every day, and close the names that leave. A fixed share count such as `order_target(symbol, 10)` does not scale with the portfolio. The rules are in [client/templates/README.md](../client/templates/README.md).

For a generated project with tests and a README, `uv run cookiecutter templates/simple` (from `client/`) writes `simple_foreverbull/crossover.py`; see [Strategy projects](../README.md#strategy-projects).

## 4. ✅ Check the strategy before you run it

These checks run on your machine. They need no cluster, and they catch most mistakes before a backtest session starts.

Load the file the same way `fbull backtest run` does. It prints the function and its parameters:

```bash
uv run python -c "
from foreverbull import Algorithm
algo = Algorithm.from_file_path('my_strategy/sma_crossover.py')
print(algo.get_definition())
"
```

Expected output (one line, wrapped here):

```text
Algorithm(file_path='…/my_strategy/sma_crossover.py', functions=[Algorithm.Function(name='handle_data',
parameters=[Algorithm.FunctionParameter(key='short_window', defaultValue='10', valueType='int'), …,
Algorithm.FunctionParameter(key='cash_reserve', defaultValue='0.05', valueType='float')])])
```

Optionally, unit-test the decision on synthetic bars. The `session` fixture from `foreverbull-testing` starts a Postgres container with Docker (no cluster), writes bars, and calls the same code path a backtest worker uses:

```bash
cat > my_strategy/test_sma_crossover.py <<'PY'
from pathlib import Path

from foreverbull_testing.session import Session

from foreverbull import Algorithm


STRATEGY = Path(__file__).with_name("sma_crossover.py")


def test_buys_the_name_that_crosses_up(session: Session):
    algo = Algorithm.from_file_path(str(STRATEGY))
    times = session.cross_up("XUP", 10, 30)
    orders = session.run(algo, times[-1], 100_000)
    assert [order.symbol for order in orders] == ["XUP"]
    assert orders[0].amount > 0


def test_flattens_a_held_name_that_crosses_down(session: Session):
    algo = Algorithm.from_file_path(str(STRATEGY))
    times = session.trend("XDN", 40, 50.0, 10.0)
    orders = session.run(algo, times[-1], 100_000, positions=[("XDN", 10)])
    assert [(order.symbol, order.amount) for order in orders] == [("XDN", -10)]
PY

# from client/: plain `uv sync` does not install foreverbull-testing, so add it for this run
uv run --with-editable ./testing pytest my_strategy/ -q
```

Expected: `2 passed`. With your own virtualenv, install the plugin once with `uv pip install "foreverbull-testing @ git+https://github.com/lhjnilsson/foreverbull.git@main#subdirectory=client/testing" pytest`, then run `pytest my_strategy/ -q`. The fixture API is in [client/testing/README.md](../client/testing/README.md).

## 5. Run the backtest

```bash
uv run fbull backtest run my-demo my_strategy/sma_crossover.py
```

Arguments are the backtest name and the strategy file. Options:

| Option | Effect |
| --- | --- |
| `--start YYYY-MM-DD` / `--end YYYY-MM-DD` | Run part of the backtest's range. Dates outside the range stop the run before it starts. |
| `--dry` | Print the backtest, file, and dates that would run, then exit. Checks that the broker knows the backtest name; it does not load the strategy. |
| `fbull -v`, `-vv`, `-vvv` | Put this before `backtest`. More log output, including your strategy's `logging` calls (`-vvv` shows `debug`). |

The strategy runs on your machine. `fbull` starts worker processes that import the file, and the broker sends them each trading day. Keep the terminal open until the command returns.

## 6. Read the results

### The run summary

A finished run prints a spinner for the session, an `execution` progress bar, then a summary. Numbers and IDs here are example data:

```text
✔ Backtest finished: my-demo · 2023-01-01 → 2023-12-31 · provider: yahoo

  Return        +4.2%  Benchmark (SPY)  +3.1%
  Sharpe         1.35  Sortino           1.10
  Max drawdown  -6.8%

  Details: uv run fbull backtest execution 01234567
  Charts:  http://localhost:3000/d/ee9ep4jxtwkcgf?var-execution=01234567-89ab-cdef-0123-456789abcdef
```

- Return, Sharpe, Sortino, and max drawdown are for the whole run. A ratio that is not defined yet prints `-`. The benchmark cell says `not set` or `not available` when there is no benchmark return.
- `No trades: the strategy held no position on any of the N trading days.` means the run worked but your strategy never held anything. Check that its conditions can be true with the bars it gets (for example a `long_window` longer than the range).
- `✔ Backtest finished, but no results were stored for this execution.` means the execution has no stored per-day results. Check the broker log (end of [Common errors](#7-common-errors)).

### Per-day results and the inputs used

Paste the `Details:` command. It takes the full execution ID or a unique prefix:

```bash
uv run fbull backtest execution 01234567
```

It prints an `Execution` table (ID, backtest, session, dates, provider), the Grafana `Link:`, a `Pinned inputs (function parameters)` table with the parameter values this run used, and a `periods` table with one row per trading day: Date, Portfolio, Alpha, Beta, Sharpe, and Sortino. Example tables are in the root README under [Example: `backtest execution` output](../README.md#example-backtest-execution-output).

Find earlier runs:

```bash
uv run fbull backtest executions --backtest my-demo   # every execution of this backtest
uv run fbull backtest get my-demo                     # the backtest, its periods, and its executions
```

### Charts in Grafana

Open the `Charts:` URL. It is the backtest execution dashboard with that run selected; the backtest and execution selectors at the top switch to other runs. A local install serves Grafana at [http://localhost:3000](http://localhost:3000) with anonymous access. A remote install uses its own Grafana address (`GRAFANA_URL`). What the dashboards show: [grafana/ANALYSIS.md](../grafana/ANALYSIS.md).

### Iterate

Change a default in `my_strategy/sma_crossover.py` (for example `long_window: int = 50`), repeat step 4, and run step 5 again. Each run is a new execution; compare them with `fbull backtest executions` and the Grafana links. To run one fixed version of a strategy across several backtests and compare them, see [Strategy lifecycle](../README.md#strategy-lifecycle).

## 7. Common errors

Messages that start with `✖` are one line and exit with code 1. Fix the cause and run the same command again unless the table says otherwise.

| You see | Meaning | Fix |
| --- | --- | --- |
| Traceback ending in `StatusCode.UNAVAILABLE` … `failed to connect to all addresses` | `fbull` cannot reach the broker (`localhost:50055` on a local install, or `FOREVERBULL_BROKER_ADDRESS` / `remote.broker_address`). | `uv run fbenv status`. If the workloads are stopped, `uv run fbenv start`. A remote address is `<ingress.host>:443` with `ingress.tls=true`. `host:50055` is refused. `kubectl port-forward` to `localhost:50055` stays plaintext. |
| `✖ There's no backtest named 'my-demo'. …` | The name passed to `run` does not exist. | `uv run fbull backtest list`, then use a listed name or run step 2. |
| `✖ Backtest 'my-demo' has no data to run. …` | The backtest record exists but its data is missing. | `uv run fbull backtest delete my-demo`, then step 2 again. |
| `✖ --start 2022-06-01 is before backtest 'my-demo' begins. …` (or `--end … is after …`) | `--start`/`--end` is outside the backtest's range. | Use dates inside the range shown by `fbull backtest get`, or leave both out. |
| `Invalid start date: …` | `--start`/`--end` is not `YYYY-MM-DD`. | Use an ISO date such as `2023-03-01`. |
| `FileNotFoundError: … No such file or directory` | The strategy path is wrong, relative to the current directory. | Run from the directory in [Conventions](#conventions), or pass an absolute path. |
| `ConfigurationError: No algorithm found in …` | The file has no module-level `Algorithm(...)`. | Add `algo = Algorithm(functions=[Function(callable=handle_data)])` at the bottom. |
| `TypeError: Function handle_data must have a parameter of type Asset or Assets` | No argument is annotated `Asset` or `Assets`. | Annotate one, as in step 3. |
| `TypeError: ('Unsupported type: ', …)` | An argument has no annotation, or one other than `Portfolio`, `Asset`, `Assets`, `int`, or `float`. | Annotate every argument. Use `int` or `float` for parameters. |
| `Worker request failed` with a Python traceback, then the run stops with a `✖` line | Your function raised an exception on some trading day. The traceback names the line. Depending on the broker version, the `✖` line repeats the exception or says the run stopped early. | Fix the code, re-run step 4, then step 5. Typical causes: too few bars for a window, a `NaN` from an indicator, a `KeyError` from a DataFrame column name. |
| `✖ Couldn't run backtest 'my-demo': the data service can't read market data. …` | The install's database permissions are wrong. Not your strategy. | Recreate or update the environment (README [Troubleshooting](../README.md#troubleshooting)). Report it if it persists. |
| `✖ The backtest session ended before an execution started.` | The broker closed the session before the run started. | Run again. If it repeats, check the broker log below. |
| `fbull backtest create` fails | Price download or ingest failed; nothing was saved. | README [Troubleshooting](../README.md#troubleshooting): `fbull backtest create` can't load data, and Price downloads fail. |

When the message is not enough, read the broker and Zipline logs (local Kind):

```bash
kubectl --context kind-foreverbull -n foreverbull logs deploy/foreverbull
kubectl --context kind-foreverbull -n foreverbull logs deploy/zipline
```
