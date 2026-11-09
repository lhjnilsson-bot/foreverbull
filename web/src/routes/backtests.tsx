import { type ChangeEvent, type FormEvent, useState } from "react";
import { Link, Navigate, Outlet, useMatch, useNavigate, useOutletContext } from "react-router-dom";
import { ArrowUpRight, LoaderCircle } from "lucide-react";
import { canCreate } from "../access";
import { Grants } from "../auth";
import { useAuth } from "../auth-context";
import { EquityChart } from "../components/equity-chart";
import {
  Detail,
  DetailHeading,
  Empty,
  Field,
  FormError,
  H2,
  ListPane,
  Modal,
  buttonClass,
  fmtDate,
  inputClass,
  rowActive,
  rowClass,
} from "../components/fb";
import { hasStoredDate, lastStoredPeriod, portfolioSeries } from "../equity";
import { openRunInGrafana, rankRunsInGrafana } from "../grafana";
import { type ResultCard, resultCard } from "../result-card";
import type { Backtest, Execution, ExecutionResult, GetExecution } from "../types";
import { postJson, useJson, type Load } from "../useJson";

type BacktestList = {
  load: Load<Backtest[]>;
  refresh: () => void;
};

function rowsOf(load: Load<Backtest[]>): Backtest[] | null {
  if (load.state !== "ready" || !Array.isArray(load.data)) return null;
  return load.data;
}

export function BacktestsLayout() {
  const navigate = useNavigate();
  const match = useMatch("/backtests/:name");
  const name = match?.params.name;
  const { state } = useAuth();
  const [revision, setRevision] = useState(0);
  const load = useJson<Backtest[]>("/api/backtests", revision);
  const rows = rowsOf(load);
  const refresh = () => setRevision((current) => current + 1);

  return (
    <>
      <ListPane
        title="Backtests"
        count={rows ? rows.length : undefined}
        newLabel="New backtest"
        newTitle="Create this backtest."
        allowCreate={canCreate(state.status, state.status === "in" ? state.account.role : undefined)}
        newForm={(close) => (
          <CreateBacktest
            onDone={close}
            onCreated={(created) => {
              refresh();
              navigate(`/backtests/${encodeURIComponent(created)}`);
            }}
          />
        )}
      >
        {load.state === "error" && load.message ? (
          <li className="px-5 py-6 text-muted-foreground">{load.message}</li>
        ) : null}
        {rows && rows.length === 0 ? <Empty text="Create a backtest." /> : null}
        {(rows ?? []).map((backtest) => (
          <BacktestRow key={backtest.name} backtest={backtest} selected={backtest.name === name} />
        ))}
      </ListPane>
      <Outlet context={{ load, refresh } satisfies BacktestList} />
    </>
  );
}

export function BacktestIndex() {
  const { load } = useOutletContext<BacktestList>();
  const rows = rowsOf(load);
  const first = rows?.find((backtest) => backtest.name)?.name;
  if (first) return <Navigate to={`/backtests/${encodeURIComponent(first)}`} replace />;
  return (
    <Detail k="none">
      <p className="text-muted-foreground">Select a backtest.</p>
    </Detail>
  );
}

const num = (n: number) => n.toLocaleString("en-US", { maximumFractionDigits: 2 });

const pct = (n: number) => `${(n * 100).toFixed(2)}%`;

// List rows always include result. A year-1 date and zero metrics are the empty join, not a stored period.
function listHasResult(e: Execution): boolean {
  const result = e.result;
  if (!result) return false;
  const year = result.date?.year;
  if (typeof year === "number" && year > 1) return true;
  if (typeof result.sharpe === "number") return true;
  if (typeof result.portfolioValue === "number" && result.portfolioValue !== 0) return true;
  if (typeof result.endingValue === "number" && result.endingValue !== 0) return true;
  if (typeof result.maxDrawdown === "number" && result.maxDrawdown !== 0) return true;
  return false;
}

