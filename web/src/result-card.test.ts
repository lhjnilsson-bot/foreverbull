import assert from "node:assert/strict";
import { test } from "node:test";
import { hasStoredDate } from "./equity.ts";
import {
  dataSourceValue,
  formatDrawdown,
  formatPercent,
  formatUsd,
  missingFieldsMessage,
  noTradesLine,
  resultCard,
} from "./result-card.ts";
import type { ExecutionResult } from "./types.ts";

// Protojson with EmitDefaultValues: every non-optional scalar is present,
// unset optional ratios and benchmark return are absent.
function period(day: number, fields: Partial<ExecutionResult>): ExecutionResult {
  return {
    date: { year: 2024, month: 1, day },
    portfolioValue: 0,
    longsCount: 0,
    shortsCount: 0,
    startingValue: 0,
    endingValue: 0,
    startingCash: 0,
    maxDrawdown: 0,
    algorithmPeriodReturn: 0,
    ...fields,
  };
}

function cells(load: ReturnType<typeof resultCard>): string[][] {
  assert.ok(load?.ok, "expected a card");
  return load.card.rows.map((row) => row.map((cell) => `${cell.label}=${cell.value}`));
}

test("the card prints the CLI golden row from the first open and the last stored period", () => {
  const first = period(2, {
    startingCash: 100_000,
    startingValue: 0,
    portfolioValue: 101_000,
    endingValue: 50_000,
    algorithmPeriodReturn: 0.01,
    maxDrawdown: -0.01,
    sharpe: 9,
  });
  const heldShort = period(3, { shortsCount: 1 });
  const last = period(4, {
    startingCash: 40_000,
    startingValue: 62_000,
    endingValue: 90_000,
    portfolioValue: 104_200,
    algorithmPeriodReturn: 0.042,
    maxDrawdown: -0.068,
    sharpe: 1.35,
    sortino: 1.1,
    benchmarkPeriodReturn: 0.031,
    longsCount: 2,
  });
  const load = resultCard({ stored: [first, heldShort, last], benchmark: "SPY", provider: "yahoo" });
  assert.deepEqual(cells(load), [
    ["Start=$100,000.00", "End=$104,200.00"],
    ["Return=+4.2%", "Benchmark (SPY)=+3.1%"],
    ["Data source=yahoo"],
    ["Sharpe=1.35", "Sortino=1.10"],
    ["Max drawdown=-6.8%", "Active days=2 of 3"],
  ]);
  assert.ok(load?.ok);
  assert.equal(load.card.noTrades, undefined);
});

test("a tradeless run keeps zeros, dashes for ratios, and the No trades lines", () => {
  const stored = [period(2, { startingCash: 100_000, portfolioValue: 100_000 }), period(3, { portfolioValue: 100_000 })];
  const load = resultCard({ stored, benchmark: "", provider: "stooq" });
  assert.deepEqual(cells(load), [
    ["Start=$100,000.00", "End=$100,000.00"],
    ["Return=+0.0%", "Benchmark=not set"],
    ["Data source=stooq"],
    ["Sharpe=-", "Sortino=-"],
    ["Max drawdown=0.0%", "Active days=0 of 2"],
  ]);
  assert.ok(load?.ok);
  assert.deepEqual(load.card.noTrades, [
    "No trades: the strategy held no position on any of the 2 trading days.",
    "Return and drawdown stay at 0 and Sharpe and Sortino are not defined.",
  ]);
});

test("a one-day run with no trades uses the singular", () => {
  const load = resultCard({ stored: [period(2, { startingCash: 1_000 })], benchmark: "", provider: "yahoo" });
  assert.ok(load?.ok);
  assert.deepEqual(load.card.noTrades, [
    "No trades: the strategy held no position on its 1 trading day.",
    "Return and drawdown stay at 0 and Sharpe and Sortino are not defined.",
  ]);
  assert.equal(noTradesLine(1), "No trades: the strategy held no position on its 1 trading day.");
  assert.equal(noTradesLine(2), "No trades: the strategy held no position on any of the 2 trading days.");
  assert.equal(noTradesLine(250), "No trades: the strategy held no position on any of the 250 trading days.");
});

test("a stored zero ratio prints and a set benchmark without a return is not available", () => {
  const stored = [period(2, { startingCash: 1_000, sharpe: 0, longsCount: 1 })];
  const rows = cells(resultCard({ stored, benchmark: "QQQ", provider: "alpaca" }));
  assert.deepEqual(rows[1], ["Return=+0.0%", "Benchmark (QQQ)=not available"]);
  assert.deepEqual(rows[2], ["Data source=alpaca"]);
  assert.deepEqual(rows[3], ["Sharpe=0.00", "Sortino=-"]);
});

test("no stored session day is no card, not a card of zeros", () => {
  const emptyJoin = { ...period(2, {}), date: { year: 1, month: 1, day: 1 } };
  assert.equal(resultCard({ stored: [emptyJoin].filter(hasStoredDate), benchmark: "", provider: "yahoo" }), undefined);
  assert.equal(resultCard({ stored: [], benchmark: "", provider: "yahoo" }), undefined);
});

