package runbook

import (
	"context"
	"errors"
	"fmt"
)

var (
	// ErrPlanChanged is returned by [Run] when the journal's attempts were
	// made with a plan of a different shape: its fingerprint differs. Start a
	// new journal for a new plan.
	ErrPlanChanged = errors.New("runbook: the plan has changed since the journal's last attempt")

	// ErrInputsChanged is returned by [Run] when the inputs differ from those
	// the journal's latest attempt recorded. [WithNewInputs] accepts the
	// change. Secrets are not compared: they are never recorded.
	ErrInputsChanged = errors.New("runbook: the inputs have changed since the journal's last attempt")

	// ErrAlreadyComplete is returned by [Run] when the journal's latest
	// attempt passed. [Force] runs the plan again regardless.
	ErrAlreadyComplete = errors.New("runbook: the journal's latest attempt already passed")

	// ErrRollbackUnfinished is returned by [Run] when the journal's latest
	// attempt is a [Rollback] that did not pass. Run Rollback again to finish
	// it first.
	ErrRollbackUnfinished = errors.New("runbook: the journal's rollback has not finished")

	// ErrStopped is returned by [Run] when a [Stopper] or [RequestStop]
	// stopped the run before its work was done. The run is unfinished, and a later Run
	// resumes it.
	ErrStopped = errors.New("runbook: the run was stopped")

	// ErrStepReturned is returned by [StepContext] methods called after the
	// step has returned, for example from a goroutine it left running.
	ErrStepReturned = errors.New("runbook: the step has returned")

	// ErrSkipGroup, returned by a step's Run, ends the step's group early.
	// The step passes, its signal is recorded, and the group's children that
	// have not started are recorded as skipped. Siblings already running in
	// a parallel group finish. The group passes unless something failed.
	ErrSkipGroup = errors.New("runbook: skip the rest of the group")

	// ErrSkipRun, returned by a step's Run, ends the run early, as passed.
	// The step passes, its signal is recorded, and every element that has
	// not started is recorded as skipped. Steps already running finish.
	ErrSkipRun = errors.New("runbook: skip the rest of the run")
)

// signalOf returns the signal err carries, if it is [ErrSkipGroup] or
// [ErrSkipRun].
func signalOf(err error) Signal {
	switch {
	case errors.Is(err, ErrSkipRun):
		return SignalSkipRun
	case errors.Is(err, ErrSkipGroup):
		return SignalSkipGroup
	default:
		return ""
	}
}

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
	case errors.Is(err, ErrGateDeclined):
		info.Kind = ErrorDeclined
	}

	return info
}