function latestStored(runs: Execution[]): ExecutionResult | undefined {
  for (let index = runs.length - 1; index >= 0; index -= 1) {
    const execution = runs[index];
    if (execution && listHasResult(execution)) return execution.result;
  }
  return undefined;
}

function BacktestRow({ backtest, selected }: { backtest: Backtest; selected: boolean }) {
  const load = useJson<Execution[]>(
    backtest.name ? `/api/backtests/${encodeURIComponent(backtest.name)}/executions` : null,
  );
  const runs = load.state === "ready" && Array.isArray(load.data) ? load.data : null;
  const stored = runs ? latestStored(runs) : undefined;
  const sharpe = stored?.sharpe;
  const start = backtest.periods?.[0]?.startDate;
  return (
    <li>
      <Link
        to={backtest.name ? `/backtests/${encodeURIComponent(backtest.name)}` : "/backtests"}
        className={`${rowClass} ${selected ? rowActive : ""}`}
      >
        <div className="flex items-baseline justify-between gap-3">
          <span className="min-w-0 truncate font-mono text-[13px]">{backtest.name}</span>
          {backtest.provider ? (
            <span className="font-mono text-xs text-muted-foreground">{backtest.provider}</span>
          ) : null}
        </div>
        {backtest.ownerLogin ? (
          <span className="mt-1 block truncate text-xs text-muted-foreground">{backtest.ownerLogin}</span>
        ) : null}
        {runs ? (
          <div className="mt-2 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted-foreground">
            <span>
              {runs.length} {runs.length === 1 ? "run" : "runs"}
            </span>
            {typeof sharpe === "number" ? <span className="font-mono">Sharpe {sharpe.toFixed(2)}</span> : null}
            {runs.length > 0 && !stored ? <span>no result</span> : null}
            {runs.length === 0 ? <span>{fmtDate(start)}</span> : null}
          </div>
        ) : null}
      </Link>
    </li>
  );
}

export function BacktestDetail() {
  const match = useMatch("/backtests/:name");
  const name = match?.params.name ?? "";
  const { state } = useAuth();
  const { load: listLoad } = useOutletContext<BacktestList>();
  const listed = rowsOf(listLoad)?.find((item) => item.name === name);
  const [execRevision, setExecRevision] = useState(0);
  const config = useJson<{ grafanaUrl?: unknown }>("/api/config");
  const grafanaBase =
    config.state === "ready" && typeof config.data.grafanaUrl === "string" ? config.data.grafanaUrl : null;
  const backtest = useJson<Backtest>(name ? `/api/backtests/${encodeURIComponent(name)}` : null);
  const executions = useJson<Execution[]>(
    name ? `/api/backtests/${encodeURIComponent(name)}/executions` : null,
    execRevision,
  );

  if (!name) return <Detail k="none" />;
  if (backtest.state === "error") {
    const missing = backtest.message === "not found" || !backtest.message;
    return (
      <Detail k="missing">
        <p className="text-muted-foreground">{missing ? `No backtest named ${name}.` : backtest.message}</p>
      </Detail>
    );
  }
  const shown = backtest.state === "ready" ? backtest.data : listed;
  if (!shown?.name) return <Detail k={name} />;

  const b = shown;
  const periods = b.periods ?? [];
  const p = periods[0];
  const assets = periods.flatMap((period) => period.assets ?? []);
  const on = assets.filter((asset) => asset.enabled).map((asset) => asset.symbol ?? "");
  const off = assets.filter((asset) => !asset.enabled).map((asset) => asset.symbol ?? "");
  const runs = executions.state === "ready" && Array.isArray(executions.data) ? executions.data : [];
  const rank = b.name ? rankRunsInGrafana(grafanaBase ?? "", b.name) : null;

  return (
    <Detail k={b.name ?? name}>
      <DetailHeading title={b.name ?? name} subtitle="Backtest" />
      <dl>
        <Field label="Name">{b.name}</Field>
        {b.ownerLogin ? <Field label="Owner">{b.ownerLogin}</Field> : null}
        <Field label="Start">{fmtDate(p?.startDate)}</Field>
        <Field label="End">{fmtDate(p?.endDate)}</Field>
        <Field label="Benchmark">{b.benchmark || "—"}</Field>
        <Field label="Provider">{b.provider || "—"}</Field>
      </dl>
      <H2>Universe</H2>
      <dl>
        <Field label="Enabled">{on.filter(Boolean).join(", ") || "—"}</Field>
        <Field label="Disabled">{off.filter(Boolean).join(", ") || "—"}</Field>
      </dl>
      <H2>Executions</H2>
      {rank ? (
        <p className="mb-3">
          <GrafanaLink href={rank.href}>{rank.label}</GrafanaLink>
        </p>
      ) : null}
      {executions.state === "error" && executions.message ? (
        <p className="text-muted-foreground">{executions.message}</p>
      ) : null}
      <ul>
        {runs.map((execution) => (
          <ExecutionRow
            key={execution.id}
            execution={execution}
            revision={execRevision}
            benchmark={b.benchmark ?? ""}
            grafanaBase={grafanaBase}
          />
        ))}
      </ul>
      {canCreate(state.status, state.status === "in" ? state.account.role : undefined) ? (
        <RunForm backtest={b.name ?? name} onStarted={() => setExecRevision((current) => current + 1)} />
      ) : null}
      {b.name ? <Grants kind="backtest" name={b.name} ownerLogin={b.ownerLogin} /> : null}
    </Detail>
  );
}