test("a missing or non-finite printed field is refused and named", () => {
  const broken = period(2, { startingCash: 1_000 });
  delete broken.algorithmPeriodReturn;
  delete broken.shortsCount;
  broken.maxDrawdown = Number.NaN;
  assert.deepEqual(resultCard({ stored: [broken], benchmark: "", provider: "yahoo" }), {
    ok: false,
    message:
      "Couldn't show this run's result: the stored result is missing the return, max drawdown " +
      "and number of short positions. " +
      "Please report it at https://github.com/lhjnilsson/foreverbull/issues with this message.",
  });
});

test("the missing list names one, two, or three fields without an Oxford comma", () => {
  const tail = " Please report it at https://github.com/lhjnilsson/foreverbull/issues with this message.";
  assert.equal(
    missingFieldsMessage(["algorithmPeriodReturn"]),
    "Couldn't show this run's result: the stored result is missing the return." + tail,
  );
  assert.equal(
    missingFieldsMessage(["algorithmPeriodReturn", "maxDrawdown"]),
    "Couldn't show this run's result: the stored result is missing the return and max drawdown." + tail,
  );
  assert.equal(
    missingFieldsMessage(["algorithmPeriodReturn", "maxDrawdown", "shortsCount"]),
    "Couldn't show this run's result: the stored result is missing the return, max drawdown " +
      "and number of short positions." +
      tail,
  );
});

test("every required field has a provisional name", () => {
  assert.equal(
    missingFieldsMessage([
      "startingCash",
      "startingValue",
      "portfolioValue",
      "algorithmPeriodReturn",
      "maxDrawdown",
      "longsCount",
      "shortsCount",
    ]),
    "Couldn't show this run's result: the stored result is missing the starting cash, " +
      "starting value of positions, ending portfolio value, return, max drawdown, number of long positions " +
      "and number of short positions. " +
      "Please report it at https://github.com/lhjnilsson/foreverbull/issues with this message.",
  );
});

test("the data source names the execution's provider and never guesses one", () => {
  assert.equal(dataSourceValue("yahoo"), "yahoo");
  assert.equal(dataSourceValue("stooq"), "stooq");
  assert.equal(dataSourceValue("alpaca"), "alpaca");
  assert.equal(dataSourceValue(""), "not recorded");
  assert.equal(dataSourceValue("polygon"), "polygon");
});

test("formatting matches the CLI helpers", () => {
  assert.equal(formatPercent(0.042), "+4.2%");
  assert.equal(formatPercent(-0.01), "-1.0%");
  assert.equal(formatPercent(0), "+0.0%");
  assert.equal(formatDrawdown(0), "0.0%");
  assert.equal(formatDrawdown(-0.0001), "0.0%");
  assert.equal(formatDrawdown(-0.068), "-6.8%");
  assert.equal(formatUsd(104_200), "$104,200.00");
  assert.equal(formatUsd(100_000.126), "$100,000.13");
  assert.equal(formatUsd(0), "$0.00");
  assert.equal(formatUsd(-12.3), "-$12.30");
});

test("a total-return run shows total and price return with the CLI labels", () => {
  const first = period(2, { startingCash: 100_000, portfolioValue: 100_000 });
  const last = period(4, {
    portfolioValue: 112_812.4,
    algorithmPeriodReturn: 0.128124,
    priceReturn: 0.111963,
    maxDrawdown: -0.05,
    sharpe: 1.2,
    sortino: 1.0,
    benchmarkPeriodReturn: 0.0912,
    benchmarkPriceReturn: 0.0805,
    longsCount: 1,
  });
  const load = resultCard({
    stored: [first, last],
    benchmark: "SPY",
    provider: "yahoo",
    returnBasis: "RETURN_BASIS_TOTAL",
    foreignDividendsGross: 2,
  });
  assert.deepEqual(cells(load), [
    ["Start=$100,000.00", "End=$112,812.40"],
    ["Total return=+12.8%", "Benchmark (SPY) total=+9.1%"],
    ["Price return=+11.2%", "Benchmark (SPY) price=+8.1%"],
    ["Data source=yahoo"],
    ["Sharpe=1.20", "Sortino=1.00"],
    ["Max drawdown=-5.0%", "Active days=1 of 2"],
  ]);
  assert.ok(load?.ok);
  assert.deepEqual(load.card.notes, [
    "Dividends paid on the ex-date: the cash lands then, not on the pay date.",
    "2 foreign dividends booked gross, before withholding.",
  ]);
});

test("an adjusted-close run labels the return and notes dividends inside prices", () => {
  const first = period(2, { startingCash: 100_000, portfolioValue: 100_000 });
  const last = period(4, {
    portfolioValue: 110_000,
    algorithmPeriodReturn: 0.1,
    maxDrawdown: -0.02,
    longsCount: 1,
  });
  const load = resultCard({
    stored: [first, last],
    benchmark: "SPY",
    provider: "stooq",
    returnBasis: "RETURN_BASIS_ADJUSTED_CLOSE",
  });
  assert.deepEqual(cells(load), [
    ["Start=$100,000.00", "End=$110,000.00"],
    ["Adjusted-close return=+10.0%", "Benchmark (SPY)=not available"],
    ["Data source=stooq"],
    ["Sharpe=-", "Sortino=-"],
    ["Max drawdown=-2.0%", "Active days=1 of 2"],
  ]);
  assert.ok(load?.ok);
  assert.deepEqual(load.card.notes, [
    "Adjusted-close return (dividends inside prices): no dividends were booked as cash.",
  ]);
});
