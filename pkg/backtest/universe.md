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

For each scraped Equity row, `CreateBacktestFromETF` calls `Marketdata.GetAsset` by symbol (Yahoo metadata for every backtest provider; `MARKET_DATA_PROVIDER` is not read). `backtest_period_asset.enabled` is true when that call succeeds and false when it fails. `DownloadFinancialData` fails create (and removes the backtest) if `DownloadHistoricalData` returns `STATUS_ERROR` for any symbol; it does not disable the member and continue.

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

`asset_symbol` is shared by every ETF backtest. `CreateBacktestFromETF` writes each alias of its own window with `Instrument.StoreAlias`, which reads the ISIN's rows and changes them in one transaction:

- A NULL `to_date` only says the ticker was still current in the last list that backtest read. A backtest that ends before a rename leaves FB open-ended.
- An open-ended row yields to a different ticker that starts after its `from_date`. When a later backtest stores META from 2021-10-31, the open FB row is closed at 2021-10-31. When the later window starts after the rename and only sees META from its start, FB is closed at that start; nothing is known about the days in between.
- The same rule works in reverse order. A new open-ended row is capped at the `from_date` of the next stored row of a different ticker. An older backtest that sees FB open-ended after META is stored from 2021-10-31 stores FB up to 2021-10-31, not NULL.
- Rows of the same ticker that overlap are one alias and are stored as their union. A known `to_date` wins over an open one.
- Any other overlap of two tickers is refused with `asset_symbol_no_overlap` (`repository.ErrAliasConflict`), and the create fails. That covers a row with a known `to_date` that another ticker starts inside, an open row that another ticker starts on or before, and two tickers with the same `from_date`. A stored row is never renamed in place.

Known limit: creates run one at a time, because the broker handles each command from one JetStream subscription. `StoreAlias` locks the ISIN's existing rows (`FOR UPDATE`) but takes no advisory lock. If creates ever run in parallel, two of them can miss each other's new rows for one ISIN; the exclusion constraint then fails one create instead of merging its rows. Take `pg_advisory_xact_lock` on the ISIN before that changes.

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

Seed at the backtest start from the last list published on or before `start_date`. S0 is that list: for a 2020-01-01 start it is the 2019-12-31 month-end, and for a start that is itself a published as-of it is that day. Every Equity ISIN in S0 opens a member interval at `start_date` and an `asset_symbol` interval for that ticker from the same day, and S0's list holds until the next as-of after the start. A list published after the start is a look-ahead: an ISIN that first appears in the 2020-01-31 list (or in a cached mid-January as-of) is not a member before that as-of, and is a member from it.

`CreateBacktestFromETF` reads S0 with the same `GetSnapshot` call as the window. iShares serves a holdings file for any past `asOfDate`, so it lists the month-ends (and cached as-of dates) in the three months before the start and reads them newest first, stopping at the first published file. A month-end with no file is skipped, not a stop. A gap in the window is filled from S0, not from a later list. S0 is moved to `start_date`; as-of dates before the start are not stored.

Only when nothing is published on or before the start within that walk (an ETF that launched after it, or a run of missing files) does the first list after the start seed it, so the universe is not empty. That fallback is a look-ahead and is logged as a warning with `etf`, `start_date`, and `snapshot_date` (the as-of used): `no holdings snapshot published on or before the backtest start, seeded from the first one after it`.

The seed copy keeps each ISIN and ticker, so the next as-of is not an add or a rename unless its list differs.

Adds/drops change membership. Ticker changes do not.

`pkg/backtest/internal/universe.Diff` is the consecutive-snapshot differ. It emits half-open `universe_member` intervals (and ticker alias intervals) and opens the first ones at the first snapshot's as-of; it does not know the backtest window. Persistence is `universe_member` in the backtest repository. `CreateBacktestFromETF` loads month-end snapshots, moves S0 to the backtest start, diffs them, and writes `instrument` / `asset_symbol` / `universe_member`.

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

