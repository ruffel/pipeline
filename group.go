package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

func (r *runner) group(ctx context.Context, n *node) error {
	// A group that finished and passed in an earlier attempt is settled: it
	// is skipped as a unit, with none of its steps or their Checks run.
	// Deciding step by step would re-run steps that a later step in the same
	// group reversed, such as a node's stop after its start.
	if r.prior.Element(n.path).Done && n.canSettle() {
		if err := r.emit(Event{
			Kind: EventGroupFinished, Path: n.path, Outcome: OutcomeSkipped, Reason: ReasonSettled,
		}); err != nil {
			return err
		}

		// A step in the group that ended the run early still does: without
		// this, a resume after a crash between the group finishing and the
		// run finishing would run what the signal skipped. A step's
		// ErrSkipGroup concerned only its own group, which is finished.
		if r.skippedRun(n) {
			r.skipRun.Store(true)
		}

		return nil
	}

	order, settled, err := r.order(n)
	if err != nil {
		// The group has not started: none of its children ran, so its
		// failure handler has nothing to handle.
		if ferr := r.emit(Event{
			Kind: EventGroupFinished, Path: n.path, Outcome: OutcomeFailed, Error: errorInfo(err),
		}); ferr != nil {
			return ferr
		}

		return &StepError{Path: n.path, Err: err}
	}

	start := r.clock.Now()

	if err := r.emit(Event{Kind: EventGroupStarted, Path: n.path, Order: settled}); err != nil {
		return err
	}

	err = r.children(ctx, n, order)

	var herr error
	if err != nil && !onlyStopped(err) {
		herr = r.handle(ctx, n)
	}

	outcome := OutcomePassed

	switch {
	case err == nil:
	case onlyStopped(err):
		outcome = OutcomeStopped
	default:
		outcome = OutcomeFailed

		if n.drain {
			r.drainMu.Lock()
			r.drainFailed = setPath(r.drainFailed, n.path)
			r.drainMu.Unlock()
		}
	}

	if ferr := r.emit(Event{
		Kind: EventGroupFinished, Path: n.path, Outcome: outcome, Error: errorInfo(errors.Join(err, herr)),
		DurationMs: r.since(start),
	}); ferr != nil {
		return ferr
	}

	return errors.Join(err, herr)
}

// order returns the children of n in the order they run in. For an OrderFrom
// group it also returns that order by name, for the group's start to record:
// the one an earlier attempt settled, whatever has been recorded since, or
// else the one the group's step has recorded, which must name every child
// exactly once.
func (r *runner) order(n *node) ([]*node, []string, error) {
	// An order that an earlier attempt settled stands until the group goes
	// back to never started, even if the plan no longer asks for one.
	names, from := r.prior.Order(n.path), "an earlier attempt settled"

	if names == nil {
		if n.order == nil {
			return n.children, nil, nil
		}

		raw, ok := r.recorded.recalled(n.order.Path, n.order.Key)
		if !ok {
			return nil, nil, fmt.Errorf("pipeline: %s has recorded no %q for the group's order", n.order.Path, n.order.Key)
		}

		if err := json.Unmarshal(raw, &names); err != nil {
			return nil, nil, fmt.Errorf("pipeline: what %s recorded as %q is not a list of names: %w",
				n.order.Path, n.order.Key, err)
		}

		from = fmt.Sprintf("%s recorded as %q", n.order.Path, n.order.Key)
	}

	order, err := n.arranged(names)
	if err != nil {
		return nil, nil, fmt.Errorf("pipeline: the order %s %w", from, err)
	}

	return order, names, nil
}

// handle runs a failed group's failure handler, if it has one and nothing
// keeps it from starting, and returns the handler's failure. A recovery
// handler's reset is recorded as it starts. A stop or cancel requested before
// it starts keeps it from running, as it keeps any step from starting.
//
// A stop does not keep it from running if a DrainOnStop group beneath the
// group has failed. The stop waited for that group to end, and a group that
// ends in failure is not left as it fell because a stop happened to be
// pending: its failure is handled as it would have been.
func (r *runner) handle(ctx context.Context, n *node) error {
	if n.handler == nil {
		return nil
	}

	if err := r.fatalErr(); err != nil {
		return err
	}

	switch {
	case r.cancelled(ctx):
		r.markCut()

		return nil
	case !r.stopping():
	case r.failedDraining(n):
		// Whoever is following the run sees a step start after the stop.
		_ = r.emit(Event{
			Kind: EventMessage, Path: n.path, Level: LevelInfo,
			Message: "a stop was requested: this group's failure is handled first",
		})
	default:
		r.halt()

		return nil
	}

	if n.recovers {
		if eerr := r.emit(Event{Kind: EventGroupReset, Path: n.path}); eerr != nil {
			return eerr
		}

		r.recorded.forget(n.path)
	}

	herr := r.step(ctx, n.handler, true)

	// The group is finishing, so a signal to skip the rest of it means
	// nothing. One to skip the rest of the run is already in effect.
	if errors.Is(herr, ErrSkipGroup) || errors.Is(herr, ErrSkipRun) {
		return nil
	}

	return herr
}

