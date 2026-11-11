package runbook

import (
	"context"
	"errors"
	"fmt"
)

var (
	// ErrResumeNotSupported is returned by [Run] for a journal that already has
	// attempts. Resuming arrives with the file journal.
	ErrResumeNotSupported = errors.New("runbook: the journal already has attempts; resuming is not supported yet")

	// ErrStepReturned is returned by [StepContext] methods called after the
	// step has returned, for example from a goroutine it left running.
	ErrStepReturned = errors.New("runbook: the step has returned")
)

// PanicError is a panic recovered from a step's Check or Run.
type PanicError struct {
	Value any
	// Stack is the panicking goroutine's stack. It is kept out of Error, and
	// recorded separately in the journal.
	Stack string
}

// Error implements error.
func (e *PanicError) Error() string {
	return fmt.Sprintf("panic: %v", e.Value)
}

// StepError is a failed step's error, with the step's path. The error
// returned by [Run] joins one for each failed step.
type StepError struct {
	Path string
	Err  error
}

// Error implements error.
func (e *StepError) Error() string {
	return e.Path + ": " + e.Err.Error()
}

// Unwrap returns the step's own error.
func (e *StepError) Unwrap() error {
	return e.Err
}

// errorInfo turns an error into the data recorded in the journal.
func errorInfo(err error) *ErrorInfo {
	if err == nil {
		return nil
	}

	info := &ErrorInfo{Kind: ErrorPlain, Message: err.Error()}

	var p *PanicError

	switch {
	case errors.As(err, &p):
		info.Kind, info.Stack = ErrorPanic, p.Stack
	case errors.Is(err, context.DeadlineExceeded):
		info.Kind = ErrorTimeout
	case errors.Is(err, context.Canceled):
		info.Kind = ErrorCancelled
	}

	return info
}
