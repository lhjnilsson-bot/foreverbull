import assert from "node:assert/strict";
import { test } from "node:test";
import { axisMoney, dividendNotes, lastStoredPeriod, portfolioSeries, returnMetrics } from "./equity.ts";
import type { ExecutionResult } from "./types.ts";

const day = (year: number, portfolioValue?: number, benchmarkPeriodReturn?: number): ExecutionResult => ({
  date: { year, month: 1, day: 2 },
  portfolioValue,
  benchmarkPeriodReturn,
});

test("an empty join is not a portfolio series", () => {
  const periods = [day(1, 0, 0)];
  assert.deepEqual(portfolioSeries(periods), []);
  assert.equal(lastStoredPeriod(periods), undefined);
});

test("stored days keep portfolio value and ignore benchmark return", () => {
  const periods = [day(2024, 100_000, 0), day(2024, 110_000, 0.04)];
  periods[1] = { ...periods[1], date: { year: 2024, month: 1, day: 3 } };
  assert.deepEqual(portfolioSeries(periods), [
    { date: "2024-01-02", portfolioValue: 100_000 },
    { date: "2024-01-03", portfolioValue: 110_000 },
  ]);
  assert.equal(lastStoredPeriod(periods)?.portfolioValue, 110_000);
});

test("a stored day without a finite portfolio value is omitted from the line", () => {
  const periods = [day(2024, Number.NaN), { date: { year: 2024, month: 2, day: 1 }, sharpe: 1.2 }];
  assert.deepEqual(portfolioSeries(periods), []);
  assert.equal(lastStoredPeriod(periods)?.sharpe, 1.2);
});

test("axis labels stay in currency units", () => {
  assert.equal(axisMoney(980), "980");
  assert.equal(axisMoney(12_500), "12.5k");
  assert.equal(axisMoney(2_000_000), "2m");
});

const koEnd: ExecutionResult = { algorithmPeriodReturn: 0.128124, priceReturn: 0.111963 };

test("a total-return run shows total and price return side by side", () => {
  assert.deepEqual(returnMetrics({ returnBasis: "RETURN_BASIS_TOTAL" }, koEnd), [
    { label: "Total return", value: 0.128124 },
    { label: "Price return", value: 0.111963 },
  ]);
});

test("a stooq run has one adjusted-close return and a legacy run the plain one", () => {
  assert.deepEqual(returnMetrics({ returnBasis: "RETURN_BASIS_ADJUSTED_CLOSE" }, koEnd), [
    { label: "Adjusted-close return", value: 0.128124 },
  ]);
  assert.deepEqual(returnMetrics({ returnBasis: "RETURN_BASIS_UNSPECIFIED" }, koEnd), [{ label: "Return", value: 0.128124 }]);
  assert.deepEqual(returnMetrics({}, undefined), []);
});

test("dividend notes say when the cash lands and count the foreign dividends", () => {
  assert.deepEqual(dividendNotes({ returnBasis: "RETURN_BASIS_TOTAL", foreignDividendsGross: 2 }), [
    "Dividends paid on the ex-date: the cash lands then, not on the pay date.",
    "2 foreign dividends booked gross, before withholding.",
  ]);
  assert.deepEqual(dividendNotes({ returnBasis: "RETURN_BASIS_TOTAL", foreignDividendsGross: 1 }), [
    "Dividends paid on the ex-date: the cash lands then, not on the pay date.",
    "1 foreign dividend booked gross, before withholding.",
  ]);
  assert.deepEqual(dividendNotes({ returnBasis: "RETURN_BASIS_ADJUSTED_CLOSE", foreignDividendsGross: 2 }), [
    "Adjusted-close return (dividends inside prices): no dividends were booked as cash.",
  ]);
  assert.deepEqual(dividendNotes({ returnBasis: "RETURN_BASIS_UNSPECIFIED" }), []);
});

test("the benchmark's total and price return follow the run's, named by its ticker", () => {
  const withSpy: ExecutionResult = { ...koEnd, benchmarkPeriodReturn: 0.0912, benchmarkPriceReturn: 0.0805 };
  assert.deepEqual(returnMetrics({ returnBasis: "RETURN_BASIS_TOTAL" }, withSpy, "SPY"), [
    { label: "Total return", value: 0.128124 },
    { label: "Price return", value: 0.111963 },
    { label: "SPY total return", value: 0.0912 },
    { label: "SPY price return", value: 0.0805 },
  ]);
  assert.deepEqual(returnMetrics({ returnBasis: "RETURN_BASIS_TOTAL" }, withSpy, ""), [
    { label: "Total return", value: 0.128124 },
    { label: "Price return", value: 0.111963 },
    { label: "Benchmark total return", value: 0.0912 },
    { label: "Benchmark price return", value: 0.0805 },
  ]);
  assert.deepEqual(returnMetrics({ returnBasis: "RETURN_BASIS_ADJUSTED_CLOSE" }, withSpy, "SPY"), [
    { label: "Adjusted-close return", value: 0.128124 },
    { label: "SPY adjusted-close return", value: 0.0912 },
  ]);
  assert.deepEqual(returnMetrics({ returnBasis: "RETURN_BASIS_UNSPECIFIED" }, withSpy, "SPY"), [
    { label: "Return", value: 0.128124 },
    { label: "SPY", value: 0.0912 },
  ]);
});
