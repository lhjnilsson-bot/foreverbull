import { webBacktestPath } from './webPath';

describe('web backtest path', () => {
  it('an unset backtest links to /backtests with no trailing slash', () => {
    expect(webBacktestPath(undefined)).toBe('/backtests');
    expect(webBacktestPath('')).toBe('/backtests');
    expect(webBacktestPath('   ')).toBe('/backtests');
    expect(webBacktestPath('$backtest')).toBe('/backtests');
    expect(webBacktestPath('*')).toBe('/backtests');
    expect(webBacktestPath('All')).toBe('/backtests');
  });

  it('a selected backtest is one encoded path segment', () => {
    expect(webBacktestPath('nasdaq100')).toBe('/backtests/nasdaq100');
    expect(webBacktestPath('eu small')).toBe('/backtests/eu%20small');
    expect(webBacktestPath('a/b')).toBe('/backtests/a%2Fb');
  });
});
