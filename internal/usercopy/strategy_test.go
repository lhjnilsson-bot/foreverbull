package usercopy_test

import (
	"testing"

	"github.com/lhjnilsson/foreverbull/internal/usercopy"
	"github.com/stretchr/testify/require"
)

func TestExperimentRunFailed(t *testing.T) {
	step := "Run `fbenv status` to check that the environment is running, " +
		"then run `fbull strategy experiment mean-rev --backtest spy` again."
	require.Equal(t, "Couldn't run experiment 'mean-rev': the backtest run failed. "+step,
		usercopy.ExperimentRunFailed("mean-rev", "spy", []string{usercopy.CauseRunFailed}))
	require.Equal(t, "Couldn't run experiment 'mean-rev': the backtest run failed and the failure couldn't be saved. "+step,
		usercopy.ExperimentRunFailed("mean-rev", "spy", []string{usercopy.CauseRunFailed, usercopy.CauseFailureNotStored}))
	require.Equal(t, "Couldn't run experiment 'mean-rev':\n"+
		"  the backtest run failed.\n"+
		"  the failure couldn't be saved.\n"+
		"  fbull couldn't be told which step failed.\n"+step,
		usercopy.ExperimentRunFailed("mean-rev", "spy",
			[]string{usercopy.CauseRunFailed, usercopy.CauseFailureNotStored, usercopy.CauseNotSent}))
}

func TestDeploymentSentences(t *testing.T) {
	require.Equal(t, "Couldn't start deployment 'mean-rev'. Run `fbenv status` to check that the environment is running, "+
		"then run `fbull strategy deploy mean-rev` again.", usercopy.DeploymentStartFailed("mean-rev", false))
	require.Equal(t, "Couldn't start deployment 'mean-rev', and the failure couldn't be saved, so the deployment still shows as running. "+
		"Run `fbenv status` to check that the environment is running, "+
		"then run `fbull strategy stop mean-rev`, then `fbull strategy deploy mean-rev`.",
		usercopy.DeploymentStartFailed("mean-rev", true))
	require.Equal(t, "Couldn't stop deployment 'mean-rev'. Run `fbenv status` to check that the environment is running, "+
		"then run `fbull strategy stop mean-rev` again.", usercopy.DeploymentStopFailed("mean-rev"))
}
