package pipeline

import "context"

// Step is the work of one element of a plan.
//
// Run must be idempotent: after a crash part-way through it, it may be called
// again on a system it has already partly changed.
//
// Run should return when ctx is done. That is how a timeout and a cancel
// reach it: nothing else can stop a step, and one that ignores ctx runs on.
// A try that returns after its timeout, or after a cancel, has failed,
// whatever it returns.
//
// Returning is all that a cancel asks of a step. Work the step started
// somewhere else, such as a job on another machine, carries on unless the
// step stops it, and the journal then says that the step failed, not that
// the work has ended. A step that starts such work records how to find it
// before starting it, so that a later attempt's Check can ask after it.
type Step interface {
	Run(ctx context.Context, s *StepContext) error
}

// Checker is implemented by steps that can tell whether their work is already
// done. Check runs before every Run and must not change the system. An error
// from Check fails the step, exactly like an error from Run.
//
// Check is not a check of Run's work. Nothing calls it after a Run that
// returned nil: the step has passed, whatever it left behind. A step that
// must be verified once its work is done verifies it at the end of its own
// Run, or is followed by an Observe step that does.
//
// Returning false sends the step on to Run, so return it only when Run is
// the answer to what Check found: the work is not done, and doing it now is
// safe. When Check can't tell, because the system can't be reached, or it
// finds something Run should not be let loose on, such as work half done by
// other means, it returns an error. The step then fails with nothing
// changed.
type Checker interface {
	Check(ctx context.Context, s *StepContext) (bool, error)
}

// Undoer is implemented by steps whose work can be reversed. Undo must be
// idempotent, and must cope with a Run that was interrupted part-way.
type Undoer interface {
	Undo(ctx context.Context, s *StepContext) error
}

// StepFunc adapts a function to [Step], for steps that need neither Check nor
// Undo.
type StepFunc func(ctx context.Context, s *StepContext) error

// Run implements [Step] by calling f.
func (f StepFunc) Run(ctx context.Context, s *StepContext) error {
	return f(ctx, s)
}
