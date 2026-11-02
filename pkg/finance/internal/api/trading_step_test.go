package api

import (
	"testing"

	streamPb "github.com/lhjnilsson/foreverbull/internal/stream/pb"
	"github.com/lhjnilsson/foreverbull/internal/usercopy"
	"github.com/stretchr/testify/require"
)

// A step that failed because the services couldn't reach each other names
// the trading session; any other step is passed on as it is.
func TestTradingStepNamesUnreachable(t *testing.T) {
	cause := usercopy.ServicesUnreachable
	named := tradingStep(&streamPb.Orchestration_Step{Error: &cause}, "t-1")
	require.Equal(t, "Couldn't start trading session 't-1': foreverbull's services couldn't reach each other. "+
		"Run `fbenv status` to check that the environment is running, then run `fbull trading run` again.", named.GetError())

	other := "error getting trading: not enabled"
	step := &streamPb.Orchestration_Step{Error: &other}
	require.Same(t, step, tradingStep(step, "t-1"))
}