function GrafanaLink({ href, children }: { href: string; children: string }) {
  return (
    <a href={href} className="inline-flex items-center gap-1 text-primary" target="_blank" rel="noopener noreferrer">
      {children}
      <ArrowUpRight size={14} aria-hidden />
    </a>
  );
}

function ExecutionRow({
  execution,
  revision,
  benchmark,
  grafanaBase,
}: {
  execution: Execution;
  revision: number;
  benchmark: string;
  grafanaBase: string | null;
}) {
  const id = execution.id ?? "";
  const run = openRunInGrafana(grafanaBase ?? "", id);
  const detail = useJson<GetExecution>(id ? `/api/executions/${encodeURIComponent(id)}` : null, revision);
  const periods = detail.state === "ready" && Array.isArray(detail.data.periods) ? detail.data.periods : [];
  const fromGet = lastStoredPeriod(periods) !== undefined;
  const fromList = listHasResult(execution);
  const stored = fromGet || fromList;
  const settled = fromList || detail.state === "ready" || detail.state === "error";
  const status = stored ? "stored" : settled ? "no result" : "";
  const source: ExecutionResult | undefined = fromGet ? lastStoredPeriod(periods) : fromList ? execution.result : undefined;
  const sharpe = source?.sharpe;
  const drawdown = source?.maxDrawdown;
  const endValue = source?.endingValue;
  const [open, setOpen] = useState(false);

  return (
    <li className="animate-enter border-b py-3 font-mono text-[13px]">
      <div className="flex flex-wrap items-baseline gap-x-5 gap-y-1">
        <span className="break-all">{id}</span>
        {status ? <span className={stored ? "text-status-ready" : "text-muted-foreground"}>{status}</span> : null}
        <span className="text-muted-foreground">
          {fmtDate(execution.startDate)} → {fmtDate(execution.endDate)}
        </span>
        {id ? (
          <span className="ml-auto inline-flex items-center gap-4">
            <button type="button" className="text-primary" onClick={() => setOpen(true)}>
              Portfolio
            </button>
            {run ? <GrafanaLink href={run.href}>{run.label}</GrafanaLink> : null}
          </span>
        ) : null}
      </div>
      {stored && (typeof sharpe === "number" || typeof drawdown === "number" || typeof endValue === "number") ? (
        <div className="mt-1 flex gap-5 text-muted-foreground">
          {typeof sharpe === "number" ? <span>sharpe {sharpe.toFixed(2)}</span> : null}
          {typeof drawdown === "number" ? <span>max drawdown {pct(drawdown)}</span> : null}
          {typeof endValue === "number" ? <span>end value {num(endValue)}</span> : null}
        </div>
      ) : null}
      <Modal open={open} onClose={() => setOpen(false)} title={id ? `Run ${id}` : "Run"} wide>
        <RunReport
          execution={execution}
          detail={detail}
          benchmark={benchmark}
          stored={stored}
          status={status}
          runHref={run?.href ?? null}
          runLabel={run?.label ?? ""}
        />
      </Modal>
    </li>
  );
}

