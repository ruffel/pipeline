package runbook

import "context"

// Step is the work of one element of a plan.
//
// Run must be idempotent: after a crash part-way through it, it may be called
// again on a system it has already partly changed.
type Step interface {
	Run(ctx context.Context, s *StepContext) error
}

// Checker is implemented by steps that can tell whether their work is already
// done. Check runs before every Run and must not change the system. An error
// from Check fails the step, exactly like an error from Run.
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
