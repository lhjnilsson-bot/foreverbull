package usercopy

import "fmt"

// BacktestStopFailed is the sentence for a backtest whose strategy worker
// pool couldn't be stopped after the run. It doesn't say the results were
// saved: a client may stop the session without storing them.
func BacktestStopFailed(backtest string) string {
	return fmt.Sprintf("Backtest '%s' finished, but foreverbull couldn't stop the processes running your strategy. "+
		"Run `fbenv status` to check the environment before the next run.", backtest)
}

// TradingSessionStopFailed is the sentence for a trading session whose
// strategy worker pool couldn't be stopped. A trading session has no name,
// so it is named by its id.
func TradingSessionStopFailed(sessionID string) string {
	return fmt.Sprintf("Trading session '%s' ended, but foreverbull couldn't stop the processes running your strategy. "+
		"Run `fbenv status` to check the environment before the next session.", sessionID)
}
