# Trading profiles and backtest price sources

A deployment names the trading profile it places orders on. A backtest names the price source that filled its bars. The process does not choose one broker or one price feed at startup.

This file is the contract. `pkg/finance/trading.md` still describes the order shape and the four adapters. Where this file and that one disagree about configuration, this file wins. The order shape, the ISIN wrapper, the sentinel errors, and the refusal of `mode=live` do not change.

## What is stored where

| Fact | Where it lives |
| --- | --- |
| Profile name on a running strategy | `Deployment.backend` |
| Price source of a backtest | `Backtest.provider` |
| Adapter, currency, and credentials for a profile | The profiles file, mounted from a Kubernetes Secret |
| Kill switch | `TRADING_ENABLED`, default `false` |
| Paper and demo base URLs | Constants in the adapters |
| ISIN listings | Embedded sample, unless a profile sets a listings path |

The deployment row and the backtest row do not store keys, tokens, or passwords. `.foreverbull/config.yaml` does not store them either. Committed Helm values do not store them.

## Profiles file

Path comes from `TRADING_PROFILES_FILE`. In the cluster that path is `/etc/foreverbull/profiles.yaml`, mounted from Secret `foreverbull-trading`, key `profiles.yaml`. The Secret is optional. When the file is absent and `TRADING_ENABLED` is false, the registry is empty and the process starts. When `TRADING_ENABLED` is true, a missing or invalid file is a startup error.

Local Kind: if `.foreverbull/trading-profiles.yaml` exists, `fbenv` applies it as that Secret before `helm upgrade`. `.foreverbull` is gitignored. The committed example is `deploy/trading-profiles.example.yaml` and its credential fields are empty. `fbenv` must not print the file.

Shape:

```yaml
profiles:
  - name: alpaca-paper
    provider: alpaca
    currency: USD
    apiKey: ""
    apiSecret: ""
  - name: saxo-sim
    provider: saxo
    currency: USD
    accessToken: ""
    accountKey: ""
  - name: ig-demo
    provider: ig
    currency: USD
    apiKey: ""
    identifier: ""
    password: ""
  - name: ibkr-paper
    provider: ibkr
    currency: USD
    accountId: ""
    accessToken: ""
```

Rules:

- `name` is unique. `provider` is `alpaca`, `saxo`, `ibkr`, or `ig`.
- No `baseUrl` field. A file that contains one is invalid. URLs are the paper and demo constants from `trading.md`.
- `ibkr` is paper mode. The file cannot set a live account mode.
- Currency defaults to `USD` when omitted. `GetPortfolio` still returns `ErrCurrency` when the account currency differs.
- Two deployments that name the same profile share that account's positions.

`Deploy` with `dry_run=false` requires `TRADING_ENABLED` and a `backend` equal to a profile name in the registry. An unknown name is `FailedPrecondition`. `dry_run=true` may omit `backend`; the stored value is empty, not `"default"`. `mode=LIVE` stays refused. `TRADING_ALLOW_LIVE_URL` is removed. A live host is not representable in the file.

The runner loads the client for `deployment.backend` on each session. It does not keep a single process-wide trading client. `pkg/strategy` still does not import a broker SDK.

Remove these environment variables and the Helm values that set them: `TRADING_PROVIDER`, `TRADING_ALLOW_LIVE_URL`, `TRADING_CURRENCY`, `ALPACA_MARKETS_BASE_URL`, `ALPACA_MARKETS_API_KEY`, `ALPACA_MARKETS_API_SECRET`, `SAXO_BASE_URL`, `SAXO_ACCESS_TOKEN`, `SAXO_ACCOUNT_KEY`, `IG_BASE_URL`, `IG_API_KEY`, `IG_IDENTIFIER`, `IG_PASSWORD`, `IBKR_BASE_URL`, `IBKR_ACCOUNT_ID`, `IBKR_ACCOUNT_MODE`, `IBKR_ACCESS_TOKEN`. Credentials exist only in the profiles file.

`broker.yaml` must not render those keys as plain `value:` entries. It mounts the Secret and sets `TRADING_PROFILES_FILE` and `TRADING_ENABLED`. `deploy/helm/check-chart.sh` fails if the rendered broker manifest contains an Alpaca, Saxo, IG, or IBKR credential value, or the host `api.alpaca.markets`.

## Price source on a backtest

`Backtest.provider` is `yahoo`, `stooq`, or `alpaca`. Create stores `yahoo` when the caller omits it. The column is `NOT NULL`. Existing rows that predate the column migrate to `yahoo`.

