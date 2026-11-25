package pipeline

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Result is how a run ended.
type Result struct {
	// Run identifies the attempt in the journal.
	Run     string
	Attempt int
	Outcome Outcome
}

// Run runs plan, recording every outcome in j before observers are told.
//
// It returns nil when every step passed or was skipped as already done.
// Otherwise it returns the failed steps' errors, each wrapped in a
// [StepError] with the step's path and joined with [errors.Join], so
// [errors.Is] and [errors.As] see the steps' own errors. If the journal
// cannot be written, the run stops at once and Run returns that error.
//
// Cancelling ctx cancels running steps, which are recorded as failed, and
// starts nothing new. The run's outcome is then [OutcomeCancelled]. A step
// that ignores the cancel runs on, and has failed when it returns, whatever
// it returns.
//
// A [Stopper] given with [WithStopper] stops the run gracefully instead:
// running steps finish, nothing new starts, and the outcome is
// [OutcomeStopped].
//
// A run stopped or cancelled, by any of these or by [RequestStop], is
// unfinished, and Run returns an error that matches [ErrStopped], joined
// with the errors of any steps that failed. A cancel's error matches the
// context's error too, such as [context.Canceled]. The Result's Outcome
// says which it was.
//
// Other processes stop or cancel a run on a file journal with
// [RequestStop], and approve its gates with [Approve]. Run makes a
// directory for their requests beside the journal, named after it with
// ".control" added, that only its owner can write to.
//
// If the journal already has attempts, Run resumes: it makes a new attempt
// that does only what is left.
//
//   - A group that finished and passed in an earlier attempt is settled, and
//     is skipped as a unit: none of its steps run, nor their Checks. A group
//     with only Observe steps beneath it never settles.
//   - Elsewhere, an Effect step that an earlier attempt finished has its
//     Check run again, and its Run if the work is no longer done
//     ([ResumeVerify]). With no Check, or under [ResumeTrust], it is skipped.
//     A skipped step's ErrSkipGroup or ErrSkipRun is applied again, and so
//     is an ErrSkipRun from inside a settled group.
//   - Ensure and Observe steps run again whenever their group is entered.
//   - A step whose Run was interrupted or failed, and whose Check now finds
//     the work done, passes as recovered.
//
// Run refuses a journal whose attempts used a plan of a different shape
// ([ErrPlanChanged]), one whose latest attempt is a [Rollback] that has not
// passed ([ErrRollbackUnfinished]), and one whose latest attempt passed
// ([ErrAlreadyComplete]) unless [Force] is given. After a rollback passes,
// Run runs what it undid again.
//
// The Result is nil when Run refuses the journal, or fails before an attempt
// has started. Once an attempt has started, Run returns its Result whatever
// the error, and its Outcome says how the attempt ended.
func Run(ctx context.Context, plan *Plan, j *Journal, opts ...Option) (*Result, error) {
	return attempt(ctx, plan, j, opts, false)
}

