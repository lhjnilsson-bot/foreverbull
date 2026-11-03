package stream

import (
	"errors"

	"github.com/lhjnilsson/foreverbull/internal/usercopy"
	"github.com/nats-io/nats.go"
)

// ErrServicesUnreachable is a command that failed because one service
// couldn't reach another, or couldn't store what the other needs to find
// it. A command's state carries only its plain cause, so the API that
// started the command can name what failed for the user.
var ErrServicesUnreachable = errors.New(usercopy.ServicesUnreachable)

// ErrStateNotRecorded is a task the foreverbull stream refused, or one of
// whose states it refused, because the stream is full. The task is failed
// with this cause instead of leaving its waiter on a state that was never
// stored.
var ErrStateNotRecorded = errors.New(usercopy.TaskStateNotRecorded)

// jsStreamStoreFailed is the JetStream error code for a publish the stream
// couldn't store, such as "maximum bytes exceeded" on a full stream with
// DiscardNew.
const jsStreamStoreFailed nats.ErrorCode = 10077

// streamFull reports whether err is the stream refusing a publish because it
// is full.
func streamFull(err error) bool {
	var apiErr *nats.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode == jsStreamStoreFailed
}

// refused reports whether err is a publish the stream certainly didn't
// store: an error the stream answered with, such as a full stream, or no
// stream answering at all. Any other error, such as a lost ack
// (nats.ErrTimeout), a deadline, or a dropped connection, can come back
// after the server stored the message.
//
// Every APIError is a refusal only because the foreverbull stream has a
// single replica (Replicas unset): the one server that answers is the one
// that stores. With replicas, an error such as a leader change can answer a
// publish the stream still stores, and this needs revisiting.
func refused(err error) bool {
	var apiErr *nats.APIError
	return errors.As(err, &apiErr) ||
		errors.Is(err, nats.ErrNoStreamResponse) || errors.Is(err, nats.ErrNoResponders)
}

// notRecorded marks err, a publish a full stream refused, as
// ErrStateNotRecorded. Its text is the plain cause, because it becomes a
// step's message; the NATS error stays in the chain for the log.
func notRecorded(err error) error {
	return &notRecordedError{err: err}
}

type notRecordedError struct{ err error }

func (e *notRecordedError) Error() string   { return usercopy.TaskStateNotRecorded }
func (e *notRecordedError) Unwrap() []error { return []error{ErrStateNotRecorded, e.err} }

// Unreachable marks err as ErrServicesUnreachable. Its text stays err's, so
// the log keeps the whole chain.
func Unreachable(err error) error {
	return &unreachableError{err: err}
}

type unreachableError struct{ err error }

func (e *unreachableError) Error() string   { return e.err.Error() }
func (e *unreachableError) Unwrap() []error { return []error{ErrServicesUnreachable, e.err} }

// stateError is the error a command's ERROR state carries: the plain cause
// for an unreachable service or a full stream, the error itself otherwise.
func stateError(err error) error {
	switch {
	case errors.Is(err, ErrServicesUnreachable):
		return ErrServicesUnreachable
	case errors.Is(err, ErrStateNotRecorded):
		return ErrStateNotRecorded
	}
	return err
}

// commandError is the error a publisher returns for an ERROR state's text.
func commandError(text string) error {
	switch text {
	case usercopy.ServicesUnreachable:
		return ErrServicesUnreachable
	case usercopy.TaskStateNotRecorded:
		return ErrStateNotRecorded
	}
	return errors.New(text)
}
