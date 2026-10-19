# ETF universes — implementation contract

This document is the implementation contract for point-in-time ETF universes in `pkg/backtest`. It is not code. Read it before changing the iShares scraper, `backtest_period_asset`, finance `asset` / `ohlc`, or Zipline ingest.

Goal: ETF-built universes that are stable and point-in-time. Identity is ISIN. Ticker is a dated alias. Membership is intervals from consecutive as-of diffs. Ingest unions every instrument that ever appears; the session filters to members on date `t`.

## 1. Problem

`CreateBacktestFromETF` (`pkg/backtest/internal/stream/command/backtest.go`) builds a backtest from an iShares product. Three defects make that universe neither stable nor point-in-time.

### 1.1 Yearly first-hit

`ISharesETFLoopup.GetAssets` (`pkg/backtest/internal/stream/dependency/backtest.go`) takes a list of calendar years. For each year it runs:

```text
for _, year := range years {
    for day := 0; day <= 365; day++ {
        date = {year}-01-01 + day
        GET ajax?tab=all&fileType=json&asOfDate=YYYYMMDD
        if aaData is empty: continue
        retMap[year] = parsed rows
        break
    }
}
```

The first non-empty iShares `aaData` in that year becomes *the* holdings list for the whole year. January reconstitutions, month-end membership, and anything after the first hit are discarded. `CreateBacktestFromETF` then stores one `backtest_period` per calendar year (`start_date` = Jan 1, `end_date` = Dec 31) and copies that single list into `backtest_period_asset`.

### 1.2 Ticker as primary key

Finance identity is the ticker:

```sql
CREATE TABLE IF NOT EXISTS asset (
    symbol text primary key,
    name text);
```

`ohlc` is keyed the same way: `UNIQUE (symbol, time)` with `symbol TEXT references asset(symbol)`. Proto `foreverbull.finance.v1.Asset` already has `string ISIN = 3`, and the iShares row already supplies it (`dependency.Asset.ISIN` from `row[8]`). Neither is stored. FB then META are two assets. A ticker recycle is a silent identity collision.

`GetAssetRequest` already has `oneof identifier { string symbol = 1; string isin = 2; }`. `CreateBacktestFromETF` always sends `GetAssetRequest_Symbol`.

### 1.3 `enabled` means GetAsset succeeded

For each scraped Equity row, `CreateBacktestFromETF` calls `Marketdata.GetAsset` by symbol (Yahoo metadata for every backtest provider; `MARKET_DATA_PROVIDER` is not read). `backtest_period_asset.enabled` is true when that call succeeds and false when it fails. `DownloadFinancialData` later sets `enabled=false` if `DownloadHistoricalData` returns `STATUS_ERROR`.

`enabled` is therefore “the vendor knows this ticker,” not “this ISIN is in the ETF on these dates.” Non-Equity rows (`AssetType != "Equity"`) are skipped; that filter stays.

### 1.4 Ingest union, session empty list

Ingest is already a union, which is correct:

- `DownloadFinancialData` walks every period, keeps enabled symbols plus `backtest.benchmark`, and downloads `[periods[0].start_date, periods[-1].end_date]`.
- `SQLIngester.what_to_ingest` (`client/zipline/src/foreverbull_zipline/data_bundles/foreverbull.py`) does the same set: enabled `period.assets` plus `benchmark`, then `SELECT open, high, low, close, volume, time FROM ohlc WHERE symbol='…' AND time BETWEEN …`.

The session does not filter that union. `ZipelineSession.RunBacktest` (`pkg/backtest/internal/backtest/engine_session.go`) calls:

```go
workers.Process(ctx, period.Portfolio.Timestamp.AsTime(), []string{}, period.Portfolio)
```

`worker.Pool.Process(ctx, timestamp, symbols, portfolio)` forwards `symbols` to the algorithm. `Process([]string{})` means the worker never receives the members as of `t`. Parallel functions iterate the slice and do no work; sequential functions receive an empty list.

Zipline equity metadata computes `auto_close_date = end_date + 1 day` and then overwrites it with `None` (`TODO: How to autoclose date?`). Dropped names never close.

## 2. Design principles

