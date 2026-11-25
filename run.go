package pipeline

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
		recorded:      recorded{prev: prev},
		budget:        cfg.outputBudget,
	}

	res, err := r.run(runCtx, cfg.labels, cfg.data)

	// Every error leaves redacted, not only step failures: a cancel's cause
	// or a journal failure can carry a secret too.
	return res, r.redact.err(err)
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
	// drainMu guards draining, the DrainOnStop groups that have carried on
	// past a stop, each of which says so once, and drainFailed, those that
	// have failed: a stop does not hold back the handling of their failure.
	// Both are by path, and made on first use.
	drainMu     sync.Mutex
	draining    map[string]bool
	drainFailed map[string]bool

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

	// recorded is what the attempt knows of recorded values.
	recorded recorded
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
