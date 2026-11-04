import type { ExecutionResult } from "./types";

// The result card reads the same stored periods and prints the same strings
// as the `fbull backtest run` summary, so one execution shows one set of
// numbers on every surface. Grafana's overview uses the same field map.

export type CardCell = {
  label: string;
  value: string;
};

export type ResultCard = {
  // Pairs in the CLI order: Start · End, Return · Benchmark, Data source,
  // Sharpe · Sortino, Max drawdown · Active days.
  rows: CardCell[][];
  // Set when no stored session held a long or a short.
  noTrades?: string[];
};

export type ResultCardLoad = { ok: true; card: ResultCard } | { ok: false; message: string };

export type ResultCardInput = {
  // Stored session days only (hasStoredDate), oldest first.
  stored: readonly ExecutionResult[];
  benchmark: string;
  provider: string;
};

// Zipline reports returns as fractions, so 0.042 prints as +4.2%.
export function formatPercent(fraction: number): string {
  const percent = fraction * 100;
  const sign = percent < 0 || Object.is(percent, -0) ? "-" : "+";
  return `${sign}${Math.abs(percent).toFixed(1)}%`;
}

// Drawdown is at most zero, so 0 prints as 0.0% rather than +0.0%.
export function formatDrawdown(fraction: number): string {
  const percent = fraction * 100;
  if (Number(percent.toFixed(1)) === 0) return "0.0%";
  return `${percent.toFixed(1)}%`;
}

// Portfolio dollars: two decimals and thousands separators. A minus sits before the dollar sign.
export function formatUsd(amount: number): string {
  const text = Math.abs(amount).toLocaleString("en-US", { minimumFractionDigits: 2, maximumFractionDigits: 2 });
  if (amount < 0) return `-$${text}`;
  return `$${text}`;
}

// A missing ratio stays a dash. A stored 0 is a real ratio and prints as 0.00.
function formatRatio(ratio: number | undefined): string {
  if (typeof ratio !== "number") return "-";
  return ratio.toFixed(2);
}

function benchmarkCell(benchmark: string, last: ExecutionResult): CardCell {
  if (!benchmark) return { label: "Benchmark", value: "not set" };
  const label = `Benchmark (${benchmark})`;
  if (typeof last.benchmarkPeriodReturn !== "number") return { label, value: "not available" };
  return { label, value: formatPercent(last.benchmarkPeriodReturn) };
}

// The provider copied onto the execution, not the backtest's current one:
// a backtest can change provider after the run. Values are the --provider
// names the CLI prints, so an unrecognised one is shown as stored.
export function dataSourceValue(provider: string): string {
  // Executions created before the provider column have none stored.
  if (provider === "") return "not recorded";
  return provider;
}

// A session is active when its close still holds a long or a short.
function positionHeld(period: ExecutionResult): boolean {
  return (period.longsCount ?? 0) !== 0 || (period.shortsCount ?? 0) !== 0;
}

// Every field the card prints, with the name the error line uses for it.
const fieldNames = {
  startingCash: "starting cash",
  startingValue: "starting value of positions",
  portfolioValue: "ending portfolio value",
  algorithmPeriodReturn: "return",
  maxDrawdown: "max drawdown",
  longsCount: "number of long positions",
  shortsCount: "number of short positions",
} as const;

export type RequiredField = keyof typeof fieldNames;

const requiredFirst: readonly RequiredField[] = ["startingCash", "startingValue"];
const requiredLast: readonly RequiredField[] = ["portfolioValue", "algorithmPeriodReturn", "maxDrawdown"];
const requiredEach: readonly RequiredField[] = ["longsCount", "shortsCount"];

function finite(value: unknown): boolean {
  return typeof value === "number" && Number.isFinite(value);
}

// Every field the card prints must be a stored finite number. A missing one
// is named instead of printed as 0, so a broken row never reads as a flat run.
function missingFields(stored: readonly ExecutionResult[]): RequiredField[] {
  const first = stored[0];
  const last = stored[stored.length - 1];
  const missing = new Set<RequiredField>();
  for (const field of requiredFirst) if (!first || !finite(first[field])) missing.add(field);
  for (const field of requiredLast) if (!last || !finite(last[field])) missing.add(field);
  for (const period of stored) {
    for (const field of requiredEach) if (!finite(period[field])) missing.add(field);
  }
  return [...missing];
}

// "A", "A and B", "A, B and C": no comma before the last name.
function joinNames(names: readonly string[]): string {
  if (names.length <= 1) return names.join("");
  return `${names.slice(0, -1).join(", ")} and ${names[names.length - 1]}`;
}

export function missingFieldsMessage(fields: readonly RequiredField[]): string {
  const names = joinNames(fields.map((field) => fieldNames[field]));
  return (
    `Couldn't show this run's result: the stored result is missing the ${names}. ` +
    "Please report it at https://github.com/lhjnilsson/foreverbull/issues with this message."
  );
}

export function noTradesLine(dayCount: number): string {
  if (dayCount === 1) return "No trades: the strategy held no position on its 1 trading day.";
  return `No trades: the strategy held no position on any of the ${dayCount} trading days.`;
}

export function resultCard({ stored, benchmark, provider }: ResultCardInput): ResultCardLoad | undefined {
  const first = stored[0];
  const last = stored[stored.length - 1];
  if (!first || !last) return undefined;
  const missing = missingFields(stored);
  if (missing.length > 0) {
    return { ok: false, message: missingFieldsMessage(missing) };
  }
  const start = (first.startingCash ?? 0) + (first.startingValue ?? 0);
  const activeDays = stored.filter(positionHeld).length;
  const card: ResultCard = {
    rows: [
      [
        { label: "Start", value: formatUsd(start) },
        { label: "End", value: formatUsd(last.portfolioValue ?? 0) },
      ],
      [{ label: "Return", value: formatPercent(last.algorithmPeriodReturn ?? 0) }, benchmarkCell(benchmark, last)],
      [{ label: "Data source", value: dataSourceValue(provider) }],
      [
        { label: "Sharpe", value: formatRatio(last.sharpe) },
        { label: "Sortino", value: formatRatio(last.sortino) },
      ],
      [
        { label: "Max drawdown", value: formatDrawdown(last.maxDrawdown ?? 0) },
        { label: "Active days", value: `${activeDays} of ${stored.length}` },
      ],
    ],
  };
  if (activeDays === 0) {
    // Without this a tradeless run reads as a broken one: zeros and dashes, no reason.
    card.noTrades = [
      noTradesLine(stored.length),
      "Return and drawdown stay at 0 and Sharpe and Sortino are not defined.",
    ];
  }
  return { ok: true, card };
}
