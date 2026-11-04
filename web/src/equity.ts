import type { DateParts, Execution, ExecutionResult } from "./types";

export type EquityPoint = {
  date: string;
  portfolioValue: number;
};

// A year of 1 is the empty SQL join, not a stored session day.
export function hasStoredDate(period: ExecutionResult): boolean {
  const year = period.date?.year;
  const month = period.date?.month;
  const day = period.date?.day;
  return (
    typeof year === "number" &&
    year > 1 &&
    typeof month === "number" &&
    month >= 1 &&
    typeof day === "number" &&
    day >= 1
  );
}

export function portfolioSeries(periods: readonly ExecutionResult[] | undefined): EquityPoint[] {
  const points: EquityPoint[] = [];
  for (const period of periods ?? []) {
    if (!hasStoredDate(period)) continue;
    if (typeof period.portfolioValue !== "number" || !Number.isFinite(period.portfolioValue)) continue;
    points.push({ date: isoDate(period.date), portfolioValue: period.portfolioValue });
  }
  return points;
}

export function lastStoredPeriod(periods: readonly ExecutionResult[] | undefined): ExecutionResult | undefined {
  let last: ExecutionResult | undefined;
  for (const period of periods ?? []) {
    if (hasStoredDate(period)) last = period;
  }
  return last;
}

export function axisMoney(value: number): string {
  const abs = Math.abs(value);
  if (abs >= 1_000_000) return `${compact(value / 1_000_000)}m`;
  if (abs >= 10_000) return `${compact(value / 1_000)}k`;
  return value.toLocaleString("en-US", { maximumFractionDigits: 0 });
}

function compact(value: number): string {
  const rounded = Math.round(value * 10) / 10;
  return Number.isInteger(rounded) ? String(rounded) : rounded.toFixed(1);
}

function isoDate(date: DateParts | undefined): string {
  const year = date?.year ?? 0;
  const month = date?.month ?? 0;
  const day = date?.day ?? 0;
  return `${year}-${String(month).padStart(2, "0")}-${String(day).padStart(2, "0")}`;
}

export type ReturnMetric = { label: string; value: number };

// The run's returns, labelled by what they measure, the same words as the
// CLI summary: total and price return side by side, one adjusted-close
// return for stooq, or the plain return of a run stored before dividends.
// With a benchmark ticker, the benchmark's returns follow under the same
// words: "SPY total return" and "SPY price return".
export function returnMetrics(
  execution: Execution | undefined,
  last: ExecutionResult | undefined,
  benchmark = "",
): ReturnMetric[] {
  if (!last || typeof last.algorithmPeriodReturn !== "number") return [];
  const ticker = benchmark || "Benchmark";
  const metrics: ReturnMetric[] = [];
  const add = (label: string, value: number | undefined) => {
    if (typeof value === "number") metrics.push({ label, value });
  };
  switch (execution?.returnBasis) {
    case "RETURN_BASIS_TOTAL":
      add("Total return", last.algorithmPeriodReturn);
      add("Price return", last.priceReturn);
      add(`${ticker} total return`, last.benchmarkPeriodReturn);
      add(`${ticker} price return`, last.benchmarkPriceReturn);
      return metrics;
    case "RETURN_BASIS_ADJUSTED_CLOSE":
      add("Adjusted-close return", last.algorithmPeriodReturn);
      add(`${ticker} adjusted-close return`, last.benchmarkPeriodReturn);
      return metrics;
    default:
      add("Return", last.algorithmPeriodReturn);
      add(ticker, last.benchmarkPeriodReturn);
      return metrics;
  }
}

export const EX_DATE_NOTE = "Dividends paid on the ex-date: the cash lands then, not on the pay date.";
export const ADJUSTED_CLOSE_NOTE = "Adjusted-close return (dividends inside prices): no dividends were booked as cash.";

// What the run says about dividends under its metrics, the same notes as
// the CLI summary.
export function dividendNotes(execution: Execution | undefined): string[] {
  switch (execution?.returnBasis) {
    case "RETURN_BASIS_TOTAL": {
      const notes = [EX_DATE_NOTE];
      const foreign = execution.foreignDividendsGross ?? 0;
      if (foreign === 1) notes.push("1 foreign dividend booked gross, before withholding.");
      if (foreign > 1) notes.push(`${foreign} foreign dividends booked gross, before withholding.`);
      return notes;
    }
    case "RETURN_BASIS_ADJUSTED_CLOSE":
      return [ADJUSTED_CLOSE_NOTE];
    default:
      return [];
  }
}