> **Disabled until listing and delisting dates are stored.** Leaving out a delisted member, and the auto-close in §8, do not run on a gap inside the period for now. foreverbull doesn't store listing or delisting dates yet, so create fails on a gap at either end of the period (§7.5, the edge-gap rule), and ETF membership doesn't count as evidence of a listing or delisting. What follows is the rule once those dates are stored.

A member whose prices have ended is left out of `Process`. A delisting mid-month keeps the ISIN in `universe_member` until the next month-end list drops it, but the bundle has no bar for it after the delisting. Before the first bar the session reads, for each member's tickers, the ISIN's last `ohlc` bar under any of its tickers for the backtest's provider, before the day after the backtest's last period (a later download for another backtest is not in this bundle). On date `t` a ticker whose ISIN's last bar is before the UTC date of `t`, and before the newest bar any member has, is not passed. A member whose last bar is that newest bar is where the stored data ends, not a delisting, so it is neither left out nor named. A renamed ticker reads its ISIN's bars, so FB is not left out when Yahoo stores the history under META. The run is never silent about it: the execution stores one line per last-bar date in `Execution.notices`, and `fbull backtest run` folds them only when it prints, with a `⚠ ` prefix: one or two dates print one line each, three or more print one line with the total (`12 symbols had no prices before the run ended and were left out. Run with --verbose for the list.`), `fbull --verbose` adds every stored line under it, and a line names at most five symbols, then `and N more`. The stored lines stay complete for other readers, e.g. `3 symbols had no prices after 2026-03-31 and were left out: ABC, DEF, GHI.` or `1 symbol had no prices after 2026-03-31 and was left out: ABC.` When the portfolio the session reads on the day a member is left out holds any symbol of a line, Zipline closes that position at the end of the session at the last close (§8), a trade the strategy never placed, so the line ends `Positions held were closed at the last price on 2026-03-31.` with the line's last-price date. The folded line then ends `Positions held were closed at their last price.`, before the `--verbose` hint. A line with no held symbol keeps the copy without it. A failed read of those last bars fails the run before Zipline starts. A symbol-list backtest (§7.3) is not filtered this way.

### 📝 7.3 Symbol-list backtests

A backtest created from a symbol list (`fbull backtest create --file`, for example `examples/demo.yaml`) has no `universe_member` rows. Its universe is the enabled `backtest_period_asset` rows of the period whose `[start_date, end_date]` holds the UTC date of `t`, sorted, each symbol once. That is the set `DownloadFinancialData` and `SQLIngester` put in the bundle. A disabled asset has no bars, so it is not passed.