function RunReport({
  execution,
  detail,
  benchmark,
  stored,
  status,
  runHref,
  runLabel,
}: {
  execution: Execution;
  detail: Load<GetExecution>;
  benchmark: string;
  stored: boolean;
  status: string;
  runHref: string | null;
  runLabel: string;
}) {
  const periods = detail.state === "ready" && Array.isArray(detail.data.periods) ? detail.data.periods : [];
  const points = portfolioSeries(periods);
  const provider = detail.state === "ready" ? (detail.data.provider ?? "") : "";
  // The run's basis comes with the stored execution, not the list row.
  const storedExecution = detail.state === "ready" ? detail.data.execution : undefined;
  // Only the GET periods carry the first open and every session's positions,
  // so the list row's last result never stands in for the card.
  const card = resultCard({
    stored: periods.filter(hasStoredDate),
    benchmark,
    provider,
    returnBasis: storedExecution?.returnBasis,
    foreignDividendsGross: storedExecution?.foreignDividendsGross,
  });
  return (
    <div className="grid gap-8">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <dl className="min-w-0 flex-1">
          <Field label="Execution">{execution.id || "—"}</Field>
          <Field label="Backtest">{execution.backtest || "—"}</Field>
          <Field label="Session">{execution.session || "—"}</Field>
          <Field label="Period">
            {fmtDate(execution.startDate)} → {fmtDate(execution.endDate)}
          </Field>
          <Field label="Status">
            <span className={stored ? "text-status-ready" : "text-muted-foreground"}>{status || "—"}</span>
          </Field>
        </dl>
        {runHref ? <GrafanaLink href={runHref}>{runLabel}</GrafanaLink> : null}
      </div>
      {card?.ok ? <ResultCardView card={card.card} /> : null}
      {card && !card.ok ? <p className="text-muted-foreground">{card.message}</p> : null}
      {!card && detail.state === "ready" ? <p className="text-muted-foreground">No result stored for this run.</p> : null}
      {detail.state === "loading" ? (
        <p className="flex items-center gap-2 text-muted-foreground">
          <LoaderCircle className="animate-spin motion-reduce:animate-none" size={16} aria-hidden />
          Loading the stored series.
        </p>
      ) : null}
      {detail.state === "error" && detail.message ? <p className="text-muted-foreground">{detail.message}</p> : null}
      {points.length >= 2 ? (
        <div>
          <h3 className="mb-5 font-medium">Portfolio value</h3>
          <EquityChart points={points} />
        </div>
      ) : null}
    </div>
  );
}

function ResultCardView({ card }: { card: ResultCard }) {
  return (
    <div className="grid gap-4 border-y py-6">
      <dl className="grid grid-cols-2 gap-x-8 gap-y-5">
        {card.rows.flatMap((row) =>
          row.map((cell, index) => (
            <Metric key={cell.label} label={cell.label} value={cell.value} first={index === 0} />
          )),
        )}
      </dl>
      {card.notes ? (
        <div className="text-muted-foreground">
          {card.notes.map((line) => (
            <p key={line}>{line}</p>
          ))}
        </div>
      ) : null}
      {card.noTrades ? (
        <div className="text-muted-foreground">
          {card.noTrades.map((line) => (
            <p key={line}>{line}</p>
          ))}
        </div>
      ) : null}
    </div>
  );
}

