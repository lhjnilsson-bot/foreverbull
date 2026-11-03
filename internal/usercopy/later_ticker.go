package usercopy

// LaterTickerNotRecorded is the cause in "Couldn't load data for backtest
// '<name>': <cause> <saved> <next step>" when create read a symbol under a
// later ticker of the same ISIN and couldn't store that on the backtest.
// Create then fails rather than keep a backtest that reads one company
// under another ticker without a word. The exact string is pinned in
// later_ticker_test.go.
const LaterTickerNotRecorded = "foreverbull couldn't save which later tickers were used for some symbols."
