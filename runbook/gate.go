package runbook

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"runtime/debug"
	"time"
)

// ErrGateDeclined is the error a gate fails with when it times out under
// [TimeoutDecline].
var ErrGateDeclined = errors.New("runbook: the gate timed out and was declined")

// TimeoutPolicy says what a gate does when it times out.
type TimeoutPolicy string

// The timeout policies.
const (
	// TimeoutContinue releases the gate, as if it were approved.
	TimeoutContinue TimeoutPolicy = "continue"
	// TimeoutDecline fails the gate with [ErrGateDeclined]. A later attempt
	// waits again.
	TimeoutDecline TimeoutPolicy = "decline"
	// TimeoutStop stops the run gracefully, as a [Stopper] does. The gate is
	// not released, and a later attempt waits again.
	TimeoutStop TimeoutPolicy = "stop"
)

// GateOptions configures a [Gate].
type GateOptions struct {
	// Prompt says what is being approved, such as "Upgrade datacenter DC2?".
	Prompt string
	// Approvers can each release the gate; the first to approve wins. On a
	// file journal, [Approve] can always release it too.
	Approvers []Approver
	// Timeout bounds the wait. Zero means the gate waits until it is
	// approved, or the run is stopped or cancelled.
	Timeout time.Duration
	// OnTimeout says what happens when the wait times out. It is required
	// with a Timeout, and not allowed without one.
	OnTimeout TimeoutPolicy
}

// Approver answers a gate. Wait blocks until the approver approves, and
// returns who approved. It returns an error when it can no longer answer, as
// a reader of a lost terminal would: it withdraws, and the gate goes on
// waiting for its other approvers. A panic in Wait or Name withdraws too.
//
// ctx is done when the gate is released some other way, or the run stops or
// is cancelled. Wait must then return; what it returns is ignored.
type Approver interface {
	// Name identifies the approver, such as "stdin". It is recorded as what
	// released the gate.
	Name() string
	Wait(ctx context.Context, gate GateInfo) (who string, err error)
}

// GateInfo is what an [Approver] is asked to approve.
type GateInfo struct {
	Path   string
	Prompt string
	// Deadline is when the gate times out, or zero if it does not.
	Deadline time.Time
}

// GateNode is a gate in a plan, built with [Gate]. Its methods return a
// modified copy.
type GateNode struct {
	name        string
	description string
	labels      L
	opts        GateOptions
}

// Gate returns a step that waits for approval before the run goes on, from
// one of opts.Approvers, from [Approve] in any process (on a file journal),
// or from its timeout policy.
//
// The journal records the gate waiting, with its deadline, and what released
// it. A gate released in an earlier attempt doesn't wait again. One that a
// stop interrupted, or a crash, waits again on resume, with its timeout
// started afresh: a run never goes on by itself sooner than its timeout
// after it starts waiting.
func Gate(name string, opts GateOptions) GateNode {
	opts.Approvers = append([]Approver(nil), opts.Approvers...)

	return GateNode{name: name, opts: opts}
}

// Labels sets labels on the gate.
func (g GateNode) Labels(l L) GateNode {
	g.labels = maps.Clone(l)

	return g
}

// Description sets a human-readable summary of the gate.
func (g GateNode) Description(d string) GateNode {
	g.description = d

	return g
}

func (GateNode) element() {}

// gateSettings is a validated gate's options.
type gateSettings struct {
	prompt    string
	approvers []Approver
	timeout   time.Duration
	onTimeout TimeoutPolicy
}

// How a gate wait ended.
type gateEnd int

const (
	gateApproved gateEnd = iota
	gateTimedOut
	gateStopped
	gateCancelled
)

// gateRelease is how a gate's wait ended, and by whom if it was approved.
type gateRelease struct {
	end gateEnd
	by  string
	who string
	uid *int
}

// answer is an approver's answer: who approved, or why it withdrew.
type answer struct {
	name string
	who  string
	err  error
}

// gate waits for a gate to be released, and records how.
func (r *runner) gate(ctx context.Context, n *node) error {
	if r.prior.Element(n.path).Done {
		return r.emit(Event{
			Kind: EventStepFinished, Path: n.path, Outcome: OutcomeSkipped, Reason: ReasonEarlierAttempt,
		})
	}

	start := r.clock.Now()

	if err := r.emit(Event{Kind: EventStepStarted, Path: n.path}); err != nil {
		return err
	}

	approvals := r.openGate(n.path)
	defer r.closeGate(n.path)

	g := n.gate
	info := GateInfo{Path: n.path, Prompt: g.prompt}

	var timeout <-chan time.Time

	if g.timeout > 0 {
		info.Deadline = r.clock.Now().Add(g.timeout).UTC()
		// The wait is timed on the clock's own timer, which for the system
		// clock is monotonic: a change of wall-clock time doesn't release it.
		timeout = r.clock.After(g.timeout)
	}

	if err := r.emit(Event{
		Kind: EventGateWaiting, Path: n.path, Prompt: g.prompt, Deadline: info.Deadline,
	}); err != nil {
		return err
	}

	rel, err := r.await(ctx, n, info, approvals, timeout)
	if err != nil {
		return err
	}

	return r.released(ctx, n, r.prevailing(ctx, rel), start)
}