`MARKET_DATA_PROVIDER` is removed. Asset search, `GetAsset`, and `GetIndex` stay on Yahoo. Stooq remains daily `GetOHLC` only, with Yahoo for metadata, chosen because this backtest says `stooq`. Alpaca `GetOHLC` uses the key of a profile:

- The create request may set `profile` when `provider` is `alpaca`.
- If `profile` is empty and exactly one Alpaca profile is configured, use that one.
- Zero Alpaca profiles, or more than one with `profile` empty, fails ingest before any download.
- `profile` on a `yahoo` or `stooq` backtest is ignored.

Stored bars are adjusted per provider as follows. A split never shows as a one-day move:

| Provider | Splits | Dividends | Return basis |
| --- | --- | --- | --- |
| `yahoo` | adjusted (chart `close`) | not adjusted | price return |
| `alpaca` | adjusted (`adjustment=split`, sent explicitly; the SDK default is `raw`) | not adjusted | price return |
| `stooq` | adjusted | adjusted | close to total return |

Bars already stored are not replaced on a later download (`ON CONFLICT DO NOTHING`). Alpaca bars downloaded before this change stay unadjusted. A way to fetch them again is planned.

`ohlc` is unique on `(provider, symbol, time)`. Existing tables gain `provider text NOT NULL DEFAULT 'yahoo'` and replace `UNIQUE (symbol, time)` with `UNIQUE (provider, symbol, time)`. The migration is idempotent and does not delete bars. `CREATE TABLE IF NOT EXISTS` alone is not a migration.

Every read that decides which bars a backtest or a worker sees filters on that provider:

- `OHLC.Store`, `Exists`, and the execution bundle query.
- Python `Asset.stock_data` and the close lookup used by `order_target_percent`.
- The Zipline bundle query in `client/zipline`.
- The calendar missing-bar check when it is deciding ingest completeness for a backtest.

The worker pod gets `OHLC_PROVIDER` from the backtest when the pod is created. Python and Zipline read that variable and default it to `yahoo` (`DEFAULT_PROVIDER` in `foreverbull.models.finance`) only when it is unset or blank, which covers tests that insert bars and leave the column at its default. Two providers may store two closes for the same symbol and day. A run reads only its own.

Changing `provider` on an existing backtest does not relabel old rows. The next ingest writes the new provider's rows. The run reads the provider stored on the backtest.

Ingest keeps one scale per provider and symbol. A bar is bad when its open, high, low, or close is 0 or below, NaN, or infinite.

