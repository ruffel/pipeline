package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrPlanChanged is returned by [Run] when the journal's attempts were
	// made with a plan of a different shape: its fingerprint differs. Start a
	// new journal for a new plan, or give [WithPlanChange] to carry on with
	// a changed one. Under that option an error matching ErrPlanChanged
	// names what could not be carried over.
	ErrPlanChanged = errors.New("pipeline: the plan has changed since the journal's last attempt")

	// ErrInputsChanged is returned by [Run] when the inputs differ from those
	// the journal's latest attempt recorded. [WithNewInputs] accepts the
	// change, and [WithInputConsumers] one that nothing has used yet.
	// Secrets are not compared: they are never recorded.
	ErrInputsChanged = errors.New("pipeline: the inputs have changed since the journal's last attempt")

	// ErrAlreadyComplete is returned by [Run] when the journal's latest
	// attempt passed. [Force] runs the plan again regardless.
	ErrAlreadyComplete = errors.New("pipeline: the journal's latest attempt already passed")

	// ErrRollbackUnfinished is returned by [Run] when the journal's latest
	// attempt is a [Rollback] that did not pass. Run Rollback again to finish
	// it first.
	ErrRollbackUnfinished = errors.New("pipeline: the journal's rollback has not finished")

	// ErrIrreversible is matched by the error [Rollback] returns when an
	// attempt has called the Run of a step marked [StepNode.Irreversible],
	// whether or not the plan still marks it. The error is an
	// [*IrreversibleError], which names the step. Nothing was undone, and
	// nothing was written to the journal, so [Run] still resumes it.
	ErrIrreversible = errors.New("pipeline: an irreversible step has run")

	// ErrNoUndo is matched by the error [Rollback] returns under
	// [WithStrictUndo] when a step it would have to undo has no Undo. The
	// error is a [*NoUndoError], which names every such step. Nothing was
	// undone, and nothing was written to the journal.
	ErrNoUndo = errors.New("pipeline: a step that ran has no Undo")

	// ErrStopped is matched by the error [Run] and [Rollback] return when
	// the attempt was stopped or cancelled before its work was done: by a
	// [Stopper], by [RequestStop], or by cancelling its context. The
	// attempt is unfinished, and a later one carries on from it. The
	// Result's Outcome says whether it was stopped or cancelled.
	ErrStopped = errors.New("pipeline: the run was stopped")

	// ErrStepReturned is returned by [StepContext] methods called after the
	// step has returned, for example from a goroutine it left running.
	ErrStepReturned = errors.New("pipeline: the step has returned")

	// ErrSkipGroup, returned by a step's Run, ends the step's group early.
	// The step passes, its skip is recorded, and the group's children that
	// have not started are recorded as skipped. Siblings already running in
	// a parallel group finish. The group passes unless something failed.
	ErrSkipGroup = errors.New("pipeline: skip the rest of the group")

	// ErrSkipRun, returned by a step's Run, ends the run early, as passed.
	// The step passes, its skip is recorded, and every element that has
	// not started is recorded as skipped. Steps already running finish.
	ErrSkipRun = errors.New("pipeline: skip the rest of the run")
)

// skipOf returns the skip err carries, if it is [ErrSkipGroup] or
// [ErrSkipRun].
func skipOf(err error) Skip {
	switch {
	case errors.Is(err, ErrSkipRun):
		return SkipRun
	case errors.Is(err, ErrSkipGroup):
		return SkipGroup
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
// returned by [Run] joins one for each failed step. A group that fails
// without starting, for want of an order to run in, is reported in one too,
// with the group's path: see [GroupNode.OrderFrom].
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

// IrreversibleError is why [Rollback] refused a journal: a step that can't
// be undone has run. It matches [ErrIrreversible].
type IrreversibleError struct {
	// Path is the first step, in plan order, whose Run was called while it
	// was irreversible, and Attempt the first attempt that called it then.
	// The plan given to Rollback may no longer mark the step, or have it at
	// all: what counts is what it was when it ran. For a step that only ran
	// before a plan marked it, Attempt is the first that called it.
	Path    string
	Attempt int

	// unplanned means the journal does not say what the step was when it
	// ran, so Rollback takes it to have been irreversible.
	unplanned bool
}

// Error implements error.
func (e *IrreversibleError) Error() string {
	if e.unplanned {
		return fmt.Sprintf("%v, or may have: attempt %d ran %s, and the journal has no plan for that attempt "+
			"that names it: rollback is refused", ErrIrreversible, e.Attempt, e.Path)
	}

	return fmt.Sprintf("%v: %s, in attempt %d: rollback is no longer possible", ErrIrreversible, e.Path, e.Attempt)
}

// Unwrap returns [ErrIrreversible].
func (e *IrreversibleError) Unwrap() error {
	return ErrIrreversible
}

// NoUndoError is why a [Rollback] under [WithStrictUndo] refused a journal.
// It matches [ErrNoUndo].
type NoUndoError struct {
	// Paths are the steps whose Run was called and that have no Undo, in
	// the order a rollback would have reached them.
	Paths []string
}

// Error implements error.
func (e *NoUndoError) Error() string {
	return fmt.Sprintf("%v: %s", ErrNoUndo, strings.Join(e.Paths, ", "))
}

// Unwrap returns [ErrNoUndo].
func (e *NoUndoError) Unwrap() error {
	return ErrNoUndo
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
