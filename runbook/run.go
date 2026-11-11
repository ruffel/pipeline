package runbook

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
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
// starts nothing new. The run's outcome is then [OutcomeCancelled].
//
// The journal must be new: resuming a journal that has attempts is not
// supported yet ([ErrResumeNotSupported]).
func Run(ctx context.Context, plan *Plan, j *Journal, opts ...Option) (*Result, error) {
	cfg := config{clock: systemClock{}, outputBudget: MaxStepOutput}
	for _, o := range opts {
		o(&cfg)
	}

	prev, err := j.State()
	if err != nil {
		return nil, err
	}

	if len(prev.Attempts) > 0 {
		return nil, ErrResumeNotSupported
	}

	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	r := &runner{
		plan:      plan,
		journal:   j,
		clock:     cfg.clock,
		id:        newRunID(),
		attempt:   len(prev.Attempts) + 1,
		observers: cfg.observers,
		obsCtx:    context.WithoutCancel(ctx),
		cancel:    cancel,
		runDone:   runCtx.Done(),
		prev:      prev,
		values:    map[string]map[string]json.RawMessage{},
		budget:    cfg.outputBudget,
	}

	return r.run(runCtx, cfg.labels)
}

// runner holds one attempt's state.
type runner struct {
	plan    *Plan
	journal *Journal
	clock   Clock
	id      string
	attempt int
	cancel  context.CancelCauseFunc
	prev    *State

	// runDone is closed when the run is cancelled. cut records that a cancel
	// of the run stopped something: a step it interrupted, or children it
	// kept from starting.
	runDone <-chan struct{}
	cut     atomic.Bool

	// skipRun is set when a step returns ErrSkipRun: no element starts after.
	skipRun atomic.Bool

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
}

