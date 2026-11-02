package auth

import "errors"

// ErrUseNotRecorded matches a request stopped because the last use of its
// token or browser session couldn't be written.
var ErrUseNotRecorded = errors.New("use not recorded")

// useNotRecordedError is a failed last-use write. It reads "record token
// use: <cause>" and matches both ErrUseNotRecorded and the cause.
type useNotRecordedError struct {
	what string
	err  error
}

func (e *useNotRecordedError) Error() string {
	return "record " + e.what + " use: " + e.err.Error()
}

func (e *useNotRecordedError) Unwrap() []error {
	return []error{ErrUseNotRecorded, e.err}
}
