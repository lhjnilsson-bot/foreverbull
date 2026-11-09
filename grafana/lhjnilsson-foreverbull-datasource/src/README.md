# Foreverbull datasource

Grafana backend datasource for Foreverbull backtest executions and strategy experiments.

Plugin ID: `lhjnilsson-foreverbull-datasource`. Requires Grafana 10.4 or later.

The backend talks to the Foreverbull gRPC broker (`BROKER_URL`, in-cluster default `foreverbull:50055`). Query type `GetExecutionMetric` loads one execution by ID (`execution.id`) and returns selected metrics over time:

- `portfolio_value`
- `max_drawdown`
- `alpha`
- `beta`
- `sharpe`
- `sortino`
- `capital_used`
- `long_count`
- `short_count`
- `long_value`
- `short_value`

Execution picker values are UUIDs. Labels are `{backtest} {start}–{end}` plus a short id. Overlay another run as a second query. Strategy / version / experiment variables cascade on the compare dashboard; experiment values are still execution UUIDs.

## Dashboards

| UID | Title | Deep link |
| --- | --- | --- |
| `ee9ep4jxtwkcgf` | Backtest execution | `{GRAFANA_URL}/d/ee9ep4jxtwkcgf?var-execution={id}` |
| `fb-strategy-compare` | Strategy compare | `{GRAFANA_URL}/d/fb-strategy-compare` |
| `fb-backtest-overview` | Backtest overview | `{GRAFANA_URL}/d/fb-backtest-overview` |

`GRAFANA_URL` is the Grafana origin (default `http://localhost:3000` on local Kind). CLI `fbull backtest run` / `fbull strategy experiments` and `GetExperiment.grafana_path` print the first URL. Helm `grafana.url` sets broker `GRAFANA_URL` for remote origins. Do not change UID `ee9ep4jxtwkcgf` or the `var-execution` query key. The Web page uses the same execution URL and `{GRAFANA_URL}/d/fb-backtest-overview?var-backtest={name}` for the overview filter. The variable name is `backtest`.

`FOREVERBULL_WEB_URL` is the public foreverbull origin (Helm `grafana.webUrl`, default empty; Kind via `fbenv` is `http://localhost:8080`). The execution dashboard's "Open in foreverbull" link is `{origin}/backtests/{name}` when a backtest is selected and `{origin}/backtests` when none is. The overview dashboard's link is `{origin}/backtests`. An empty value hides the links; an unusable one stops Grafana from starting. Do not put credentials or a `$` in the origin.

This plugin is built into the `lhjnilsson/fb-grafana` image (`task grafana:build:docker`). Product Grafana is Kind/Helm, not plugin-dev compose. Do not enable live trading from Grafana.