1. **ISIN is identity.** One iShares ISIN is one instrument for the life of the backtest. FB and META are one asset with two ticker intervals.
2. **Ticker is a dated alias.** `ohlc` stays on `symbol`. Do not rewrite bars onto ISIN. Map ISIN → symbol through `asset_symbol`.
3. **Membership is intervals, not calendar years.** Consecutive as-of snapshots are diffed by ISIN. Adds and drops open or close `universe_member` rows. Ticker changes do not.
4. **Ingest unions; the session is point-in-time.** Every symbol that ever appears (plus benchmark) is downloaded and ingested. On date `t` the session passes only current members into `Process`.
5. **Sample, do not dump daily.** Month-end or known rebalance as-of dates. Cache those snapshots. Do not walk `day := 0; day <= 365` to find the first hit.
6. **Reuse existing column types.** `text`, `date`, `boolean`, `timestamp` as they already appear on `backtest`, `backtest_period`, `asset`, and `ohlc`. No native `uuid` columns.

## 3. What already exists (reuse)

| Capability | Where | Universe should |
| --- | --- | --- |
| iShares holdings + ISIN | `ISharesETFLoopup.GetSnapshot` / `ListSnapshotDates`; `dependency.Asset.{Symbol,Name,Sector,AssetType,ISIN}` | Keep the product URL and JSON parse. Sample month-end `asOfDate`s (not `day := 0; day <= 365`). `GetSnapshot` is Equity-only and includes ISIN. `CreateBacktestFromETF` uses those APIs. `GetAssets(years)` is a year-keyed compatibility wrapper |
| ETF → benchmark ticker | `GetBenchmark` (`iShares S&P 100` → `OEF`) | Unchanged; `backtest.benchmark text NOT NULL` |
| Yearly period + membership | `backtest_period` / `backtest_period_asset` | Stop using calendar-year lists as membership. Keep `backtest(name, benchmark)` |
| Vendor asset lookup | `Marketdata.GetAsset(symbol)`; RPC oneof already has `isin` | Do not switch Yahoo/Alpaca. Do not add a provider. ISIN comes from iShares, not from `GetAsset` |
| Bars | `ohlc(symbol, time)` | Ingest still writes `(symbol, time)`. Session maps ISIN → symbol then reads bars by symbol |
| Trading calendar | `trading_calendar_date (code, date)` PK | Month-end as-of dates = last `date` per month in range for `XNYS` |
| Zipline ingest | `SQLIngester` union of enabled symbols + benchmark | Union becomes “every alias that ever appears + benchmark.” Set `auto_close_date`; do not null it |
| Session worker | `Process(timestamp, symbols, portfolio)` | Pass PIT tickers, not `[]string{}` |

`backtest_period_asset.enabled` and yearly `backtest_period` rows remain until callers migrate. New membership lives in `universe_member`. Do not dual-write forever; the yearly table is the old model.

## 4. Identity and aliases

### 4.1 ISIN

Source of truth for a holdings row is iShares `row[8]` (`dependency.Asset.ISIN`). Proto field is `foreverbull.finance.v1.Asset.ISIN` (`string`). Store it as `text`, same as `asset.symbol` and `backtest.name`.

`asset.symbol` stays the finance primary key. Ingest (`repository.Asset.Store(symbol, name)`, `ohlc.Store(symbol, …)`, `DownloadHistoricalDataRequest.symbols`) does not change. Yahoo `GetAsset(symbol)` and Alpaca `GetAsset(symbol)` stay symbol-keyed.

### 4.2 `instrument`

Stable identity. One iShares ISIN is one instrument. `id` equals `isin` (text, not uuid). `name` is the issuer string from iShares; it is not identity.

```sql
CREATE TABLE IF NOT EXISTS instrument (
    id text PRIMARY KEY,
    isin text UNIQUE,
    name text
);
```

FB then META is one `instrument` row. A new ticker is a new `asset_symbol` interval, not a new instrument.

### 4.3 `asset_symbol`

Ticker history for one ISIN. Half-open `[from_date, to_date)`. `to_date` NULL means current.

```sql
CREATE TABLE IF NOT EXISTS asset_symbol (
    isin text NOT NULL,
    symbol text NOT NULL,
    from_date date NOT NULL,
    to_date date,
    PRIMARY KEY (isin, from_date)
);
```

`isin` matches `instrument.isin`. `symbol` is the same `text` as `asset.symbol` / `ohlc.symbol`. `from_date` / `to_date` are `date`, same as `backtest_period.start_date`. Do not FK `symbol` to `asset(symbol)` at write time: the alias can exist before `Store` creates the asset row.

Example: FB then META is one ISIN, two rows. Membership does not close.

On date `t`, current ticker is:

```sql
SELECT symbol FROM asset_symbol
 WHERE isin = $1
   AND from_date <= $2
   AND (to_date IS NULL OR $2 < to_date);
```

Mapping helpers (`pkg/finance/internal/repository.Instruments`):