// children runs a group's children under its failure policy, in the order
// given: the plan's, unless the group takes its order from a step. Children
// not started because of a failure, a cancel or a stop get no events: replay
// reports them as not reached. A group cut short by a cancel returns the
// context's error, and one cut short by a stop returns ErrStopped, even if
// no child failed.
func (r *runner) children(ctx context.Context, n *node, order []*node) error {
	if n.parallel {
		return r.parallel(ctx, n)
	}

	var errs []error

	// Only a failure or a cancel ends a DrainOnStop group early. A stop, or
	// a step elsewhere ending the run early, waits for it: either would
	// leave it half done while it was healthy.
	drains := n.boundary != nil

children:
	for i, c := range order {
		if !drains && r.skipRun.Load() {
			errs = append(errs, r.skipRest(order[i:], ReasonRunSkipped))

			break
		}

		if r.cancelled(ctx) {
			errs = append(errs, context.Cause(ctx))

			r.markCut()

			break
		}

		switch stopping := r.stopping(); {
		case stopping && drains:
			r.noteDrain(n.boundary)
		case stopping:
			errs = append(errs, ErrStopped)

			r.halt()

			break children
		}

		err := r.element(ctx, c)

		switch {
		case err == nil:
		case errors.Is(err, ErrSkipGroup):
			errs = append(errs, r.skipRest(order[i+1:], ReasonGroupSkipped))

			break children
		case errors.Is(err, ErrSkipRun):
			// The next iteration records the rest as skipped.
		default:
			errs = append(errs, err)

			if n.failure != FailureContinue {
				// A run-wide skip signalled inside the failed child (after an
				// earlier failure there) still applies to the rest.
				if r.skipRun.Load() {
					errs = append(errs, r.skipRest(order[i+1:], ReasonRunSkipped))
				}

				break children
			}
		}
	}

	return errors.Join(errs...)
}

func (r *runner) parallel(ctx context.Context, n *node) error {
	gctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg    sync.WaitGroup
		g     parallelGroup
		slots chan struct{}
	)

	if n.limit > 0 {
		slots = make(chan struct{}, n.limit)
	}

	started := len(n.children)

	for i, c := range n.children {
		if slots != nil {
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
			case <-r.stopDone():
			}
		}

		if g.holdBack(ctx, r) {
			started = i

			break
		}

		wg.Go(func() {
			defer func() {
				if slots != nil {
					<-slots
				}
			}()

			g.finished(r.element(gctx, c), n.failure, cancel)
		})
	}

	wg.Wait()

	// Children never started are skipped if any child signalled, including a
	// signal that came after a failure had already stopped admission.
	if reason := g.skipReason(r); reason != "" && started < len(n.children) {
		g.add(r.skipRest(n.children[started:], reason))
	}

	return g.err()
}

// parallelGroup is the state a parallel group's children share.
type parallelGroup struct {
	mu      sync.Mutex
	errs    []error
	stopped bool   // a child failed: start nothing new
	skip    Reason // a child signalled: record the rest as skipped
}

// holdBack reports whether the next child must not start. A cancel or stop
// that keeps it from starting is recorded.
func (g *parallelGroup) holdBack(ctx context.Context, r *runner) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	switch {
	case g.stopped || g.skip != "" || r.skipRun.Load():
		return true
	case r.cancelled(ctx):
		g.errs = append(g.errs, context.Cause(ctx))

		r.markCut()

		return true
	case r.stopping():
		g.errs = append(g.errs, ErrStopped)

		r.halt()

		return true
	default:
		return false
	}
}

// skipReason returns why children that never started are skipped, if a child
// of this group, or any step in the run, signalled.
func (g *parallelGroup) skipReason(r *runner) Reason {
	g.mu.Lock()
	defer g.mu.Unlock()

	if r.skipRun.Load() {
		return ReasonRunSkipped
	}

	return g.skip
}

// finished records how a child ended, applying the group's failure policy.
func (g *parallelGroup) finished(err error, policy FailurePolicy, cancel context.CancelFunc) {
	if err == nil {
		return
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	switch {
	case onlyStopped(err):
		// A child a stop cut short has not failed: the failure policy does
		// not apply, and the stop itself keeps anything new from starting.
		g.errs = append(g.errs, err)
	case errors.Is(err, ErrSkipRun):
		g.skip = ReasonRunSkipped
	case errors.Is(err, ErrSkipGroup):
		if g.skip == "" {
			g.skip = ReasonGroupSkipped
		}
	default:
		g.errs = append(g.errs, err)

		switch policy {
		case FailureStop:
			g.stopped = true
		case FailureCancel:
			g.stopped = true

			cancel()
		case FailureContinue:
		}
	}
}

func (g *parallelGroup) add(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.errs = append(g.errs, err)
}

func (g *parallelGroup) err() error {
	g.mu.Lock()
	defer g.mu.Unlock()

	return errors.Join(g.errs...)
}

// skipRest records elements that never started as skipped, with reason:
// each group after its descendants, in plan order.
func (r *runner) skipRest(elems []*node, reason Reason) error {
	for _, n := range elems {
		if err := r.skipRest(n.children, reason); err != nil {
			return err
		}

		kind := EventStepFinished
		if n.kind == KindGroup {
			kind = EventGroupFinished
		}

		if err := r.emit(Event{Kind: kind, Path: n.path, Outcome: OutcomeSkipped, Reason: reason}); err != nil {
			return err
		}
	}

	return nil
}
