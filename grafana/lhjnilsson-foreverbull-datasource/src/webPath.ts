// Path for the execution dashboard's Open in foreverbull link.
// An unset backtest is /backtests, with no trailing slash. A selected name
// is /backtests/ plus one encoded segment.
export function webBacktestPath(name: string | undefined): string {
  const trimmed = (name ?? '').trim();
  if (trimmed === '' || trimmed.startsWith('$') || trimmed === '*' || trimmed === 'All') {
    return '/backtests';
  }
  return `/backtests/${encodeURIComponent(trimmed)}`;
}