// attempt makes one attempt at plan against j: a run, or a rollback.
func attempt(ctx context.Context, plan *Plan, j *Journal, opts []Option, rollback bool) (*Result, error) {
	cfg := config{clock: systemClock{}, outputBudget: MaxStepOutput}
	for _, o := range opts {
		o(&cfg)
	}

	in, err := newInputs(cfg.inputs)
	if err != nil {
		return nil, err
	}

	redact := &redactor{fn: cfg.redact, secrets: in.secrets}

	prev, err := j.State()
	if err != nil {
		return nil, err
	}

	changed, err := applyPlanChange(prev, plan, cfg.planChange)
	if err != nil {
		return nil, err
	}

	// A refusal can carry a secret, in a hook's error or a value it names.
	inputsChanged, err := changedInputsAccepted(prev, plan, in, cfg)
	if err != nil {
		return nil, redact.err(err)
	}

	prior, err := admit(prev, plan, cfg, rollback)
	if err != nil {
		return nil, err
	}

	// Other processes ask a run to stop through files beside the journal.
	var control string

	if j.path != "" {
		control = controlDir(j.path)

		if err := prepareControl(control); err != nil {
			return nil, err
		}
	}

	if cfg.controlPoll <= 0 {
		cfg.controlPoll = DefaultControlPoll
	}

	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	r := &runner{
		plan:          plan,
		journal:       j,
		clock:         cfg.clock,
		id:            newRunID(),
		attempt:       len(prev.Attempts) + 1,
		observers:     cfg.observers,
		obsCtx:        context.WithoutCancel(ctx),
		cancel:        cancel,
		runDone:       runCtx.Done(),
		prev:          prev,
		prior:         prior,
		changed:       changed,
		inputsChanged: inputsChanged,
		trust:         cfg.resume == ResumeTrust,
		redact:        redact,
		inputs:        in,
		stopper:       cfg.stopper,
		stop:          newStopSignal(),
		gates:         map[string]chan controlRequest{},
		control:       control,
		controlPoll:   cfg.controlPoll,
		forced:        cfg.force && !rollback && len(prev.Attempts) > 0,
		rollingBack:   rollback,
		values:        map[string]map[string]json.RawMessage{},
		budget:        cfg.outputBudget,
	}

	res, err := r.run(runCtx, cfg.labels, cfg.data)

	// Every error leaves redacted, not only step failures: a cancel's cause
	// or a journal failure can carry a secret too.
	return res, r.redact.err(err)
}

// applyPlanChange decides whether an attempt may be made with plan against a
// journal that stands at prev, and applies the change to prev if the plan
// has a different shape and how allows it. It returns the change, for the
// attempt to record, or nil if the shape is the same.
func applyPlanChange(prev *State, plan *Plan, how PlanChange) (*planDiff, error) {
	if len(prev.Attempts) == 0 || prev.Fingerprint == plan.Fingerprint() {
		return nil, nil //nolint:nilnil // No change is not an error.
	}

	if how != MatchByPath || prev.Plan == nil {
		return nil, ErrPlanChanged
	}

	d := diffPlans(*prev.Plan, plan.Spec())

	// A path that an earlier change removed still has its record, which a
	// plan that puts it back carries on from: its kind must not differ
	// either.
	rekinded := d.rekinded

	for _, n := range d.added {
		if slices.ContainsFunc(prev.Removed, func(was NodeSpec) bool { return was.Path == n.Path && was.Kind != n.Kind }) {
			rekinded = append(rekinded, n.Path)
		}
	}

	if len(rekinded) > 0 {
		return nil, fmt.Errorf("%w: the kind of %s differs, so its record can't be carried over",
			ErrPlanChanged, strings.Join(rekinded, ", "))
	}

	// What replay does with the attempt's run.started, done now: resume and
	// rollback decide from prev before that event is written.
	prev.changePlan(d)

	return &d, nil
}

// changedInputsAccepted decides whether an attempt may be made with inputs
// in against a journal that stands at prev. If they differ from the latest
// attempt's and the change is accepted, it returns the pointers of the
// fields that changed, for the attempt to record.
func changedInputsAccepted(prev *State, plan *Plan, in *inputs, cfg config) ([]string, error) {
	if err := checkConsumers(plan, cfg.inputConsumers); err != nil {
		return nil, err
	}

	if len(prev.Attempts) == 0 || prev.InputsDigest == in.digest {
		return nil, nil
	}

	decide := cfg.inputChange
	if cfg.inputConsumers != nil {
		decide = append([]func(InputChange) error{consumersHook(cfg.inputConsumers)}, decide...)
	}

	if len(decide) == 0 && !cfg.newInputs {
		return nil, ErrInputsChanged
	}

	changed, err := changedInputs(prev.Inputs, in.encoded)
	if err != nil {
		return nil, err
	}

	if cfg.newInputs {
		return changed, nil
	}

	change := InputChange{Old: prev.Inputs, New: in.encoded, Changed: changed, State: prev}

	for _, fn := range decide {
		if err := fn(change); err != nil {
			if errors.Is(err, ErrInputsChanged) {
				return nil, err
			}

			return nil, fmt.Errorf("%w: %w", ErrInputsChanged, err)
		}
	}

	return changed, nil
}