- With nothing stored, ingest fetches the requested range and inserts it.
- `OHLC.Store` upserts. A refetched bar replaces the stored one, but a bad bar is refused and never replaces a stored row.
- When the stored history starts on or before the requested start, ingest fetches from the last stored day, so the fetch overlaps one stored day. If that day comes back with a good close within 0.5% of the stored close, the new days are upserted.
- Otherwise the scale of the stored history can't be trusted: the close moved (for example after a split), the overlap day is missing from the fetch, or either close is bad. Ingest then fetches again from the first stored day and replaces that range in one transaction (`OHLC.ReplaceRange`).
- When the requested start is before the stored history, ingest fetches from the start through the last stored day and replaces that range the same way.
- Every batch is checked with `repository.ValidateBars` before anything is written, for an upsert and for a replace. If one bar is bad, ingest writes nothing for that symbol, leaves every stored row as it was, and returns the `InvalidPriceError` naming the provider, symbol, and day. A bad bar never turns into a missing day.
- Inside a replace, a stored day the provider no longer returns is removed. The removed span is the requested range, not the span of the returned bars, so a stored day after the provider's last bar is removed too. A replace whose refetch returns no bars fails with `<provider> returned no prices for SYM between START and END. Check that SYM traded between those dates, try again later, or use another provider with --provider.` and changes nothing. The DownloadHistoricalData RPC still reports it to backtest create as `no price data for SYM`, which backtest create turns into its own sentence.
- One row per trading day. `OHLC.Store` and `OHLC.ReplaceRange` compare days by trading date, so a bar for 05:00Z replaces a row stored at 00:00Z on the same day. The trading date is the UTC day. SQL compares `time >= <00:00 UTC> AND time < <next 00:00 UTC>`, with both bounds computed in Go, so the result doesn't depend on the session time zone and the `(provider, symbol, time)` index can be used.
- Ingest and the `DownloadHistoricalData` RPC store bars through the same rule (`OHLC.Sync`). Each symbol runs in one transaction whose first statement is `pg_advisory_xact_lock(hashtext('ohlc:' || symbol))`; the stored range, the overlap close, and the append-or-replace decision are read and made under that lock, so two ingests or downloads of one symbol store one after the other. The provider is asked before the lock: `Sync` reads what is stored, fetches every span its decision needs, and only then takes the lock and decides again with those bars. The provider is called under the lock only when another sync changed the stored history in between and the decision needs a span that wasn't fetched, so a slow provider normally holds neither the lock nor a connection. Every provider call gets the request's context, so a cancelled download or ingest stops the fetch. The key is the symbol only, without the provider, so syncs of one symbol from two providers also wait for each other. The transaction sets `lock_timeout` to 60s and `idle_in_transaction_session_timeout` to 90s before it takes the lock. Concurrent syncs on one pool are capped at the pool's `MaxConns` minus 1, so one connection always stays free for the health check.
- A database error reaches the task state as `Couldn't store prices for SYM. Try again in a few minutes. If it keeps failing, report it at https://github.com/lhjnilsson/foreverbull/issues.` The RPC sends its generic download sentence instead. The cause is logged on the server.
- Provider requests share one rate per provider for the whole broker process, across every download and ingest: Yahoo 5 a second, Stooq 2, Alpaca 3 (under Alpaca's free 200 a minute). The limiter is `golang.org/x/time/rate` with no burst, plus a shared pause. The ISIN lookup on another site is not counted against Yahoo. A 429 pauses the provider for every caller, including callers already waiting for a slot: they wait out the pause, then take a new slot. The pause is `Retry-After` when that is 1 to 30 seconds, checked before it becomes a duration. Otherwise it is 2s doubling to 30s. Yahoo, Stooq, and the Alpaca asset-status lookup send the request again up to 3 times after a 429, each time through the shared limiter. Alpaca bar requests are sent again by foreverbull's own transport, up to 3 times after a 429 or any 5xx (500 to 599), so at most 4 sends, each time through the shared limiter and under the fetch's ctx. The wait is the same `Retry-After` or backoff; only a 429 pauses the provider for every caller. The Alpaca SDK's own retry is off, so nothing is retried twice. A wait that would end after the fetch's deadline or its 60s budget, counting a shared pause another caller's 429 set, isn't started: the fetch fails at once with that status, so the user sees the 429 or 5xx, not a timeout. When the deadline ends a retry's wait or its send, the fetch also fails with the last status Alpaca sent. A cancel without a deadline stays a cancel, and a download's own time limit is still reported as that limit. Any other status is not retried. A 422 means Alpaca found the bars request invalid: it fails the fetch at once with `ErrAlpacaBarsRejected`, and is not asked again with an earlier end. Provider limit: Alpaca's free plan refuses SIP data from the last 15 minutes with a 422, so every Alpaca bars request ends at least 15 minutes before now (`alpacaRecentDataLimit`), set before the request. Before 16:30 New York the end is already just before midnight New York today. From 16:30 it is now minus 15 minutes, which is still after the 16:00 close, so today's daily bar is included. When this cap or the completed-session rule leaves nothing after the start, Alpaca isn't asked: `AlpacaBars.NothingToFetch` says so first, and the download answers the symbol OK with nothing stored and doesn't look for a later ticker, because an empty answer for that range says nothing about the ticker. Every error status from the bars endpoint is an `AlpacaBarsError`; the log line has the status and the number of tries, and the response body is neither logged nor in the error. Bars come from Alpaca's market data API (`https://data.alpaca.markets`), not the paper trading host. The wait for a slot runs under the request's timeout, or for Alpaca bars under the 60s fetch budget (there the timeout bounds each send), so a fetch under the symbol lock still ends before the 90s idle timeout. A wait whose slot would come after its deadline is refused at once. A wait that is cancelled or times out gives its slot back, so abandoned waits don't delay the next fetch. Yahoo `GetIndex` looks up at most 4 components at a time. Because each member is a provider lookup, `GetIndex` is limited to an editor or an admin while `AUTH_ENABLED` is true, like `DownloadHistoricalData`. A viewer or a `read` token gets `PermissionDenied`.
- A provider's HTTP error status on a symbol's bars, after those retries, is sent with `STATUS_ERROR` as the typed `provider_status` (provider and HTTP code). Backtest create reads that field, not the text, and collects every such symbol before it fails: `Couldn't load data for backtest 'oef-2024q1': foreverbull couldn't download prices for XYZ from alpaca: <cause>. The backtest wasn't saved. <step>`, or one block with a line per symbol (`  XYZ: <cause>.`) for two or more, and `couldn't download prices: <cause>` lines in the mixed block with missing prices or failed lookups. The `STATUS_ERROR` text, which RPC callers see, is `foreverbull couldn't download prices for XYZ from alpaca: <cause>. <step>`. Its steps name no backtest and no `--provider` flag, and they end with `run the download again`, for example `Run the download again in a few minutes, or try another provider.` The causes are `alpaca rejected the request as invalid (HTTP 422)`, `alpaca didn't accept foreverbull's API key (HTTP 401)` (or 403), `alpaca limited how many requests foreverbull can make` (429), `alpaca had a server error (HTTP 503)` with the actual code for any status from 500 to 599, and `alpaca refused the request (HTTP 418)` for any other status. The symbol is only in the lead and the number of tries only in the log. The wording and steps live in `pkg/finance/api/failure_copy.go`.
- A `DownloadHistoricalData` RPC and an ingest command each stop after `FINANCE_TASK_TIME_LIMIT`, a Go duration that defaults to `2h`. The broker refuses to start when it isn't a duration or isn't above zero. Symbols stored before the limit stay stored, and the symbol in flight rolls back. That includes a split refetch: the stored rows keep their old closes. The RPC ends with `DeadlineExceeded` and `the download hit its time limit of 2h0m0s at SYM`, naming the first symbol it didn't answer for. A symbol waiting on its lock, or a provider wait refused because its slot comes after the limit, gets this text too. Backtest create turns it into ``Couldn't load data for backtest 'NAME': the download hit the 2-hour limit after 6,212 of 10,000 symbols. The backtest wasn't saved. Split the symbols across several backtests, then run `fbull backtest create` again.``, with the progress line's counts. The ingest task state is `The price import hit the 2-hour limit at SYM. Prices imported before SYM were kept. Import fewer symbols at a time, or try again later.` A different limit is named the same way: whole hours in hours (`the 1-hour limit`), anything else in whole minutes (`the 90-minute limit`), rounded up with at least 1, so `30s` reads `the 1-minute limit`.
- A provider call that times out while there is still time (an HTTP timeout, Alpaca's fetch budget, a refetch in `Sync`, or a provider wait refused under one of those shorter deadlines) fails that symbol with `<provider> took too long to respond for SYM` as `STATUS_ERROR`. Backtest create turns it into `Couldn't load data for backtest 'NAME': <provider> took too long to respond for SYM. The backtest wasn't saved. Try again later, or use another provider with --provider.` The ingest task state is `<provider> took too long to respond for SYM, so the price import stopped. Prices imported before SYM were kept. Try again later.`
- Ingest writes bars only inside a transaction: new days go through `OHLC.StoreBars` and replaced ranges through `OHLC.ReplaceRange`. A connection that can't open a transaction gets `ErrNoTransaction`, and nothing is written or removed.
- A requested range that is already stored is not fetched.

## CLI

`fbull backtest create` reads optional `provider` and `profile` from the YAML file. A flag `--provider` overrides the file. An omitted provider is stored as `yahoo`. Create prints `Using provider: <name> (<source>)`, with source `default`, `from --provider`, or `from <YAML file name>`. The printed backtest shows the provider. An unknown name fails with the valid list. While prices download, the Downloading row reads `Downloading prices: N of TOTAL symbols` and updates in place, at most every 2 seconds. Progress goes over core NATS (`foreverbull-progress.…`), not the `foreverbull` JetStream stream, and is not written to `message_states`.

`fbull strategy deploy --no-dry-run` requires `--backend <profile>`. Dry-run may omit it. The help text still says live mode is refused.

## Out of scope

- Enabling live deploy or live base URLs.
- Splitting one profile's positions across deployments.
- A secret manager other than a Kubernetes Secret. The gitignored file is only the install source for that Secret.
- FX conversion, stops, or a second Python order API.
- Rewriting ETF membership or ISIN identity.

## Definition of done

- `TRADING_PROVIDER` and `MARKET_DATA_PROVIDER` are not read.
- A deployment with `backend=alpaca-paper` uses that profile's client. A second deployment can name a different profile.
- Dry-run stores an empty backend. Paper `PlaceOrder` without a known profile is `FailedPrecondition`. `mode=LIVE` is still refused.
- Rendered Helm for the broker has no trading credential `value:` fields. The example profiles file has empty secrets.
- `ohlc` uniqueness includes `provider`. A test stores two closes for one symbol and day under `yahoo` and `stooq`, and a read with `OHLC_PROVIDER=yahoo` returns only the Yahoo close.
- `fbull strategy` tests still refuse live mode.
