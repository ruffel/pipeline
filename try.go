package pipeline

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"strings"
	"time"
)

// step runs a step's tries: Check, then Run if the work isn't done. A fresh
// step, a failure handler, runs whatever an earlier attempt did with it.
func (r *runner) step(ctx context.Context, n *node, fresh bool) error {
	prior := r.prior.Element(n.path)
	if fresh {
		prior = ElementState{Status: StatusNotReached}
	}

	// An Effect step an earlier attempt finished is trusted when it has no
	// Check to verify it with, or the run was told to trust the journal.
	// Ensure and Observe steps are never trusted.
	if n.kind == KindEffect && prior.Done && (r.trust || !n.hasCheck) {
		if err := r.emit(Event{
			Kind: EventStepFinished, Path: n.path, Outcome: OutcomeSkipped, Reason: ReasonEarlierAttempt,
		}); err != nil {
			return err
		}

		return r.applySkip(prior.Skip)
	}

	start := r.clock.Now()

	if err := r.emit(Event{Kind: EventStepStarted, Path: n.path}); err != nil {
		return err
	}

	sc := &StepContext{r: r, n: n}

	// An earlier attempt called Run and never finished the step: if Check
	// now finds the work done, that Run did it.
	interrupted := prior.RunCalled && !prior.Done

	outcome, reason, skip, err := r.tries(ctx, n, sc, interrupted)

	// A step verified as still done keeps the skip its Run gave.
	if outcome == OutcomeSkipped {
		skip = prior.Skip
	}

	if skip == SkipRun {
		// Set at once, so parallel siblings start nothing new.
		r.skipRun.Store(true)
	}

	// Close before step.finished: writes still in flight land first, and any
	// later ones are refused, so nothing follows the step's last event.
	sc.close()

	if err != nil && ctx.Err() != nil {
		r.markCut()
	}

	if ferr := r.emit(Event{
		Kind: EventStepFinished, Path: n.path, Outcome: outcome, Reason: reason, Skip: skip,
		Error: errorInfo(err), DurationMs: r.since(start),
	}); ferr != nil {
		return ferr
	}

	if err != nil {
		return &StepError{Path: n.path, Err: r.redact.err(err)}
	}

	return r.applySkip(skip)
}

// refuseSkip fails a step for a skip that it recorded before a later build
// made its group a DrainOnStop group, as a skip returned in one fails. The
// failure clears the skip, so the next attempt runs the step.
func (r *runner) refuseSkip(n *node, skip Skip) error {
	err := fmt.Errorf("pipeline: an earlier attempt's Run returned %s, and nothing may end %s, "+
		"a DrainOnStop group, early", skip, n.boundary.path)

	if ferr := r.emit(Event{
		Kind: EventStepFinished, Path: n.path, Outcome: OutcomeFailed, Error: errorInfo(err),
	}); ferr != nil {
		return ferr
	}

	return &StepError{Path: n.path, Err: err}
}

// skippedRun reports whether a step at or beneath n returned ErrSkipRun
// when its Run last finished.
func (r *runner) skippedRun(n *node) bool {
	if r.prior.Element(n.path).Skip == SkipRun {
		return true
	}

	return slices.ContainsFunc(n.children, r.skippedRun)
}

// applySkip turns a step's skip, from this attempt or replayed from an
// earlier one, into the error its group acts on.
func (r *runner) applySkip(skip Skip) error {
	switch skip {
	case SkipGroup:
		return ErrSkipGroup
	case SkipRun:
		r.skipRun.Store(true)

		return ErrSkipRun
	default:
		return nil
	}
}

// tries runs the step until it passes, is found done, signals, or runs out
// of tries. A signal from Run passes the step.
func (r *runner) tries(
	ctx context.Context, n *node, sc *StepContext, interrupted bool,
) (Outcome, Reason, Skip, error) {
	ranOnce := false

	for try := 1; ; try++ {
		done, err := r.try(ctx, n, sc, &ranOnce)

		if skip := skipOf(err); skip != "" {
			return OutcomePassed, "", skip, nil
		}

		switch {
		case err == nil && done && (ranOnce || interrupted):
			return OutcomePassed, ReasonRecovered, "", nil
		case err == nil && done:
			return OutcomeSkipped, ReasonDone, "", nil
		case err == nil:
			return OutcomePassed, "", "", nil
		case r.lastTry(ctx, n, try):
			return OutcomeFailed, "", "", err
		}

		if eerr := r.emit(Event{Kind: EventStepRetry, Path: n.path, Try: try, Error: errorInfo(err)}); eerr != nil {
			return OutcomeFailed, "", "", eerr
		}

		if werr := r.backoff(ctx, n, err); werr != nil {
			return OutcomeFailed, "", "", werr
		}
	}
}

// lastTry reports whether a step whose try has just failed is not tried
// again: it is out of tries, or the run is ending.
//
// A DrainOnStop group is finished under a stop, not worked at. So a step in
// one is not tried again once a stop is pending, and it is the failure that
// ends the group, with tries left or without: the stop is not what kept
// anything from happening, and the run has failed. The same goes for a
// failure handler that a stop is waiting for, because such a group failed.
func (r *runner) lastTry(ctx context.Context, n *node, try int) bool {
	if try >= n.tries || ctx.Err() != nil || r.fatalErr() != nil {
		return true
	}

	return r.awaited(n) && r.stopping()
}

