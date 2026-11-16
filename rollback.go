package pipeline

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
// and the groups around it are no longer settled: [ElementState] says where
// they then stand. Once a rollback passes, gates wait again, and [Run] runs
// what it undid again.
//
// Rollback stops at the first Undo that fails, and returns its error in a
// [StepError]. The step keeps its outcome, with the error beside it in
// [ElementState.UndoError]. A stop or cancel, as for Run, keeps the next Undo from
// starting. Until a rollback passes, Run refuses the journal with
// [ErrRollbackUnfinished]; calling Rollback again carries on where the last
// one stopped, without undoing anything twice.
//
// Rollback goes back past no step marked [StepNode.Irreversible]. Once an
// attempt has called the Run of one, Rollback refuses the journal with an
// [*IrreversibleError], which matches [ErrIrreversible]. With
// [WithStrictUndo] it also refuses, with [ErrNoUndo], when a step it would
// reach has no Undo. Either way it refuses before it undoes anything, and
// records nothing, so Run still resumes the journal.
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
	return toUndo(r.prior, r.plan)
}

func toUndo(prior *State, plan *Plan) []*node {
	var steps []*node

	plan.root.walk(func(n *node) {
		if (n.kind == KindEffect || n.kind == KindEnsure) && prior.Element(n.path).RunCalled {
			steps = append(steps, n)
		}
	})

	slices.Reverse(steps)

	return steps
}

// canUndo reports why a rollback of plan may not start against a journal
// that stands at prev: an irreversible step has run, or, if strict, a step
// the rollback would reach has no Undo. It is decided before the attempt is
// recorded, because a rollback that was recorded and then refused would
// leave the journal with an unfinished rollback, which Run refuses.
func canUndo(prev *State, plan *Plan, strict bool) error {
	var refused error

	plan.root.walk(func(n *node) {
		if attempt, ran := prev.ran[n.path]; ran && n.irreversible && refused == nil {
			refused = &IrreversibleError{Path: n.path, Attempt: attempt}
		}
	})

	if refused != nil || !strict {
		return refused
	}

	var paths []string

	for _, n := range toUndo(prev, plan) {
		if !n.hasUndo {
			paths = append(paths, n.path)
		}
	}

	if len(paths) > 0 {
		return &NoUndoError{Paths: paths}
	}

	return nil
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

	// As for a try: an Undo that returns after a cancel, or after its
	// timeout, has failed, whatever it returns.
	if late := returnedLate(ctx, uctx, n.timeout, err); late != nil {
		err = late
	}

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
