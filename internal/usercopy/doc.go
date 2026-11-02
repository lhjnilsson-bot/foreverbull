// Package usercopy holds the sentences foreverbull shows a user when
// something fails, so a wording change is a swap in one place. A sentence
// here never carries a Go error chain: the cause goes to the log, and the
// user gets a plain cause and the next step.
package usercopy

import "fmt"

// IssuesURL is where a sentence asks for a report.
const IssuesURL = "https://github.com/lhjnilsson/foreverbull/issues"

// ReportIt ends a sentence for a fault only a fix can clear.
const ReportIt = "Please report it at " + IssuesURL + " with this message."

// RetryStep is the step after a fault the environment may have caused:
// check it, then run command again. With no command, the step is to try
// again.
func RetryStep(command string) string {
	if command == "" {
		return "Run `fbenv status` to check that the environment is running, then try again."
	}
	return fmt.Sprintf("Run `fbenv status` to check that the environment is running, then run `%s` again.", command)
}
