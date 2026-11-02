package stream

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStateMessageID(t *testing.T) {
	id, err := stateMessageID("foreverbull.backtest.session.run.m-1.state")
	require.NoError(t, err)
	require.Equal(t, "m-1", id)

	_, err = stateMessageID("foreverbull.backtest.state")
	require.EqualError(t, err, `state subject "foreverbull.backtest.state" isn't foreverbull.<module>.<component>.<method>.<id>.state`)
}
