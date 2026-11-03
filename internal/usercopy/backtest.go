package usercopy

import "fmt"

// BacktestNotSaved says a failed create kept nothing. It is only true when
// the backtest was removed.
const BacktestNotSaved = "The backtest wasn't saved."

// BacktestNotRemoved replaces BacktestNotSaved when the backtest couldn't be
// removed after a failed create.
const BacktestNotRemoved = "The backtest was partly saved and couldn't be removed."

// BacktestDeleteFirst is the next step when the backtest couldn't be
// removed: its own sentence to delete it first, then the step the failure
// gives when it was removed, exactly as that step reads.
func BacktestDeleteFirst(backtest, next string) string {
	return fmt.Sprintf("Run `fbull backtest delete %s` first. %s", backtest, next)
}

// BacktestSavedAndNext is the saved sentence and the next step of a failed
// create: BacktestNotSaved and next when the backtest was removed, otherwise
// BacktestNotRemoved and next behind the delete-first step.
func BacktestSavedAndNext(backtest, next string, removed bool) (string, string) {
	if removed {
		return BacktestNotSaved, next
	}
	return BacktestNotRemoved, BacktestDeleteFirst(backtest, next)
}
