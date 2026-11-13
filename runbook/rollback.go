package runbook

import (
	"context"
	"slices"
)

// Rollback undoes what the journal's attempts did, as derived from the
// journal: nobody writes the reverse plan by hand. It is recorded in j as an
// attempt of its own, so [Inspect] shows it.
//
// It takes every Effect and Ensure step whose Run an attempt called, whether
// that Run passed, failed or was interrupted, and that no rollback has undone
// since. A step that Check found already done was never Run, and is not
// taken. In reverse plan order, one at a time, it calls each one's Undo,
// bounded by the step's Timeout and not retried. A step without an Undo is
// recorded as skipped, with reason [ReasonNoUndo], and keeps its outcome.
//
// An Undo can read what the step's Run recorded, with
// [StepContext.Recorded], even if that Run was interrupted. A step undone
// goes back to never started, its recorded values and items are forgotten,
// and the groups around it are no longer settled. Once a rollback passes,
// gates wait again, and [Run] runs what it undid again.
//
// Rollback stops at the first Undo that fails, and returns its error in a
// [StepError]. A stop or cancel, as for Run, keeps the next Undo from
// starting. Until a rollback passes, Run refuses the journal with
// [ErrRollbackUnfinished]; calling Rollback again carries on where the last
// one stopped, without undoing anything twice.
//
// Rollback takes the options Run does, and refuses a journal whose attempts
// used a plan of a different shape ([ErrPlanChanged]) or other inputs
// ([ErrInputsChanged]), as Run does.
func Rollback(ctx context.Context, plan *Plan, j *Journal, opts ...Option) (*Result, error) {
	return attempt(ctx, plan, j, opts, true)
}

// undoAll undoes the steps a rollback takes, in reverse plan order, until
// one fails or a stop or cancel is requested.
func (r *runner) undoAll(ctx context.Context) error {
	for _, n := range r.toUndo() {
		if r.cancelled(ctx) {
			r.markCut()

			return context.Cause(ctx)
		}

		if r.stopping() {
			r.halt()

			return ErrStopped
		}

		if err := r.undo(ctx, n); err != nil {
			return err
		}
	}

	return nil
}

// toUndo returns the Effect and Ensure steps whose Run an attempt called,
// and no rollback has undone since, in reverse plan order: a group's
// failure handler after its children.
func (r *runner) toUndo() []*node {
	var steps []*node

	var walk func(n *node)

	walk = func(n *node) {
		for _, c := range n.children {
			walk(c)
		}

		if n.handler != nil {
			walk(n.handler)
		}

		if (n.kind == KindEffect || n.kind == KindEnsure) && r.prior.Element(n.path).RunCalled {
			steps = append(steps, n)
		}
	}

	walk(r.plan.root)
	slices.Reverse(steps)

	return steps
}

// undo calls a step's Undo, if it has one, and records how it went.
func (r *runner) undo(ctx context.Context, n *node) error {
	undoer, ok := n.step.(Undoer)
	if !ok {
		return r.emit(Event{Kind: EventUndoFinished, Path: n.path, Outcome: OutcomeSkipped, Reason: ReasonNoUndo})
	}

	start := r.clock.Now()

	if err := r.emit(Event{Kind: EventUndoStarted, Path: n.path}); err != nil {
		return err
	}

	uctx := ctx

	if n.timeout > 0 {
		var cancel context.CancelFunc

		uctx, cancel = context.WithTimeout(ctx, n.timeout)
		defer cancel()
	}

	sc := &StepContext{r: r, n: n}
	err := safely(func() error { return undoer.Undo(uctx, sc) })

	// As for a step: nothing the Undo writes may follow undo.finished.
	sc.close()

	if err != nil && ctx.Err() != nil {
		r.markCut()
	}

	outcome := OutcomeUndone
	if err != nil {
		outcome = OutcomeFailed
	}

	if ferr := r.emit(Event{
		Kind: EventUndoFinished, Path: n.path, Outcome: outcome, Error: errorInfo(err), DurationMs: r.since(start),
	}); ferr != nil {
		return ferr
	}

	if err != nil {
		return &StepError{Path: n.path, Err: r.redact.err(err)}
	}

	return nil
}