// admit checks that an attempt may be made against a journal that stands at
// prev, and returns what it decides which work is already done from: prev,
// or, for a forced run, nothing, though earlier values stay readable. The
// children of a group that has settled its order must still be those the
// order names.
func admit(prev *State, plan *Plan, cfg config, rollback bool) (*State, error) {
	prior, err := admitted(prev, plan, cfg, rollback)
	if err != nil {
		return nil, err
	}

	if err := misordered(prior, plan); err != nil {
		return nil, err
	}

	return prior, nil
}

// admitted is admit, before the plan is held against the orders that the
// journal's groups have settled.
func admitted(prev *State, plan *Plan, cfg config, rollback bool) (*State, error) {
	n := len(prev.Attempts)
	if n == 0 {
		return prev, nil
	}

	last := prev.Attempts[n-1]

	switch {
	case rollback:
		// A rollback undoes what the journal says ran, however the latest
		// attempt ended.
		return prev, canUndo(prev, plan, cfg.strictUndo)
	case last.Rollback && last.Outcome != OutcomePassed:
		return nil, ErrRollbackUnfinished
	case cfg.force:
		return newState(), nil
	case !last.Rollback && last.Outcome == OutcomePassed:
		return nil, ErrAlreadyComplete
	default:
		return prev, nil
	}
}

// misordered returns why plan can't carry on from the orders that the
// journal's groups have settled, or nil: the first group whose children are
// not those its order names, or that would now run them at once. An order
// stands until its group goes back to never started, whether or not the plan
// still asks for one. Only a later build can bring either about: the order
// named every child of a sequential group when it was settled.
func misordered(prior *State, plan *Plan) error {
	var err error

	plan.root.walk(func(n *node) {
		names, settled := prior.orders[n.path]
		if err != nil || !settled {
			return
		}

		switch _, bad := n.arranged(names); {
		case bad != nil:
			err = fmt.Errorf("%w: the children of %s are no longer those its order names", ErrPlanChanged, n.path)
		case n.parallel:
			err = fmt.Errorf("%w: %s has settled the order of its children, and may not now be parallel", ErrPlanChanged, n.path)
		}
	})

	return err
}

// runner holds one attempt's state.
type runner struct {
	plan    *Plan
	journal *Journal
	clock   Clock
	id      string
	attempt int
	cancel  context.CancelCauseFunc
	// version is the schema version the attempt's events are written as,
	// set as the attempt starts.
	version int

	// prev is the journal as it stood when the attempt started. prior is
	// what resume decides from: prev, or nothing for a forced run.
	prev   *State
	prior  *State
	trust  bool
	forced bool
	// rollingBack marks a rollback: it undoes steps instead of running them.
	rollingBack bool
	// changed is how the plan differs from the latest attempt's, if it does.
	changed *planDiff
	// inputsChanged are the pointers of the inputs that differ from the
	// latest attempt's, if any do.
	inputsChanged []string

	// redact removes secrets from the text of every event and returned error.
	redact *redactor
	inputs *inputs

	// runDone is closed when the run is cancelled. cut records that a cancel
	// of the run stopped something: a step it interrupted, or children it
	// kept from starting.
	runDone <-chan struct{}
	cut     atomic.Bool

	// skipRun is set when a step returns ErrSkipRun: no element starts after.
	skipRun atomic.Bool

	// stop is the run's graceful stop, asked for by stopper, if set, or by
	// a control file. stopNoted records the request once; halted records
	// that the stop kept something from happening. cancelNoted records a
	// control file's request to cancel.
	stopper     *Stopper
	stop        *stopSignal
	stopNoted   sync.Once
	halted      atomic.Bool
	cancelNoted sync.Once
	// draining are the DrainOnStop groups that have carried on past a stop,
	// by path: each says so once. drainFailed are those that have failed, by
	// path: a stop does not hold back the handling of their failure.
	draining    sync.Map
	drainFailed sync.Map

	// gates are the gates waiting for approval through the control
	// directory, by path.
	gatesMu sync.Mutex
	gates   map[string]chan controlRequest

	// control is the directory other processes write requests to, if the
	// journal is a file; controlPoll is how often it is read.
	control     string
	controlPoll time.Duration

	// mu serialises journal writes and observer calls, so observers see
	// events in journal order.
	mu        sync.Mutex
	observers []Observer
	obsCtx    context.Context //nolint:containedctx // Handed to observers on every event.
	fatal     error

	// budget caps each step's output, in bytes.
	budget int

	// valuesMu guards values recorded during this attempt.
	valuesMu sync.Mutex
	values   map[string]map[string]json.RawMessage
	// reset are the groups a recovery step has reset in this attempt, and
	// standing the values recorded in it that no reset has forgotten since,
	// both by path. Between them and prev they say what the journal holds
	// now, which values and prev alone do not: those still have what a reset
	// forgot, for a recovery step to read.
	reset    map[string]bool
	standing map[string]map[string]bool
}

