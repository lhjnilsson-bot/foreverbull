import assert from 'node:assert/strict';
import { test } from 'node:test';
import { webBacktestPath } from './webPath.ts';

test('an unset backtest links to /backtests with no trailing slash', () => {
  assert.equal(webBacktestPath(undefined), '/backtests');
  assert.equal(webBacktestPath(''), '/backtests');
  assert.equal(webBacktestPath('   '), '/backtests');
  assert.equal(webBacktestPath('$backtest'), '/backtests');
  assert.equal(webBacktestPath('*'), '/backtests');
  assert.equal(webBacktestPath('All'), '/backtests');
});

test('a selected backtest is one encoded path segment', () => {
  assert.equal(webBacktestPath('nasdaq100'), '/backtests/nasdaq100');
  assert.equal(webBacktestPath('eu small'), '/backtests/eu%20small');
  assert.equal(webBacktestPath('a/b'), '/backtests/a%2Fb');
});
