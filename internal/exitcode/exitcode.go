// Package exitcode defines the documented exit codes of the forge CLI and the
// typed error that carries one. Commands return errors; only main turns them
// into an exit code and a message.
package exitcode

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
)

// Code is a process exit code.
type Code int

// Exit codes, as documented in the design doc.
const (
	OK               Code = 0
	Unexpected       Code = 1
	Usage            Code = 2
	NotFound         Code = 3
	Unauthenticated  Code = 4
	PermissionDenied Code = 5
	Conflict         Code = 6
	OperationFailed  Code = 7
	Timeout          Code = 8
)

// Error is an error with an exit code and an optional hint for the user.
type Error struct {
	Code Code
	Err  error
	Hint string
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

// New returns an Error with a formatted message.
func New(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Err: fmt.Errorf(format, args...)}
}

// WithHint returns a copy of e with the hint set.
func (e *Error) WithHint(hint string) *Error {
	c := *e
	c.Hint = hint
	return &c
}

// Usagef returns a usage error (exit code 2).
func Usagef(format string, args ...any) *Error { return New(Usage, format, args...) }

// From classifies any error into an *Error. Connect errors from the API are
// mapped by their code; context errors become Timeout.
func From(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Code: Timeout, Err: err, Hint: "raise --timeout or retry later"}
	}
	var ce *connect.Error
	if errors.As(err, &ce) {
		return fromConnect(ce)
	}
	return &Error{Code: Unexpected, Err: err}
}

func fromConnect(ce *connect.Error) *Error {
	msg := errors.New(ce.Message())
	if ce.Message() == "" {
		msg = errors.New(ce.Code().String())
	}
	switch ce.Code() {
	case connect.CodeInvalidArgument, connect.CodeOutOfRange:
		return &Error{Code: Usage, Err: msg}
	case connect.CodeNotFound:
		return &Error{Code: NotFound, Err: msg}
	case connect.CodeUnauthenticated:
		return &Error{Code: Unauthenticated, Err: msg, Hint: "run 'forge auth login'"}
	case connect.CodePermissionDenied:
		return &Error{Code: PermissionDenied, Err: msg}
	case connect.CodeAlreadyExists, connect.CodeAborted, connect.CodeFailedPrecondition:
		return &Error{Code: Conflict, Err: msg}
	case connect.CodeDeadlineExceeded:
		return &Error{Code: Timeout, Err: msg}
	case connect.CodeUnavailable:
		return &Error{Code: Unexpected, Err: msg, Hint: "the Forge API is unreachable; check 'forge doctor'"}
	default:
		return &Error{Code: Unexpected, Err: msg}
	}
}