func (r *runner) run(ctx context.Context, labels L, given any) (*Result, error) {
	// Data that can't be recorded is refused before the attempt starts.
	data, err := runData(given)
	if err != nil {
		return nil, err
	}

	start := r.clock.Now()
	spec := r.plan.Spec()
	r.version = r.plan.version()

	// An attempt that carries on in an order the journal has settled says so
	// on the group's start, whatever its own plan asks for.
	if len(r.prior.orders) > 0 {
		r.version = max(r.version, schemaOrder)
	}

	err = r.emit(Event{
		Kind: EventRunStarted, Path: r.plan.root.path, Attempt: r.attempt,
		Plan: &spec, Fingerprint: r.plan.Fingerprint(), Labels: labels,
		Inputs: r.inputs.encoded, InputsDigest: r.inputs.digest, InputsChanged: r.inputsChanged,
		Forced: r.forced, Rollback: r.rollingBack, PID: os.Getpid(), Host: hostname(), Data: data,
	})
	if err == nil && r.changed != nil {
		err = r.emit(Event{
			Kind: EventPlanChanged, Path: r.plan.root.path,
			Added: paths(r.changed.added), Removed: paths(r.changed.removed),
		})
	}

	if err != nil {
		return &Result{Run: r.id, Attempt: r.attempt, Outcome: OutcomeFailed}, err
	}

	err = r.work(ctx)

	outcome := OutcomePassed

	// The run is cancelled or stopped only if the cancel or stop kept
	// something from happening. One that came after the work ended (even
	// after a failure) changes nothing.
	switch {
	case err == nil:
	case r.cut.Load():
		outcome = OutcomeCancelled
		err = stoppedErr(err)
	case r.halted.Load():
		outcome = OutcomeStopped
		err = stoppedErr(err)
	default:
		outcome = OutcomeFailed
	}

	if fatal := r.fatalErr(); fatal != nil {
		return &Result{Run: r.id, Attempt: r.attempt, Outcome: OutcomeFailed}, fatal
	}

	if ferr := r.emit(Event{
		Kind: EventRunFinished, Path: r.plan.root.path, Outcome: outcome, Error: errorInfo(err), DurationMs: r.since(start),
	}); ferr != nil {
		return &Result{Run: r.id, Attempt: r.attempt, Outcome: OutcomeFailed}, ferr
	}

	return &Result{Run: r.id, Attempt: r.attempt, Outcome: outcome}, err
}

// runData encodes what an attempt was given with WithRunData. Nil, of any
// type, is no data.
func runData(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}

	raw, err := encodeEmbeddable(v)
	if err != nil {
		return nil, fmt.Errorf("pipeline: run data: %w", err)
	}

	if len(raw) > MaxRunDataBytes {
		return nil, fmt.Errorf("pipeline: run data is %d bytes, over the %d limit", len(raw), MaxRunDataBytes)
	}

	if string(raw) == "null" {
		return nil, nil
	}

	return raw, nil
}