- `Resolve(symbol, t)` → the `instrument` whose alias at `t` is that ticker
- `Symbols(instrument, t)` → the ticker of that ISIN at `t`

Ingest unions `DISTINCT symbol` for every ISIN that appears in `universe_member` for that backtest, including expired aliases, plus `backtest.benchmark`.

### 4.4 Missing ISIN

A holdings row with empty `ISIN` is skipped. It does not create an `instrument` and does not get a synthetic id. Do not match or merge rows by ticker or issuer name. FB and META are one instrument only when iShares supplies the same ISIN.

## 5. Snapshots

### 5.1 Cache

Do not refetch iShares on every `CreateBacktestFromETF`. Two layers:

**Raw JSON (scraper, this step).** `ISharesETFLoopup` caches each successful iShares `aaData` response on the filesystem, keyed by ETF slug and `asOfDate`:

```text
{cacheDir}/{etf_slug}/{YYYYMMDD}.json
# example: iShares_S_P_100/20221231.json
```

- Runtime dir: `$FOREVERBULL_ETF_SNAPSHOT_CACHE`, else `{os.UserCacheDir()}/foreverbull/etf_snapshots` (temp dir if the user cache is unavailable).
- Tests: `pkg/backtest/internal/stream/dependency/testdata/etf_snapshots` fixtures, or a temp dir. Unit tests use `httptest`; they must not hit live iShares.
- Empty `aaData` is unpublished: do not cache it. `GetSnapshot` then walks back at most 6 calendar days and caches the first published body under the requested as-of.
- Request shape stays `tab=all`, `fileType=json`, `asOfDate=YYYYMMDD`.
- API: `GetSnapshot(etf, asOf) ([]Asset, error)` (Equity-only, includes ISIN) and `ListSnapshotDates(etf, start, end) ([]time.Time, error)` (last calendar day of each month in range, plus any dates already in the cache).

**Parsed holdings table (later).** Persist membership rows once `CreateBacktestFromETF` is rewired:

```sql
CREATE TABLE IF NOT EXISTS etf_holdings_snapshot (
    etf_name text NOT NULL,
    as_of_date date NOT NULL,
    isin text NOT NULL,
    symbol text NOT NULL,
    name text,
    sector text,
    asset_type text NOT NULL,
    PRIMARY KEY (etf_name, as_of_date, isin)
);
```

Column types match `dependency.Asset` and `backtest.name` (`text` / `date`). `etf_name` is the product key already used by `GetAssets` (e.g. `iShares S&P 100`). When the table exists, empty `aaData` should try the previous session on `trading_calendar_date` instead of a 6-day lookback.

### 5.2 Sampling

Replace the yearly first-hit loop. The scraper now samples; it does not walk every calendar day.

For `[start_date, end_date]`:

1. `ListSnapshotDates` takes the last calendar day of each month in range. Cached reconstitution dates in that range are included.
2. `GetSnapshot` GETs each as-of once (filesystem cache on repeat).
3. Later: load `trading_calendar_date` for `XNYS` (`code text`, `date date`) and use the last session of each month; insert parsed rows into `etf_holdings_snapshot`.

Do not sample every calendar day. Do not keep “first published day of the year” as a special case. A year of snapshots is 12 month-ends (plus lookback only when a month-end is unpublished), never `day := 0; day <= 365`.

Tests of the scraper use `httptest` or cached fixtures. Do not hit live iShares in unit tests.

## 6. Diff → membership intervals

### 6.1 `universe_member`

```sql
CREATE TABLE IF NOT EXISTS universe_member (
    backtest_name text REFERENCES backtest (name) ON DELETE CASCADE,
    isin text NOT NULL,
    from_date date NOT NULL,
    to_date date,
    PRIMARY KEY (backtest_name, isin, from_date)
);
```

Half-open `[from_date, to_date)`. `to_date` NULL means still a member at the backtest end. `backtest_name` matches `backtest.name` / `backtest_period.backtest_name`.

Members on date `t`:

```sql
SELECT isin FROM universe_member
 WHERE backtest_name = $1
   AND from_date <= $2
   AND (to_date IS NULL OR $2 < to_date);
```

Join `asset_symbol` at `t` to get the ticker list for `Process`.

### 6.2 Consecutive snapshot diff

Order snapshots for the ETF by `as_of_date`. Restrict to `asset_type = 'Equity'` (same filter as `CreateBacktestFromETF`). Diff **by ISIN**, not by ticker.

