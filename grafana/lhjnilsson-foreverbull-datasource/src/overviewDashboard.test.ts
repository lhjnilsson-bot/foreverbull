import fs from 'fs';
import path from 'path';

interface DashboardLink {
  url?: string;
  title?: string;
  tooltip?: string;
}

interface DashboardTarget {
  queryType?: string;
  backtest?: string;
  execution?: { id?: string };
  metrics?: Array<{ name?: string }>;
}

interface DashboardPanel {
  title?: string;
  type?: string;
  targets?: DashboardTarget[];
}

interface DashboardFile {
  uid?: string;
  title?: string;
  time?: { from?: string; to?: string };
  links?: DashboardLink[];
  panels?: DashboardPanel[];
  templating?: { list?: Array<{ name?: string; allValue?: string; includeAll?: boolean }> };
}

function readDashboard(name: string): { raw: string; json: DashboardFile } {
  const file = path.join(__dirname, '..', 'provisioning', 'dashboards', name);
  const raw = fs.readFileSync(file, 'utf8');
  return { raw, json: JSON.parse(raw) as DashboardFile };
}

describe('backtest overview dashboard', () => {
  const overview = readDashboard('backtest-overview.json');
  const execution = readDashboard('executions.json');
  const compare = readDashboard('strategy-compare.json');

  it('is a separate board with a two-year window', () => {
    expect(overview.json.uid).toBe('fb-backtest-overview');
    expect(overview.json.title).toBe('Backtest overview');
    expect(overview.json.time).toEqual({ from: 'now-2y', to: 'now' });
    expect(overview.raw).not.toContain('now-30y');
    expect(overview.raw).not.toContain('executionId');
    expect(overview.json.uid).not.toBe(execution.json.uid);
    expect(overview.json.uid).not.toBe(compare.json.uid);
    expect(execution.json.uid).toBe('ee9ep4jxtwkcgf');
    expect(compare.json.uid).toBe('fb-strategy-compare');
  });

  it('lists overlapping runs with ListExecutionOverview and links each row back', () => {
    expect(overview.raw).toContain('ListExecutionOverview');
    expect(overview.raw).toContain('/d/ee9ep4jxtwkcgf?var-execution=${__data.fields.execution}');
    expect(overview.raw).toContain('var-backtest=${__data.fields.backtest}');
    expect(overview.raw).toContain('percentunit');
    expect(overview.raw).toContain('currencyUSD');
    expect(overview.raw).toContain('starting_portfolio_value');
    expect(overview.raw).toContain('"value": "Start"');
    expect(overview.raw).toContain('"value": "End"');
    expect(overview.raw).toContain('"value": "Start value"');
    expect(overview.raw).toContain('"value": "End value"');
    expect(overview.raw).toContain('"value": "Total return"');
    expect(overview.raw).toContain('"value": "Benchmark"');
    expect(overview.raw).toContain('"value": "Sharpe"');
    expect(overview.raw).toContain('"value": "Sortino"');
    expect(overview.raw).toContain('"value": "Max drawdown"');
    expect(overview.raw).toContain('active_days');
    expect(overview.raw).toContain('"value": "Active days"');
    expect(overview.raw).not.toContain('Starting portfolio value');
    expect(overview.raw).not.toContain('Ending portfolio value');
    expect(overview.raw).not.toContain('Start date');
    expect(overview.raw).not.toContain('End date');
    expect(overview.raw).not.toContain('Algorithm period return');
    const targets = (overview.json.panels ?? []).flatMap((panel) => panel.targets ?? []);
    expect(targets.length).toBeGreaterThan(0);
    for (const target of targets) {
      expect(target.queryType).toBe('ListExecutionOverview');
      expect(target.backtest).toBe('$backtest');
      expect(target.execution).toBeUndefined();
    }
    const backtest = (overview.json.templating?.list ?? []).find((variable) => variable.name === 'backtest');
    expect(backtest?.includeAll).toBe(true);
    expect(backtest?.allValue).toBe('*');
  });

  it('keeps the execution page link to this board', () => {
    const link = (execution.json.links ?? []).find((item) => item.url === '/d/fb-backtest-overview');
    expect(link?.title).toBe('Backtest overview');
    expect(execution.raw).toContain('ee9ep4jxtwkcgf');
  });

  it('links each board back to foreverbull when the web origin is set', () => {
    const executionLink = (execution.json.links ?? []).find((item) => item.title === 'Open in foreverbull');
    const overviewLink = (overview.json.links ?? []).find((item) => item.title === 'Open in foreverbull');
    expect(executionLink?.url).toBe('__FOREVERBULL_WEB_URL__${web_path}');
    expect(executionLink?.tooltip).toBe('Open this backtest in foreverbull to run it again or share it.');
    expect(overviewLink?.url).toBe('__FOREVERBULL_WEB_URL__/backtests');
    expect(overviewLink?.tooltip).toBe('Create a backtest or start a run in foreverbull.');
    expect(execution.raw).not.toContain('/backtests/');
    expect(execution.raw).not.toContain('Open the run');
    expect(compare.raw).not.toContain('Open in foreverbull');
  });

  it('shows total and price return side by side on the run page, the benchmark too', () => {
    const metrics = (title: string) =>
      (execution.json.panels ?? [])
        .find((panel) => panel.title === title)
        ?.targets?.flatMap((target) => (target.metrics ?? []).map((metric) => metric.name));
    expect(metrics('Total return · Price return')).toEqual(['total_return', 'price_return']);
    expect(metrics('Daily returns')).toEqual(['returns', 'price_returns']);
    expect(metrics('Benchmark total return · price return')).toEqual(['benchmark', 'benchmark_price_return']);
    expect(execution.raw).toContain('dividends paid on the ex-date');
    expect(execution.raw).toContain('adjusted-close return (dividends inside prices)');
  });
});