function Metric({ label, value, first }: { label: string; value: string; first: boolean }) {
  return (
    <div className={`py-1 ${first ? "col-start-1" : ""}`}>
      <dt className="text-xs text-muted-foreground">{label}</dt>
      <dd className="mt-2 font-mono text-2xl font-medium">{value}</dd>
    </div>
  );
}

function RunForm({ backtest, onStarted }: { backtest: string; onStarted: () => void }) {
  const [service, setService] = useState("");
  const [error, setError] = useState("");
  const [pending, setPending] = useState(false);

  async function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const trimmed = service.trim();
    if (pending || !trimmed) return;
    setError("");
    setPending(true);
    const result = await postJson(`/api/backtests/${encodeURIComponent(backtest)}/executions`, { service: trimmed });
    setPending(false);
    if (!result.ok) {
      setError(result.message);
      return;
    }
    setService("");
    onStarted();
  }

  return (
    <form className="mt-6 max-w-md" onSubmit={onSubmit}>
      <label className="mb-1 block text-muted-foreground" htmlFor="svc">
        Service image
      </label>
      <input
        id="svc"
        className={inputClass}
        value={service}
        spellCheck={false}
        autoComplete="off"
        onChange={(event) => setService(event.target.value)}
      />
      <button className={`${buttonClass} mt-4`} disabled={!service.trim() || pending}>
        Run this service on the backtest.
      </button>
      <FormError message={error} />
    </form>
  );
}

function CreateBacktest({ onDone, onCreated }: { onDone: () => void; onCreated: (name: string) => void }) {
  const [f, setF] = useState({ name: "", start: "", end: "", provider: "" });
  const [error, setError] = useState("");
  const [pending, setPending] = useState(false);
  const set =
    (key: keyof typeof f) => (event: ChangeEvent<HTMLInputElement | HTMLSelectElement>) =>
      setF({ ...f, [key]: event.target.value });
  const ok = f.name.trim() && f.start && f.end;

  async function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (pending || !ok) return;
    setError("");
    setPending(true);
    const body: { name: string; start: string; end: string; provider?: string } = {
      name: f.name.trim(),
      start: f.start,
      end: f.end,
    };
    // An empty provider is omitted. The server stores yahoo.
    if (f.provider) body.provider = f.provider;
    const result = await postJson("/api/backtests", body);
    setPending(false);
    if (!result.ok) {
      setError(result.message);
      return;
    }
    const created = f.name.trim();
    setF({ name: "", start: "", end: "", provider: "" });
    onCreated(created);
    onDone();
  }

  return (
    <form
      className="grid max-w-md gap-3"
      onSubmit={(event) => {
        void onSubmit(event);
      }}
    >
      <label className="grid gap-1">
        <span className="text-muted-foreground">Name</span>
        <input className={inputClass} value={f.name} spellCheck={false} autoComplete="off" onChange={set("name")} />
      </label>
      <div className="grid grid-cols-2 gap-3">
        <label className="grid gap-1">
          <span className="text-muted-foreground">Start</span>
          <input type="date" className={inputClass} value={f.start} onChange={set("start")} />
        </label>
        <label className="grid gap-1">
          <span className="text-muted-foreground">End</span>
          <input type="date" className={inputClass} value={f.end} onChange={set("end")} />
        </label>
      </div>
      <label className="grid gap-1">
        <span className="text-muted-foreground">Provider</span>
        <select className={inputClass} value={f.provider} onChange={set("provider")}>
          <option value="">—</option>
          <option value="yahoo">yahoo</option>
          <option value="stooq">stooq</option>
          <option value="alpaca">alpaca</option>
        </select>
      </label>
      <div>
        <button className={`${buttonClass} mt-1`} disabled={!ok || pending}>
          Create this backtest.
        </button>
        <FormError message={error} />
      </div>
    </form>
  );
}
