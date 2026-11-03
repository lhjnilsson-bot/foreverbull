package command

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The whole step message when the resolutions couldn't be recorded: the
// retry step when the backtest was removed, and the delete-first form with
// the same cause when it couldn't be.
func TestLaterTickerNotRecordedMessage(t *testing.T) {
	require.Equal(t, "Couldn't load data for backtest 'oef-2024q1': foreverbull couldn't save which later "+
		"tickers were used for some symbols. The backtest wasn't saved. Run `fbenv status` to check that the "+
		"environment is running, then run `fbull backtest create` again.",
		laterTickerNotRecordedMessage("oef-2024q1", true))
	require.Equal(t, "Couldn't load data for backtest 'oef-2024q1': foreverbull couldn't save which later "+
		"tickers were used for some symbols. The backtest was partly saved and couldn't be removed. Run "+
		"`fbull backtest delete oef-2024q1` first. Run `fbenv status` to check that the environment is "+
		"running, then run `fbull backtest create` again.",
		laterTickerNotRecordedMessage("oef-2024q1", false))
}
