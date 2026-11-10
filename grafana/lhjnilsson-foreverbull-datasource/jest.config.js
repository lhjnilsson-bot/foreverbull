// force timezone to UTC to allow tests to work regardless of local timezone
// generally used by snapshots, but can affect specific tests
process.env.TZ = 'UTC';

const { grafanaESModules, nodeModulesToTransform } = require('./.config/jest/utils');

module.exports = {
  // Jest configuration provided by Grafana scaffolding
  ...require('./.config/jest.config'),
  // The copied Grafana list predates these ESM-only packages. @grafana/data
  // and @grafana/ui require them, and Jest on Node 22 cannot load them unless
  // they are transformed.
  transformIgnorePatterns: [
    nodeModulesToTransform([
      ...grafanaESModules,
      '@react-hookz/web',
      '@ver0/deep-equal',
      '@wojtekmaj/date-utils',
      'get-user-locale',
      'marked',
      'memoize',
      'mimic-function',
      'react-calendar',
    ]),
  ],
};
