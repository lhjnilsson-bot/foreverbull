package usercopy

import (
	"fmt"
	"strings"
)

// Plain causes of a failed experiment run, in the order they happen.
const (
	CauseRunFailed          = "the backtest run failed"
	CauseExecutionNotStored = "the run couldn't be recorded"
	CauseFailureNotStored   = "the failure couldn't be saved"
	CauseNotReloaded        = "the saved run couldn't be read back"
	CauseNotSent            = "fbull couldn't be told which step failed"
	// CauseStartNotSent is a run that never started: fbull couldn't be
	// told it was running, so it isn't started.
	CauseStartNotSent = "fbull couldn't be told the run had started"
)

// ExperimentCommand is the command that runs an experiment of strategy on
// backtest.
func ExperimentCommand(strategy, backtest string) string {
	return fmt.Sprintf("fbull strategy experiment %s --backtest %s", strategy, backtest)
}

// ExperimentRunFailed is the sentence for an experiment that couldn't run.
// One or two causes fit the sentence ("A and B."); three or more go one per
// line under the lead, each ending with a period.
func ExperimentRunFailed(strategy, backtest string, causes []string) string {
	step := RetryStep(ExperimentCommand(strategy, backtest))
	if len(causes) >= 3 {
		return fmt.Sprintf("Couldn't run experiment '%s':\n  %s.\n%s", strategy, strings.Join(causes, ".\n  "), step)
	}
	return fmt.Sprintf("Couldn't run experiment '%s': %s. %s", strategy, strings.Join(causes, " and "), step)
}

// DeploymentStartFailed is the sentence for a deployment whose runner
// couldn't start. notStored is a failure that couldn't be saved either: the
// deployment still reads RUNNING and would resume on the next start, so the
// step stops it before deploying again.
func DeploymentStartFailed(strategy string, notStored bool) string {
	if notStored {
		return fmt.Sprintf("Couldn't start deployment '%s', and the failure couldn't be saved, so the deployment still shows as running. "+
			"Run `fbenv status` to check that the environment is running, "+
			"then run `fbull strategy stop %s`, then `fbull strategy deploy %s`.", strategy, strategy, strategy)
	}
	return fmt.Sprintf("Couldn't start deployment '%s'. %s", strategy, RetryStep("fbull strategy deploy "+strategy))
}

// DeploymentStopFailed is the sentence for a deployment whose runner
// couldn't stop.
func DeploymentStopFailed(strategy string) string {
	return fmt.Sprintf("Couldn't stop deployment '%s'. %s", strategy, RetryStep("fbull strategy stop "+strategy))
}

// ExecutionBacktestCheckFailed is the sentence when the check that the
// caller may read the backtest of an execution couldn't run. It surfaces
// only on the GetExperiment API, which no fbull command calls.
func ExecutionBacktestCheckFailed(executionID, backtest string) string {
	return fmt.Sprintf("Couldn't check that execution %s belongs to backtest '%s'. %s", executionID, backtest, RetryStep(""))
}