// awaited reports whether a stop waits for n: it is a step in a DrainOnStop
// group, or the failure handler of a group around one that has failed.
func (r *runner) awaited(n *node) bool {
	return n.boundary != nil || (n.handles != nil && r.failedDraining(n.handles))
}

// failedDraining reports whether a DrainOnStop group beneath n has failed in
// this attempt.
func (r *runner) failedDraining(n *node) bool {
	r.drainMu.Lock()
	defer r.drainMu.Unlock()

	for path := range r.drainFailed {
		if strings.HasPrefix(path, n.path+"/") {
			return true
		}
	}

	return false
}

// setPath adds path to set, making the set if it has none yet.
func setPath(set map[string]bool, path string) map[string]bool {
	if set == nil {
		set = map[string]bool{}
	}

	set[path] = true

	return set
}

// backoff waits before the step's next try. If a cancel or stop cuts the
// wait short, it returns what the step fails with: under a stop, its last
// error.
func (r *runner) backoff(ctx context.Context, n *node, err error) error {
	select {
	case <-ctx.Done():
		return errors.Join(err, ctx.Err())
	case <-r.stopDone():
		if r.cancelled(ctx) {
			return errors.Join(err, ctx.Err())
		}

		r.noteStop()
		r.untried(n)

		return err
	case <-r.clock.After(n.backoff):
		// The stop may have come at the same moment, and select picks either
		// of two ready cases: never start another try after one.
		if r.stopping() {
			r.untried(n)

			return err
		}

		return nil
	}
}

// untried records that a stop kept n from being tried again, unless the
// stop waits for n. Then the step has failed as it would have with the stop
// already pending, when it is not tried again either: whether the stop came
// before the try failed or during the wait after it, the failure is what
// the run ends with, and the run has failed.
func (r *runner) untried(n *node) {
	if !r.awaited(n) {
		r.halt()
	}
}

// try runs one Check and, if the work isn't done, Run, within the step's
// timeout. It reports whether Check found the work done.
func (r *runner) try(ctx context.Context, n *node, sc *StepContext, ranOnce *bool) (bool, error) {
	tctx := ctx

	if n.timeout > 0 {
		var cancel context.CancelFunc

		tctx, cancel = context.WithTimeout(ctx, n.timeout)
		defer cancel()
	}

	done, err := r.checkAndRun(tctx, n, sc, ranOnce)
	if late := returnedLate(ctx, tctx, n.timeout, err); late != nil {
		return false, late
	}

	// Ending the group early as passed is the one thing a DrainOnStop group
	// is there to rule out. A skip returned late has been dealt with above:
	// that try timed out or was cancelled.
	if n.boundary != nil && skipOf(err) != "" {
		return false, fmt.Errorf("pipeline: Run returned %q, and nothing may end %s, a DrainOnStop group, early",
			err.Error(), n.boundary.path)
	}

	return done, err
}

// returnedLate is what a try, or an Undo, fails with when it claimed to
// succeed after it was told to stop: it returned nil, or a skip, once ctx,
// the context of the run or of its group, was cancelled, or once tctx, its
// own, had passed its timeout. A step that watches its context returns the
// context's error instead, and fails with that; one that fails with an
// error of its own keeps it.
//
// The errors match what a step that stopped in time returns:
// [context.Canceled] after a cancel, [context.DeadlineExceeded] after a
// timeout.
func returnedLate(ctx, tctx context.Context, timeout time.Duration, err error) error {
	if err != nil && skipOf(err) == "" {
		return nil
	}

	if cerr := ctx.Err(); cerr != nil {
		// A cancel given a cause of its own keeps it within reach.
		if cause := context.Cause(ctx); !errors.Is(cause, cerr) {
			return fmt.Errorf("pipeline: returned after it was cancelled: %w: %w", cerr, cause)
		}

		return fmt.Errorf("pipeline: returned after it was cancelled: %w", cerr)
	}

	if errors.Is(tctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("pipeline: returned after its %s timeout had passed: %w", timeout, context.DeadlineExceeded)
	}

	return nil
}

// checkAndRun runs one Check and, if the work isn't done, Run.
func (r *runner) checkAndRun(ctx context.Context, n *node, sc *StepContext, ranOnce *bool) (bool, error) {
	if checker, ok := n.step.(Checker); ok {
		var done bool

		err := safely(func() error {
			var err error

			done, err = checker.Check(ctx, sc)

			return err
		})

		if skipOf(err) != "" {
			// Only Run may end its group or the run; from Check it is a bug.
			return false, fmt.Errorf("pipeline: Check returned %q; only Run may skip", err.Error())
		}

		if err != nil || done {
			return done, err
		}
	}

	// Check may have used up the try's time, or the run may have been
	// cancelled while it ran: never start Run then.
	if err := ctx.Err(); err != nil {
		return false, err
	}

	if !*ranOnce {
		if err := r.emit(Event{Kind: EventStepRunning, Path: n.path}); err != nil {
			return false, err
		}

		*ranOnce = true
	}

	return false, safely(func() error { return n.step.Run(ctx, sc) })
}

// safely calls fn, turning a panic into a [PanicError].
func safely(fn func() error) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = &PanicError{Value: v, Stack: string(debug.Stack())}
		}
	}()

	return fn()
}
