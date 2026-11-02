package stream

import (
	"errors"

	"github.com/lhjnilsson/foreverbull/internal/usercopy"
)

// ErrServicesUnreachable is a command that failed because one service
// couldn't reach another, or couldn't store what the other needs to find
// it. A command's state carries only its plain cause, so the API that
// started the command can name what failed for the user.
var ErrServicesUnreachable = errors.New(usercopy.ServicesUnreachable)

// Unreachable marks err as ErrServicesUnreachable. Its text stays err's, so
// the log keeps the whole chain.
func Unreachable(err error) error {
	return &unreachableError{err: err}
}

type unreachableError struct{ err error }

func (e *unreachableError) Error() string   { return e.err.Error() }
func (e *unreachableError) Unwrap() []error { return []error{ErrServicesUnreachable, e.err} }

// stateError is the error a command's ERROR state carries: the plain cause
// for an unreachable service, the error itself otherwise.
func stateError(err error) error {
	if errors.Is(err, ErrServicesUnreachable) {
		return ErrServicesUnreachable
	}
	return err
}

// commandError is the error a publisher returns for an ERROR state's text.
func commandError(text string) error {
	if text == usercopy.ServicesUnreachable {
		return ErrServicesUnreachable
	}
	return errors.New(text)
}
