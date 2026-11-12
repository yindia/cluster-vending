// Package fault is how code that is not an API handler says what went
// wrong in a way an API can answer with: a kind and a message for the
// caller. It knows no RPC library; the server maps a kind to its status
// code (service.ToConnect).
package fault

import (
	"errors"
	"fmt"
)

// Kind is what went wrong, as far as the caller is concerned.
type Kind int

// Kinds.
const (
	// Internal: the platform failed; the message says nothing sensitive.
	Internal Kind = iota + 1
	// Invalid: the input cannot be used.
	Invalid
	// FailedPrecondition: the input is fine, the state it needs is not there.
	FailedPrecondition
	// Unavailable: something the platform depends on cannot be reached now;
	// trying again later may work.
	Unavailable
	// NotFound: what was asked for does not exist.
	NotFound
	// AlreadyExists: what was to be created exists.
	AlreadyExists
	// Aborted: the resource changed since it was read.
	Aborted
	// Unimplemented: valid, but not supported (yet).
	Unimplemented
	// PermissionDenied: the caller may not do this, whatever the input.
	PermissionDenied
)

// Error is an error with a Kind and a message for the caller.
type Error struct {
	Kind    Kind
	Message string
}

func (e *Error) Error() string { return e.Message }

// New is an error of kind with a message for the caller.
func New(kind Kind, message string) error { return &Error{Kind: kind, Message: message} }

// Newf is New with a formatted message.
func Newf(kind Kind, format string, args ...any) error {
	return &Error{Kind: kind, Message: fmt.Sprintf(format, args...)}
}

// As returns the Error in err's chain, if there is one.
func As(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}

// Is reports whether err carries an Error of kind.
func Is(err error, kind Kind) bool {
	e, ok := As(err)
	return ok && e.Kind == kind
}
