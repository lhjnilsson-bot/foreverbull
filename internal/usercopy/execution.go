package usercopy

import "fmt"

// ExecutionReadFailed is the sentence for an execution that couldn't be
// read, when it isn't known to be missing.
func ExecutionReadFailed(executionID, command string) string {
	return fmt.Sprintf("Couldn't read execution %s. %s", executionID, RetryStep(command))
}