A share class is stored under one name. `BRK.B`, `BRK-B`, `BRK/B` and `BRKB` in the file are all stored, downloaded and read as `BRKB` (`finance.StoredSymbol`). Only the classes listed in `shareClassTickers` (`pkg/finance/internal/suppliers/marketdata/symbol.go`, e.g. `BRKA`, `BRKB`, `BFB`) are renamed. Any other symbol, such as `VOD.L`, is stored exactly as typed. A bar read (`GetOHLC`, the session's bar lookup) for `BRK.B` returns the `BRKB` bars, labelled `BRKB`.

The session decides once, before the first bar: any `universe_member` row for the backtest means §7.2, and an empty universe at `t` stays `[]`. No rows means this section. A failed read of the backtest or its membership fails the run before Zipline starts.

`RunBacktestRequest.benchmark` is the stored `backtest.benchmark` for both kinds. An empty benchmark makes Zipline use a flat zero series, so `benchmark_period_return` would be stored as `0`.

### 7.4 Bars of a renamed ticker

Bars stay on the ticker the provider stores them under, and the reader picks them by date through `asset_symbol`. Yahoo has no bars under FB for any date and serves the whole history, before the rename too, under META. Other providers may list FB up to the rename and META from it.

- **Download.** When the asked ticker returns no bars, the download looks up the instrument's other tickers in `asset_symbol` and the provider's current ticker for that instrument, fetches under those, stores bars under the ticker that answered, and records it in `asset_symbol` when it was missing. A member with no bars under any of its tickers, or any other per-symbol download `STATUS_ERROR` (GetAsset, store, …), fails `fbull backtest create` (the backtest is removed); it is not left out silently.
- **Zipline bundle.** `SQLIngester` builds one sid per ISIN alias by alias. Inside an alias's `asset_symbol` dates its own bars win, and a session without one takes the bar another ticker of the ISIN has. The first alias reaches back to the backtest start and the last one forward to its end.
- **User code.** `Asset.stock_data`, `Assets` and the close `Portfolio.order_value` uses resolve the ticker to the ISIN that holds it on the read date. On each bar time the ticker whose dates cover it wins, then the asked ticker, then another ticker of the ISIN. The frame keeps the asked ticker. A ticker with no `asset_symbol` row reads only its own bars.
- **Positions.** A held position is named by the ticker that started last on or before the session, so it reads FB before the rename and META from it, as the universe does. Zipline itself names an asset by its latest ticker.
- **Orders.** An order placed by ticker resolves to the ISIN that holds that ticker on the session the order is placed in, not on the backtest's last session. A ticker later reused by another ISIN still fills on the earlier asset.

### 7.5 Missing prices

A provider can have no price for a session a stock traded. Yahoo sends `null` in its price arrays for such a session.

- **Download.** A bar with any of open, high, low, or close missing, zero, negative, or non-finite is never stored as `0`. From Yahoo, any such bar in the answer fails `GetOHLC` with a `MissingPriceError` naming the symbol and every such day, whether it is the first, a middle, or the last bar. After the one retry no bars of the symbol are stored and no later ticker is tried. The symbol's `STATUS_ERROR` reads `yahoo sent no price for XYZ on 2024-02-14, 2024-02-20`, under the request symbol, and its days join the check below. A reported day always fails create and is named, even one that isn't a session, such as a one-off closure: the symbol's download failed, so the ingest never runs without it. Such a day isn't called a trading day in the copy. The end date is inclusive for every provider (`inclusiveFetchEnd` in `session_close.go`). Yahoo's `period2` is exclusive, and Alpaca stamps a daily bar at midnight New York (04:00 or 05:00 UTC), after an `end` of 00:00 UTC on the same date, so both ask through the midnight New York after the end date (Alpaca's inclusive `end` stops just before it). Stooq's `d2` is an inclusive date and stays the end date. All three return the same last date for a past end, and none returns a bar after the end date. Today's unfinished session is the only one left out, and it is never asked for. Every provider's fetch (Yahoo, Alpaca and Stooq) ends at the last completed XNYS session: until 16:30 America/New_York, 30 minutes after the regular 16:00 close so the closing auction and the provider's revisions are in the daily bar, it stops at midnight New York time, and from 16:30 today's session is included (`lastCompletedSessionEnd` in `session_close.go`). Alpaca's priced bar for today and Stooq's row for today are never stored before 16:30. A bar for a session after the last completed one is dropped even if the provider sends it, as Yahoo's chart API sometimes does with today's live bar past `period2`, so it is neither stored nor reported as a missing price (`afterLastSession`, logged at debug level). Stooq drops rows after its clamped `d2` with the same helper. Stooq's other exchanges follow the XNYS close too. A holiday has no bar. An early-close day counts as completed only at 16:30, which can leave a finished session out for those hours but never lets an unfinished one in.
- **Missing session.** An XNYS session of the backtest period (`periods[0].start_date` … `periods[-1].end_date`), up to the last completed session, that has no stored bar for a symbol. The first and last sessions count like every other: a symbol with no bar there fails create. A symbol may start late only with a recorded listing date after the period start, and end early only with a confirmed delisting, and neither is stored yet. The provider's first or last bar is not evidence of either, and neither is ETF membership. An XNYS holiday, such as Good Friday 2024-03-29, is not a session. A stored row with a non-positive price (written before `ohlc` refused them) is not a bar, so its session is missing. Nothing is filled with the previous close.
- **Limit: no listing or delisting dates yet.** `asset` stores only a symbol and a name, so no listing or delisting date is stored. Until they are stored, any gap at either end of the period fails create, a real IPO or delisting inside the period included. ETF membership doesn't count as evidence. The delisting/IPO auto-close path in §7.2 and §8 is disabled until then. Sessions before a symbol's first stored bar (over all stored time for the provider) read `NEWCO has no prices before 2024-02-15`, and sessions after its last read `no prices after 2024-03-27`.
- **Check.** `DownloadFinancialData` runs it once the download stream ends without error, before `Ingest` writes anything. It caps the period's end at the last completed session (`finance.LastCompletedSessionDate`), as every fetch is. It reads the span and the stored bar days (whole UTC days of the period) of every ingest symbol (§7.1) for the backtest's provider, under the symbol's stored name (`finance.StoredSymbol`) and every other ticker of its instrument in `asset_symbol`, joined. It takes the sessions from Zipline's calendar service (`ListCalendarDates`, `XNYS`), the calendar the bundle is written with, over the engine's control connection. It collects every cause of every symbol, missing sessions inside the span and gaps at either edge, then fails the `DOWNLOADING` step once and removes the backtest. There is no flag to skip it. A symbol with no stored bar under any of its instrument's tickers fails create here, so the ingest is never left to notice it. A failed bar read or calendar call fails create with the ingest failure copy.
- **User.** The step's message is the whole error. It names the request symbols (after ticker-pattern validation), never provider text. A run of missing sessions in a row reads `A to B`, even across a weekend or holiday. Every day is shown, with no `and N more`. One symbol fits one sentence: `Couldn't load data for backtest 'oef-2024q1': yahoo is missing prices for XYZ on 2024-02-14, a trading day. The backtest wasn't saved. Try another provider with --provider, or change the backtest dates, then run `fbull backtest create` again.` With several days the end of the first sentence reads `on 3 trading days: 2024-02-14, 2024-02-20 to 2024-02-21.` Two or more symbols put one line per symbol under the lead, then the next step:

  ```text
  Couldn't load data for backtest 'oef-2024q1': yahoo is missing prices on trading days for 2 symbols. The backtest wasn't saved.
    AAA: 2024-02-14
    BBB: 2024-02-20 to 2024-02-21, 2024-03-05
  Try another provider with --provider, or change the backtest dates, then run `fbull backtest create` again.
  ```

  The benchmark comes first, then the other symbols A to Z: the order a symbol list was written in is not stored. A gap at the start or end of the period has its own copy instead of the provider step. One symbol with that one cause reads `Couldn't load data for backtest 'oef-2024q1': NEWCO has no prices before 2024-02-15. foreverbull doesn't store listing or delisting dates yet, so it can't tell a late listing from missing data. The backtest wasn't saved. Choose a start on or after 2024-02-15, or remove NEWCO, then run `fbull backtest create` again.` At the end it reads `XYZ has no prices after 2024-03-27. foreverbull doesn't store listing or delisting dates yet, so it can't tell a delisting from missing data. The backtest wasn't saved. Choose an end on or before 2024-03-27, or remove XYZ, then run `fbull backtest create` again.` The day named is the first day with prices at the start and the last day with prices at the end; no prices at both ends reads `NEWCO has no prices before 2024-02-15 or after 2024-03-20.` A symbol with no stored bar at all fails too: `yahoo has no prices for XYZ between 2024-01-02 and 2024-03-28.`, with the check-the-symbol step. A period with no session to check fails once, before any provider is asked. When the requested period, before the cap, holds a real XNYS session that hasn't closed yet (today's open session or a future weekday, read from Zipline's calendar): `no trading session between 2026-10-30 and 2026-10-30 has closed yet (sessions count as closed at 16:30 New York time).` with `Choose a start on or before 2026-10-29, then run `fbull backtest create` again.`, naming the last closed session; the time comes from the same constant the fetches stop at (`finance.NoClosedSessionReason`). Otherwise the period has no trading day, past or not, such as a weekend asked for on that weekend: `there's no trading session between 2024-03-30 and 2024-03-31.` with `Choose dates that include a trading day, then run `fbull backtest create` again.` More causes put one line per symbol under the lead, then the reason and the step:

  ```text
  Couldn't load data for backtest 'oef-2024q1': yahoo is missing prices on trading days for 2 symbols. The backtest wasn't saved.
    NEWCO: no prices before 2024-02-15 or after 2024-03-20.
    OLDCO: no prices after 2024-03-01.
  foreverbull doesn't store listing or delisting dates yet, so it can't tell a late listing or a delisting from missing data. Choose a start on or after 2024-02-15 and an end on or before 2024-03-01, or remove NEWCO and OLDCO, then run `fbull backtest create` again.
  ```

  When no start and end leave a period every symbol has prices for (one symbol's first day is after another's last), the step is only `Remove AAA and BBB, then run `fbull backtest create` again.` With a missing session inside a span too, the step is the provider step above, and the symbol's line reads `NEWCO: no prices before 2024-02-15, and none on 2024-03-04.` The tickers of one instrument in `asset_symbol` are read together, since a bar may be stored under any of them (§7.4), and named once by the latest: `META (earlier FB)`. A ticker's bars count only inside the dates it was that instrument's ticker, so a bar under a reused ticker from another company's time never fills a gap; the latest ticker also covers the instrument's earlier dates, since a provider keeps a renamed instrument's history under its current ticker (Yahoo keeps FB's under META). A ticker that `asset_symbol` gives to more than one instrument is never grouped: asked for, it is read alone on its own bars and fails like any other symbol.

  When the backtest couldn't be removed, every create failure says so the same way: `The backtest wasn't saved.` gives way to `The backtest was partly saved and couldn't be removed.`, then `Run `fbull backtest delete oef-2024q1` first.` as its own sentence, then the step the error gives when the backtest was removed, exactly as it reads: `Run `fbull backtest delete oef-2024q1` first. Choose a start on or after 2024-02-15, or remove NEWCO, then run `fbull backtest create` again.` The ingest copy's `Nothing was saved, so run `fbull backtest create` again once it's fixed.` reads `Run `fbull backtest create` again once it's fixed.` after the delete sentence.
- **Zipline bundle.** `SQLIngester` refuses sids with missing sessions instead of filling them, in case a bar went missing after the check. It reads every sid first and raises one `MissingSessionsError` naming each ticker's days. The ingest answers `DATA_LOSS` with the reason `missing prices on trading days: AAA 2024-02-14; BBB 2024-02-20,2024-03-05`. The broker reads that reason and fails `INGESTING` with the same sentence as the check, removing the backtest. If the reason names a ticker that isn't in the backtest or a day that isn't a session, the sentence would be short, so the ingest failure copy is used instead, as for any other `DATA_LOSS` text or a failed calendar read. Every `DATA_LOSS` outcome is logged with Zipline's cause, passwords in URLs redacted.
- **Notices.** `IngestResponse.notices` and `Step.notices` stay for other notices; the ingest sends none today. Notices never ride in an error: the stream `State.error`, the orchestration step's `error`, and `Step.message` are set only when the step failed, so a completed step with notices still reads as a success to a client that does not know `notices`.

## 8. Zipline `auto_close_date`

> **Disabled until listing and delisting dates are stored.** The delisting/IPO auto-close below does not run on a gap inside the period for now. Until listing and delisting dates are stored, create fails on a gap at either end of the period (§7.5, the edge-gap rule), and ETF membership doesn't count as evidence. Until then, `auto_close_date` is only ever the membership drop or the day after the bundle's last day.

`SQLIngester.create_metadata` already declares `("auto_close_date", "datetime64[ns]")`. The writer builds `autoclose_date = end_date + pd.Timedelta(days=1)` then sets it to `None`.

Contract:

- One Zipline sid per ISIN. Each ticker of the ISIN is a symbol mapping of that sid (FB then META).
- A ticker's `start_date` / `end_date` = first and last bar the sid has inside that ticker's `asset_symbol` dates (§7.4), not the bars stored under the ticker.
- `auto_close_date` follows the broker's rule in §7.2. When the sid's last bar (`end_date`) is before the newest bar of any member (the benchmark does not count), it is the day after that bar, or the final membership drop when that is earlier. Otherwise the stored data ends there, not the asset: it is the final drop, or with no final drop the day after the bundle's last day, so a benchmark session that no member has does not sell a held member. Do not overwrite with `None`. A member delisted mid-month has its last bar before the month-end drop, so Zipline closes a held position on the first session after that bar, the session the broker leaves the member out of `Process`, at the last close, and credits the cash. The portfolio value does not jump, and the position does not sit at a stale price until the drop.
- The drop is the end of the member's final `universe_member` interval, not its first one. A member dropped and added back has one row per interval. While its final interval is open only the end of its bars auto-closes it, so a position held across the gap stays held. A member added back and dropped again closes on its final drop. This carries the gap with look-ahead: the session uses the later re-add to keep the position through the gap, while the strategy cannot see or trade the member until it is added back. A point-in-time close on each drop is a planned follow-up.
- Write the column as `auto_close_date` to match the metadata dtype, not `autoclose_date`.
- A drop closes membership in `universe_member` and, once bars end, auto-closes the Zipline equity. A rename does not auto-close the sid: a held position carries over and changes name on the rename date; the ISIN remains a member.

## 9. `CreateBacktestFromETF` after this contract

1. Resolve `[start_date, end_date]` to sampled as-of dates (§5.2).
2. Read S0, the last list published on or before `start_date`, from the month-ends (and cached as-of dates) in the three months before it, newest first (§6.2).
3. Fill `etf_holdings_snapshot` from cache or iShares. A gap is filled from the latest list on or before it, S0 included.
4. Move S0 to `start_date` and drop as-of dates before it. Only when step 2 found nothing does the first list after the start seed it, with a warning naming the ETF, the start, and the as-of used.
5. Diff → `asset_symbol` + `universe_member` (§6). The first interval opens at the backtest start.
6. `backtest.benchmark` from `GetBenchmark`.
7. Ingest union (§7.1). Session PIT is a runtime read, not a stored yearly list.
8. After the download and before the ingest, fail create on any missing session (§7.5). Every symbol's missing days are named in one error, and nothing is saved.

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

Landed. ETF create samples month-end iShares as-of dates (filesystem JSON cache; Equity rows with ISIN). Identity is ISIN (`instrument` + `asset_symbol`); ticker is a dated alias (FB then META is one asset). Membership is half-open `universe_member` intervals from consecutive as-of diffs, seeded at the backtest start from the last list published on or before it: adds/drops change membership, ticker changes do not. Ingest unions every alias plus the benchmark; the session maps members through `asset_symbol` at `t` into `Process` (empty universe stays `[]`). A symbol-list backtest has no membership rows and passes its enabled period assets instead (§7.3). SQLIngester writes `auto_close_date` on a drop and keeps one sid across a rename. `GetAsset` failure is not a membership drop. Yearly `backtest_period` / `backtest_period_asset` rows remain for ingest windows; they are not membership.

Not in this work: reintroducing `MARKET_DATA_PROVIDER`; daily holdings dumps; the `etf_holdings_snapshot` table (raw JSON cache is enough); hysteresis; rewriting `ohlc` off `symbol`.