// work runs the plan's elements, recording a stop request as soon as it is
// made, and before the run finishes if it was made at all. While it runs,
// it reads requests that other processes leave in the control directory.
func (r *runner) work(ctx context.Context) error {
	done := make(chan struct{})

	var watcher sync.WaitGroup

	watcher.Go(func() { r.watchStop(done) })

	if r.control != "" {
		watcher.Go(func() { r.watchControl(done) })
	}

	var err error

	if r.rollingBack {
		err = r.undoAll(ctx)
	} else {
		err = r.children(ctx, r.plan.root, r.plan.root.children)
		if err != nil && !onlyStopped(err) {
			err = errors.Join(err, r.handle(ctx, r.plan.root))
		}
	}

	close(done)
	watcher.Wait()

	_ = r.stopping()

	return err
}

// stoppedErr is the error a stopped or cancelled run returns: ErrStopped
// alone if nothing failed, or joined with the failures. A cancel's failures
// carry the context's error, so that still matches too.
func stoppedErr(err error) error {
	switch {
	case onlyStopped(err):
		return ErrStopped
	case errors.Is(err, ErrStopped):
		return err
	default:
		return errors.Join(err, ErrStopped)
	}
}

// element runs one group or step and returns its failure, if any.
func (r *runner) element(ctx context.Context, n *node) error {
	switch n.kind { //nolint:exhaustive // Every other kind is a step.
	case KindGroup:
		return r.group(ctx, n)
	case KindGate:
		return r.gate(ctx, n)
	default:
		if skip := r.prior.Element(n.path).Skip; n.boundary != nil && skip != "" {
			return r.refuseSkip(n, skip)
		}

		return r.step(ctx, n, false)
	}
}

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
			r.drainFailed.Store(n.path, true)
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

		raw, ok := r.recalled(n.order.Path, n.order.Key)
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

		r.forget(n.path)
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
	failed := false

	r.drainFailed.Range(func(path, _ any) bool {
		p, _ := path.(string)
		failed = strings.HasPrefix(p, n.path+"/")

		return !failed
	})

	return failed
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

// emit stores e in the journal, then gives it to the observers. If the
// journal can't be written the run is cancelled, and every later emit
// returns the same error.
func (r *runner) emit(e Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.emitLocked(e)
}

func (r *runner) emitLocked(e Event) error {
	if r.fatal != nil {
		return r.fatal
	}

	e.Run, e.V = r.id, r.version
	if e.TS.IsZero() {
		e.TS = r.clock.Now().UTC()
	}

	stored, err := r.journal.append(r.redact.event(e))
	if err != nil {
		r.fatal = err
		r.cancel(r.fatal)

		return r.fatal
	}

	r.deliver(stored)

	// Delivering may have written warnings about panicking observers; if one
	// of those writes failed, the journal is broken and the caller must stop.
	return r.fatal
}

// deliver hands e to every observer, dropping any that panic and recording
// a warning for each.
func (r *runner) deliver(e Event) {
	var panicked []string

	kept := make([]Observer, 0, len(r.observers))

	for _, o := range r.observers {
		if v := observe(r.obsCtx, o, e); v != nil {
			panicked = append(panicked, fmt.Sprintf("observer %T panicked and was removed: %v", o, v))

			continue
		}

		kept = append(kept, o)
	}

	r.observers = kept

	for _, msg := range panicked {
		_ = r.emitLocked(Event{Kind: EventMessage, Level: LevelWarn, Message: msg})
	}
}

// observe calls o and returns what it panicked with, if anything.
func observe(ctx context.Context, o Observer, e Event) any {
	var recovered any

	func() {
		defer func() { recovered = recover() }()

		o.OnEvent(ctx, e)
	}()

	return recovered
}

// markCut records that a cancel of the run stopped something. A cancel of
// one group's children (CancelOnError) is that group's failure, not the run's.
func (r *runner) markCut() {
	select {
	case <-r.runDone:
		r.cut.Store(true)
	default:
	}
}

func (r *runner) fatalErr() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.fatal
}

func (r *runner) since(start time.Time) int64 {
	return r.clock.Now().Sub(start).Milliseconds()
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

// runIDBytes is the size of a run ID before hex encoding.
const runIDBytes = 8

func hostname() string {
	host, _ := os.Hostname()

	return host
}

func newRunID() string {
	b := make([]byte, runIDBytes)
	_, _ = rand.Read(b)

	return hex.EncodeToString(b)
}