| Si vs Si+1 | Action |
| --- | --- |
| ISIN in Si+1, not in Si | **Add.** Insert `universe_member (from_date = Si+1.as_of_date, to_date NULL)`. Upsert `asset_symbol` for the new ticker. |
| ISIN in Si, not in Si+1 | **Drop.** Close `universe_member.to_date = Si+1.as_of_date`. Do not delete the row. |
| Same ISIN, different symbol | **Rename.** Close the old `asset_symbol.to_date = Si+1.as_of_date`; insert a new alias from Si+1. Leave `universe_member` open. |
| Same ISIN, same symbol | No-op. |

Seed from S0: every Equity ISIN opens a member interval at S0.`as_of_date` and an `asset_symbol` interval for that ticker.

Adds/drops change membership. Ticker changes do not.

`pkg/backtest/internal/universe.Diff` is the consecutive-snapshot differ. It emits half-open `universe_member` intervals (and ticker alias intervals). Persistence is `universe_member` in the backtest repository. `CreateBacktestFromETF` loads month-end snapshots, diffs them, and writes `instrument` / `asset_symbol` / `universe_member`.

### 6.3 Hysteresis (optional)

To ignore a one-off as-of glitch, require two consecutive snapshots before committing an add or a drop:

- Add: ISIN present in Si and Si+1, absent before Si → `from_date = Si.as_of_date`.
- Drop: ISIN absent in Si and Si+1, present before Si → `to_date = Si.as_of_date`.

Renames still apply on the first snapshot that shows the new ticker (the ISIN never left). Default the flag off until a product proves noisy; the schema does not change.

This step leaves hysteresis off. Month-end (and cached reconstitution) as-of dates are already sampled, so a two-snapshot confirmation would lag a genuine add/drop by a month. Enable per product if a feed is noisy.

## 7. Ingest union vs session PIT

```text
snapshots ──diff──► universe_member (ISIN intervals)
                 └─► asset_symbol (ticker intervals)

ingest:   DISTINCT symbol in asset_symbol for those ISINs  ∪  benchmark
session:  members at t  →  tickers at t  →  Process(ctx, t, tickers, portfolio)
```

### 7.1 Ingest

`DownloadFinancialData` and `SQLIngester.what_to_ingest` take the union of every alias, not the members of the last calendar year. Date window is the backtest start/end (today: `periods[0].start_date` … `periods[-1].end_date`). Benchmark is always included.

If a symbol fails `DownloadHistoricalData`, record a download error. Do **not** delete `universe_member`. Vendor coverage is not membership. `GetAsset` success is not membership.

Keep writing `asset(symbol, name)` and `ohlc(symbol, time)`. Mapping layer only.

### 7.2 Session

`ZipelineSession.RunBacktest` must stop calling `Process([]string{})`. On each period timestamp `t`:

1. ISINs from `universe_member` at `t`.
2. Symbols from `asset_symbol` at `t`.
3. `workers.Process(ctx, t, symbols, portfolio)`.

Empty universe → empty slice (algorithm sees no names). That is different from today’s bug, where the slice is always empty.

### 📝 7.3 Symbol-list backtests

A backtest created from a symbol list (`fbull backtest create --file`, for example `examples/demo.yaml`) has no `universe_member` rows. Its universe is the enabled `backtest_period_asset` rows of the period whose `[start_date, end_date]` holds the UTC date of `t`, sorted, each symbol once. That is the set `DownloadFinancialData` and `SQLIngester` put in the bundle. A disabled asset has no bars, so it is not passed.

The session decides once, before the first bar: any `universe_member` row for the backtest means §7.2, and an empty universe at `t` stays `[]`. No rows means this section. A failed read of the backtest or its membership fails the run before Zipline starts.

`RunBacktestRequest.benchmark` is the stored `backtest.benchmark` for both kinds. An empty benchmark makes Zipline use a flat zero series, so `benchmark_period_return` would be stored as `0`.

## 8. Zipline `auto_close_date`

`SQLIngester.create_metadata` already declares `("auto_close_date", "datetime64[ns]")`. The writer builds `autoclose_date = end_date + pd.Timedelta(days=1)` then sets it to `None`.

Contract:

- One Zipline equity row per **symbol** (because `ohlc` is per symbol). FB and META are two equities sharing an ISIN via `asset_symbol`.
- `start_date` / `end_date` = first and last bar actually present for that symbol in `ohlc`.
- `auto_close_date` = session after the last bar (`end_date + 1 day`), i.e. the value already computed. Do not overwrite with `None`.
- Write the column as `auto_close_date` to match the metadata dtype, not `autoclose_date`.
- A drop closes membership in `universe_member` and, once bars end, auto-closes the Zipline equity. A rename auto-closes the old ticker’s equity and opens the new one; the ISIN remains a member.