func (r *runner) run(ctx context.Context, labels L) (*Result, error) {
	start := r.clock.Now()
	spec := r.plan.Spec()

	err := r.emit(Event{
		Kind: EventRunStarted, Path: r.plan.root.path, Attempt: r.attempt,
		Plan: &spec, Fingerprint: r.plan.Fingerprint(), Labels: labels,
	})
	if err != nil {
		return &Result{Run: r.id, Attempt: r.attempt, Outcome: OutcomeFailed}, err
	}

	err = r.children(ctx, r.plan.root)

	outcome := OutcomePassed

	// The run is cancelled only if the cancel stopped something. A cancel that
	// arrived after the work ended (even after a failure) changes nothing.
	switch {
	case err == nil:
	case r.cut.Load():
		outcome = OutcomeCancelled
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

// element runs one group or step and returns its failure, if any.
func (r *runner) element(ctx context.Context, n *node) error {
	if n.kind == KindGroup {
		return r.group(ctx, n)
	}

	return r.step(ctx, n)
}

func (r *runner) group(ctx context.Context, n *node) error {
	start := r.clock.Now()

	if err := r.emit(Event{Kind: EventGroupStarted, Path: n.path}); err != nil {
		return err
	}

	err := r.children(ctx, n)

	outcome := OutcomePassed
	if err != nil {
		outcome = OutcomeFailed
	}

	if ferr := r.emit(Event{
		Kind: EventGroupFinished, Path: n.path, Outcome: outcome, Error: errorInfo(err), DurationMs: r.since(start),
	}); ferr != nil {
		return ferr
	}

	return err
}

// children runs a group's children under its failure policy. Children not
// started because of a failure or a cancel get no events: replay reports
// them as not reached. A group cut short by a cancel returns the context's
// error, even if no child failed.
func (r *runner) children(ctx context.Context, n *node) error {
	if n.parallel {
		return r.parallel(ctx, n)
	}

	var errs []error

children:
	for i, c := range n.children {
		if r.skipRun.Load() {
			errs = append(errs, r.skipRest(n.children[i:], ReasonRunSkipped))

			break
		}

		if ctx.Err() != nil {
			errs = append(errs, context.Cause(ctx))

			r.markCut()

			break
		}

		err := r.element(ctx, c)

		switch {
		case err == nil:
		case errors.Is(err, ErrSkipGroup):
			errs = append(errs, r.skipRest(n.children[i+1:], ReasonGroupSkipped))

			break children
		case errors.Is(err, ErrSkipRun):
			// The next iteration records the rest as skipped.
		default:
			errs = append(errs, err)

			if n.failure != FailureContinue {
				// A run-wide skip signalled inside the failed child (after an
				// earlier failure there) still applies to the rest.
				if r.skipRun.Load() {
					errs = append(errs, r.skipRest(n.children[i+1:], ReasonRunSkipped))
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
			}
		}

		if g.admit(ctx, r) {
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

// admit reports whether the next child must not start. A cancel that stops
// it is recorded.
func (g *parallelGroup) admit(ctx context.Context, r *runner) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	skip := g.skip != "" || r.skipRun.Load()

	if !g.stopped && !skip && ctx.Err() != nil {
		g.errs = append(g.errs, context.Cause(ctx))

		r.markCut()
	}

	return g.stopped || skip || ctx.Err() != nil
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

// step runs a step's tries: Check, then Run if the work isn't done.
func (r *runner) step(ctx context.Context, n *node) error {
	start := r.clock.Now()

	if err := r.emit(Event{Kind: EventStepStarted, Path: n.path}); err != nil {
		return err
	}

	sc := &StepContext{r: r, n: n}

	outcome, reason, signal, err := r.tries(ctx, n, sc)

	if signal == SignalSkipRun {
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
		Kind: EventStepFinished, Path: n.path, Outcome: outcome, Reason: reason, Signal: signal,
		Error: errorInfo(err), DurationMs: r.since(start),
	}); ferr != nil {
		return ferr
	}

	switch {
	case err != nil:
		return &StepError{Path: n.path, Err: err}
	case signal == SignalSkipGroup:
		return ErrSkipGroup
	case signal == SignalSkipRun:
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
func (r *runner) tries(ctx context.Context, n *node, sc *StepContext) (Outcome, Reason, Signal, error) {
	ranOnce := false

	for try := 1; ; try++ {
		done, err := r.try(ctx, n, sc, &ranOnce)

		if signal := signalOf(err); signal != "" {
			return OutcomePassed, "", signal, nil
		}

		switch {
		case err == nil && done && ranOnce:
			return OutcomePassed, ReasonRecovered, "", nil
		case err == nil && done:
			return OutcomeSkipped, ReasonDone, "", nil
		case err == nil:
			return OutcomePassed, "", "", nil
		case try >= n.tries || ctx.Err() != nil || r.fatalErr() != nil:
			return OutcomeFailed, "", "", err
		}

		if eerr := r.emit(Event{Kind: EventStepRetry, Path: n.path, Try: try, Error: errorInfo(err)}); eerr != nil {
			return OutcomeFailed, "", "", eerr
		}

		select {
		case <-ctx.Done():
			return OutcomeFailed, "", "", errors.Join(err, ctx.Err())
		case <-r.clock.After(n.backoff):
		}
	}
}

// try runs one Check and, if the work isn't done, Run. It reports whether
// Check found the work done.
func (r *runner) try(ctx context.Context, n *node, sc *StepContext, ranOnce *bool) (bool, error) {
	if n.timeout > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, n.timeout)
		defer cancel()
	}

	if checker, ok := n.step.(Checker); ok {
		var done bool

		err := safely(func() error {
			var err error

			done, err = checker.Check(ctx, sc)

			return err
		})

		if signalOf(err) != "" {
			// Only Run may end its group or the run; from Check it is a bug.
			return false, fmt.Errorf("runbook: Check returned %q; only Run may skip", err.Error())
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

	e.Run = r.id
	if e.TS.IsZero() {
		e.TS = r.clock.Now().UTC()
	}

	stored, err := r.journal.append(e)
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

func newRunID() string {
	b := make([]byte, runIDBytes)
	_, _ = rand.Read(b)

	return hex.EncodeToString(b)
}
