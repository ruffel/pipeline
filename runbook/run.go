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
	cfg := config{clock: systemClock{}}
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
		prev:      prev,
		values:    map[string]map[string]json.RawMessage{},
	}

	return r.run(runCtx, ctx, cfg.labels)
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

	// mu serialises journal writes and observer calls, so observers see
	// events in journal order.
	mu        sync.Mutex
	observers []Observer
	obsCtx    context.Context //nolint:containedctx // Handed to observers on every event.
	fatal     error

	// valuesMu guards values recorded during this attempt.
	valuesMu sync.Mutex
	values   map[string]map[string]json.RawMessage
}

func (r *runner) run(ctx, parent context.Context, labels L) (*Result, error) {
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

	// A cancel that stopped nothing (it came after the last step) leaves the
	// run passed; groups report a cancel that cut them short as an error.
	switch {
	case err == nil:
	case parent.Err() != nil:
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

	for _, c := range n.children {
		if ctx.Err() != nil {
			errs = append(errs, context.Cause(ctx))

			break
		}

		if err := r.element(ctx, c); err != nil {
			errs = append(errs, err)

			if n.failure != FailureContinue {
				break
			}
		}
	}

	return errors.Join(errs...)
}

func (r *runner) parallel(ctx context.Context, n *node) error {
	gctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		errs    []error
		stopped bool
		slots   chan struct{}
	)

	if n.limit > 0 {
		slots = make(chan struct{}, n.limit)
	}

	for _, c := range n.children {
		if slots != nil {
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
			}
		}

		mu.Lock()
		stop := stopped || ctx.Err() != nil

		if !stopped && ctx.Err() != nil {
			errs = append(errs, context.Cause(ctx))
		}
		mu.Unlock()

		if stop {
			break
		}

		wg.Go(func() {
			defer func() {
				if slots != nil {
					<-slots
				}
			}()

			if err := r.element(gctx, c); err != nil {
				mu.Lock()
				defer mu.Unlock()

				errs = append(errs, err)

				switch n.failure {
				case FailureStop:
					stopped = true
				case FailureCancel:
					stopped = true

					cancel()
				case FailureContinue:
				}
			}
		})
	}

	wg.Wait()

	return errors.Join(errs...)
}

// step runs a step's tries: Check, then Run if the work isn't done.
func (r *runner) step(ctx context.Context, n *node) error {
	start := r.clock.Now()

	if err := r.emit(Event{Kind: EventStepStarted, Path: n.path}); err != nil {
		return err
	}

	sc := &StepContext{r: r, n: n}
	defer sc.close()

	outcome, reason, err := r.tries(ctx, n, sc)

	if ferr := r.emit(Event{
		Kind: EventStepFinished, Path: n.path, Outcome: outcome, Reason: reason,
		Error: errorInfo(err), DurationMs: r.since(start),
	}); ferr != nil {
		return ferr
	}

	if err != nil {
		return &StepError{Path: n.path, Err: err}
	}

	return nil
}

func (r *runner) tries(ctx context.Context, n *node, sc *StepContext) (Outcome, Reason, error) {
	ranOnce := false

	for try := 1; ; try++ {
		done, err := r.try(ctx, n, sc, &ranOnce)

		switch {
		case err == nil && done && ranOnce:
			return OutcomePassed, ReasonRecovered, nil
		case err == nil && done:
			return OutcomeSkipped, ReasonDone, nil
		case err == nil:
			return OutcomePassed, "", nil
		case try >= n.tries || ctx.Err() != nil || r.fatalErr() != nil:
			return OutcomeFailed, "", err
		}

		if eerr := r.emit(Event{Kind: EventStepRetry, Path: n.path, Try: try, Error: errorInfo(err)}); eerr != nil {
			return OutcomeFailed, "", eerr
		}

		select {
		case <-ctx.Done():
			return OutcomeFailed, "", errors.Join(err, ctx.Err())
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
		if err != nil || done {
			return done, err
		}
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

	return nil
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
		_ = r.emitLocked(Event{Kind: EventMessage, Level: "warn", Message: msg})
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