## 9. `CreateBacktestFromETF` after this contract

1. Resolve `[start_date, end_date]` to sampled as-of dates (§5.2).
2. Fill `etf_holdings_snapshot` from cache or iShares.
3. Diff → `asset_symbol` + `universe_member` (§6).
4. `backtest.benchmark` from `GetBenchmark`.
5. Ingest union (§7.1). Session PIT is a runtime read, not a stored yearly list.

`enabled = GetAsset(symbol)` is not part of membership. Looking up a ticker in Yahoo/Alpaca remains an ingest concern.

## 10. Non-goals

Do not implement in the universe work:

- Changing bar-provider defaults beyond what `Backtest.provider` already supports (`yahoo` default when omitted, `stooq`, `alpaca`). Leave Yahoo metadata and Alpaca wiring as they are in `pkg/finance/module.go`. `MARKET_DATA_PROVIDER` is not read.
- Daily holdings dumps. Sample month-end or known rebalance as-of dates only.
- Paid index files, vendor constituent feeds, or licensing iShares beyond the existing public ajax JSON.
- Walk-forward, strategy domain, paper/live trading.
- Rewriting `ohlc` off `symbol`. A mapping layer + `asset_symbol` is enough.
- Changing `asset.symbol text primary key` or `UNIQUE (symbol, time)` on `ohlc`.
- Hitting live iShares in unit tests.
- Mocking Postgres with SQLite. Repository tests keep existing testcontainers helpers.

## 11. Current types (do not invent new ones)

Reuse these as-is where the new tables touch them:

| Column | Type | Source |
| --- | --- | --- |
| `backtest.name` | `text PRIMARY KEY` | `BacktestTable` |
| `backtest.benchmark` | `text NOT NULL` | `BacktestTable` |
| `backtest_period.start_date` / `end_date` | `date NOT NULL` | `BacktestTable` |
| `backtest_period_asset.symbol` | `text NOT NULL` | `BacktestTable` |
| `backtest_period_asset.enabled` | `boolean NOT NULL DEFAULT false` | `BacktestTable` |
| `asset.symbol` | `text primary key` | `AssetTable` |
| `asset.name` | `text` | `AssetTable` |
| `instrument.id` | `text PRIMARY KEY` (equals `isin`) | `InstrumentTable` |
| `instrument.isin` | `text UNIQUE` | `InstrumentTable` |
| `instrument.name` | `text` | `InstrumentTable` |
| `asset_symbol.isin` | `text NOT NULL` | `AssetSymbolTable` |
| `asset_symbol.symbol` | `text NOT NULL` | `AssetSymbolTable` |
| `asset_symbol.from_date` / `to_date` | `date` (`to_date` nullable) | `AssetSymbolTable` |
| `universe_member.backtest_name` | `text` FK `backtest(name)` | `UniverseMemberTable` |
| `universe_member.isin` | `text NOT NULL` | `UniverseMemberTable` |
| `universe_member.from_date` / `to_date` | `date` (`to_date` nullable) | `UniverseMemberTable` |
| `ohlc.symbol` | `TEXT references asset(symbol)` | `OHLCTable` |
| `ohlc.time` | `timestamp` | `OHLCTable` |
| `ohlc` uniqueness | `UNIQUE (symbol, time)` | `OHLCTable` |
| `trading_calendar_date.code` | `TEXT NOT NULL` | `CalendarTable` |
| `trading_calendar_date.date` | `date NOT NULL` | `CalendarTable` |
| ISIN | `string` / `text` | proto `Asset.ISIN`; iShares `row[8]` |

## 12. Status

Landed. ETF create samples month-end iShares as-of dates (filesystem JSON cache; Equity rows with ISIN). Identity is ISIN (`instrument` + `asset_symbol`); ticker is a dated alias (FB then META is one asset). Membership is half-open `universe_member` intervals from consecutive as-of diffs: adds/drops change membership, ticker changes do not. Ingest unions every alias plus the benchmark; the session maps members through `asset_symbol` at `t` into `Process` (empty universe stays `[]`). A symbol-list backtest has no membership rows and passes its enabled period assets instead (§7.3). SQLIngester writes `auto_close_date` on a drop and keeps one sid across a rename. `GetAsset` failure is not a membership drop. Yearly `backtest_period` / `backtest_period_asset` rows remain for ingest windows; they are not membership.

Not in this work: reintroducing `MARKET_DATA_PROVIDER`; daily holdings dumps; the `etf_holdings_snapshot` table (raw JSON cache is enough); hysteresis; rewriting `ohlc` off `symbol`.
