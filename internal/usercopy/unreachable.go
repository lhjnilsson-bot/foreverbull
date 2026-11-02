package usercopy

import "fmt"

// ServicesUnreachable is the plain cause when one foreverbull service
// couldn't reach another, or couldn't store what the other needs to find
// it.
const ServicesUnreachable = "foreverbull's services couldn't reach each other"

// BacktestRunUnreachable is the sentence for a backtest run whose session
// couldn't start because the services couldn't reach each other.
func BacktestRunUnreachable(backtest, command string) string {
	return fmt.Sprintf("Couldn't run backtest '%s': %s. %s", backtest, ServicesUnreachable, RetryStep(command))
}

// TradingSessionUnreachable is the sentence for a trading session that
// couldn't start because the services couldn't reach each other. A trading
// session has no name, so it is named by its id.
func TradingSessionUnreachable(sessionID string) string {
	return fmt.Sprintf("Couldn't start trading session '%s': %s. %s", sessionID, ServicesUnreachable, RetryStep("fbull trading run"))
}