// prevailing is how the wait ended once anything else already pending is
// counted. The wait takes whichever of its cases is ready first, at random
// if several are, but a cancel outranks a stop, and either outranks a
// release.
func (r *runner) prevailing(ctx context.Context, rel gateRelease) gateRelease {
	switch {
	case ctx.Err() != nil:
		return gateRelease{end: gateCancelled}
	case rel.end != gateStopped && r.stopping():
		return gateRelease{end: gateStopped}
	default:
		return rel
	}
}

// await waits for the first of: an approval, the timeout, a stop or a
// cancel. Approvers that withdraw are recorded, and the wait goes on.
func (r *runner) await(
	ctx context.Context, n *node, info GateInfo, approvals <-chan controlRequest, timeout <-chan time.Time,
) (gateRelease, error) {
	actx, stopApprovers := context.WithCancel(ctx)
	defer stopApprovers()

	answers := make(chan answer, len(n.gate.approvers))

	for _, a := range n.gate.approvers {
		go ask(actx, a, info, answers)
	}

	for {
		select {
		case req := <-approvals:
			return gateRelease{end: gateApproved, by: stopByControl, who: req.Who, uid: req.uid}, nil
		case a := <-answers:
			if a.err == nil {
				return gateRelease{end: gateApproved, by: a.name, who: a.who}, nil
			}

			if err := r.emit(Event{
				Kind: EventMessage, Path: n.path, Level: LevelWarn,
				Message: fmt.Sprintf("approver %s withdrew: %v", a.name, a.err),
			}); err != nil {
				return gateRelease{}, err
			}
		case <-timeout:
			return gateRelease{end: gateTimedOut}, nil
		case <-r.stopDone():
			return gateRelease{end: gateStopped}, nil
		case <-ctx.Done():
			return gateRelease{end: gateCancelled}, nil
		}
	}
}

// ask asks one approver and sends its answer, recovering a panic as a
// withdrawal. An approver whose Name panics is named by its type.
func ask(ctx context.Context, a Approver, info GateInfo, answers chan<- answer) {
	ans := answer{name: fmt.Sprintf("%T", a)}

	defer func() {
		if v := recover(); v != nil {
			ans.who, ans.err = "", &PanicError{Value: v, Stack: string(debug.Stack())}
		}

		answers <- ans
	}()

	ans.name = a.Name()
	ans.who, ans.err = a.Wait(ctx, info)
}

// released records how the gate's wait ended, and returns what its group
// acts on.
func (r *runner) released(ctx context.Context, n *node, rel gateRelease, start time.Time) error {
	finished := Event{Kind: EventStepFinished, Path: n.path, Outcome: OutcomePassed}

	var err error

	switch {
	case rel.end == gateApproved:
		if eerr := r.emit(Event{
			Kind: EventGateReleased, Path: n.path, By: rel.by, Who: rel.who, UID: rel.uid,
		}); eerr != nil {
			return eerr
		}
	case rel.end == gateTimedOut && n.gate.onTimeout == TimeoutStop:
		// The gate stays unreleased: the run stops before it, as if asked.
		r.stop.fire(StopRequest{Mode: StopGraceful, By: stopByTimeout, Who: n.path})
		r.noteStop()

		fallthrough
	case rel.end == gateStopped:
		// The request is recorded before the gate finishes, as it is before
		// anything else a stop keeps from happening.
		r.noteStop()
		r.halt()

		finished.Outcome, err = OutcomeStopped, ErrStopped
	case rel.end == gateTimedOut:
		if eerr := r.emit(Event{
			Kind: EventGateReleased, Path: n.path, By: stopByTimeout, Policy: n.gate.onTimeout,
		}); eerr != nil {
			return eerr
		}

		if n.gate.onTimeout == TimeoutDecline {
			finished.Outcome, err = OutcomeFailed, ErrGateDeclined
		}
	default:
		r.markCut()

		finished.Outcome, err = OutcomeFailed, context.Cause(ctx)
	}

	finished.Error, finished.DurationMs = errorInfo(err), r.since(start)

	if eerr := r.emit(finished); eerr != nil {
		return eerr
	}

	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrStopped):
		return ErrStopped
	default:
		return &StepError{Path: n.path, Err: r.redact.err(err)}
	}
}

// openGate makes the gate at path ready for approvals from the control
// directory, and returns where they arrive.
func (r *runner) openGate(path string) <-chan controlRequest {
	r.gatesMu.Lock()
	defer r.gatesMu.Unlock()

	ch := make(chan controlRequest, 1)
	r.gates[path] = ch

	return ch
}

func (r *runner) closeGate(path string) {
	r.gatesMu.Lock()
	defer r.gatesMu.Unlock()

	delete(r.gates, path)
}

// approveGate hands an approval to the gate at path, if it is waiting.
func (r *runner) approveGate(req controlRequest) {
	r.gatesMu.Lock()
	defer r.gatesMu.Unlock()

	select {
	case r.gates[req.Gate] <- req:
	default:
		// Not waiting, or already approved.
	}
}
