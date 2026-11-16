package pipeline

import (
	"maps"
	"time"
)

// Kind says what an element of a plan is.
type Kind string

// The kinds of element.
const (
	// KindGroup holds other elements, run in order or in parallel.
	KindGroup Kind = "group"

	// KindEffect changes the system once. Once it has passed, later attempts
	// verify it with Check, or skip it.
	KindEffect Kind = "effect"

	// KindEnsure enforces a setting that can drift, so it is applied again on
	// every attempt that enters its group.
	KindEnsure Kind = "ensure"

	// KindObserve only reads the system, so it runs on every attempt that
	// enters its group.
	KindObserve Kind = "observe"

	// KindGate waits for approval. Once released, later attempts don't wait
	// again.
	KindGate Kind = "gate"
)

// FailurePolicy says what a group does when one of its children fails.
type FailurePolicy string

// The failure policies.
const (
	// FailureStop starts no more children and lets running ones finish. It is
	// the default.
	FailureStop FailurePolicy = "stop"

	// FailureCancel starts no more children and cancels running ones.
	FailureCancel FailurePolicy = "cancel"

	// FailureContinue runs every child, and fails the group at the end.
	FailureContinue FailurePolicy = "continue"
)

// L is a set of labels, such as {"dc": "DC2", "node": "node7"}. An element
// inherits its parents' labels, and its own value for a key wins.
type L map[string]string

// Element is a group or a step in a plan. Build one with [Group], [Effect],
// [Ensure], [Observe] or [Gate].
type Element interface {
	element()
}

// GroupNode is a group of elements, built with [Group]. Its methods return a
// modified copy.
type GroupNode struct {
	name        string
	description string
	labels      L
	parallel    bool
	limit       int
	failure     FailurePolicy
	children    []Element
	// drain makes the group one unit to a graceful stop.
	drain bool

	// The failure handler, if any: cleanup or recovery.
	cleanup  *StepNode
	recovery *StepNode
}

// Group returns a sequential group of the given elements.
func Group(name string, children ...Element) GroupNode {
	return GroupNode{
		name:     name,
		failure:  FailureStop,
		children: append([]Element(nil), children...),
	}
}

// Parallel runs the group's children concurrently, at most limit at a time.
// A limit of zero means no limit. A child that is itself a group counts as one.
func (g GroupNode) Parallel(limit int) GroupNode {
	g.parallel = true
	g.limit = limit

	return g
}

// CancelOnError cancels running children when one fails, instead of letting
// them finish.
func (g GroupNode) CancelOnError() GroupNode {
	g.failure = FailureCancel

	return g
}

// ContinueOnError runs every child even when some fail. The group fails at the
// end if any child failed.
func (g GroupNode) ContinueOnError() GroupNode {
	g.failure = FailureContinue

	return g
}

// OnFailure names a cleanup step: an Effect step run when the group fails,
// to put back what the group changed around its work, such as turning a
// service's monitoring back on. It leaves the group's work, and the record
// of it, alone: a later attempt resumes the group from where it failed.
//
// The step runs, at a path of its own under the group, when the group fails
// during a run; not when it is stopped, or once a stop or cancel has been
// requested. It runs whenever the group fails, even if it passed in an
// earlier attempt. The group still fails, whatever the step does. A step
// that fails or is interrupted is not run again on resume: the next attempt
// carries on, and runs it again only if the group fails again.
//
// Because the group's record is left alone, a step of the group that the
// handler reverses is still recorded as passed. If that step is an Effect
// with no Check, a later attempt skips it: make it an Ensure step, or give it
// a Check.
//
// A group has one failure handler: OnFailure or [GroupNode.RecoverOnFailure].
func (g GroupNode) OnFailure(step StepNode) GroupNode {
	g.cleanup = &step

	return g
}

// RecoverOnFailure names a recovery step: an Effect step run when the group
// fails, to take the group back to where it started, such as putting a node
// back on its old version. It runs as a [GroupNode.OnFailure] step does,
// except that the group is reset first: everything in it goes back to never
// started, its recorded values and items are forgotten, and a later attempt
// runs the whole group again.
//
// The reset is recorded when the step starts, not when it passes: once
// recovery has begun, what state the group is in is unknown, whatever
// becomes of the step, so the only safe resume is from the group's first
// step. The group's steps must tolerate what a failed recovery leaves.
//
// The reset is where the step starts: a stop requested after it, even by an
// observer of the reset, lets the step finish, as it does any running step.
func (g GroupNode) RecoverOnFailure(step StepNode) GroupNode {
	g.recovery = &step

	return g
}

// DrainOnStop makes the group one unit to a graceful stop: once the group
// has started, a stop does not keep the rest of it from starting. The group
// runs on to its end and the stop takes effect after it: nothing else
// starts, and the run finishes as stopped. It is for a sequence that must
// not be left half done while it is healthy, such as a node's stop, install,
// start and verify.
//
// A stop requested before the group starts keeps it from starting, as it
// does any element. And only a stop is put off. A step that fails, panics or
// passes its timeout fails the group at once, and the rest of it does not
// run. Cancelling the run cancels the step that is running and starts
// nothing more. A step that fails once a stop is pending is not tried
// again, however many tries it has left: the group has failed, and so has
// the run. A journal that can't be written stops the run. So a stop can
// still find the group failed, part-way through: finishing it is what a
// healthy group does, not a promise, and it is no protection against the
// process dying. [State.Draining] says which groups a stop is waiting for.
//
// A stop that comes while a step is waiting to be tried again ends the
// wait, as it does in any group: the step has failed, and the run is
// stopped, with that failure in its error.
//
// Nothing in the group may keep a stop waiting for ever, or end the group
// early as passed. So the group, and every group in it, is sequential and
// stops at the first failure; every step in it has a [StepNode.Timeout]; it
// has no gate, failure handler or DrainOnStop group inside it; and a step in
// it whose Run returns [ErrSkipGroup] or [ErrSkipRun] fails. [NewPlan]
// reports a group that breaks these. A step elsewhere that ends the run
// early with ErrSkipRun does not cut the group short either. The plan's
// root may not drain.
//
// On resume the group is like any other: one that passed is settled and
// skipped, and one that was interrupted is entered again, where each step's
// Check says what is left. [Rollback] takes no notice of it: there a stop
// takes effect between any two Undos.
//
// An attempt whose plan has such a group is written as schema version 2,
// so that a program built before the option existed refuses the journal,
// and does not resume it with a plan that stops in the middle of the group.
func (g GroupNode) DrainOnStop() GroupNode {
	g.drain = true

	return g
}

// Labels sets labels on the group. Its descendants inherit them.
func (g GroupNode) Labels(l L) GroupNode {
	g.labels = maps.Clone(l)

	return g
}

// Description sets a human-readable summary of the group.
func (g GroupNode) Description(d string) GroupNode {
	g.description = d

	return g
}

func (GroupNode) element() {}

// StepNode is a step in a plan, built with [Effect], [Ensure] or [Observe].
// Its methods return a modified copy.
type StepNode struct {
	name        string
	description string
	kind        Kind
	step        Step
	labels      L
	tries       int
	backoff     time.Duration
	timeout     time.Duration
	// irreversible marks a step whose effect can't be undone.
	irreversible bool
}

// Effect returns a step that changes the system once. Once it has passed,
// later attempts check it with its Check, or skip it if it has none. Until
// then, every attempt that reaches it runs it.
func Effect(name string, step Step) StepNode {
	return newStepNode(name, KindEffect, step)
}

// Ensure returns a step that enforces a setting which can drift between
// attempts. It is applied again on every attempt that enters its group. A
// group that has passed is not entered again, so an Ensure step inside one
// holds only until that group passes: put a setting that must hold for the
// whole run at the root of the plan. [Describe] marks the difference.
func Ensure(name string, step Step) StepNode {
	return newStepNode(name, KindEnsure, step)
}

// Observe returns a step that only reads the system, such as a health check.
// It runs on every attempt that enters its group, and may not have Check or
// Undo.
func Observe(name string, step Step) StepNode {
	return newStepNode(name, KindObserve, step)
}

func newStepNode(name string, kind Kind, step Step) StepNode {
	return StepNode{name: name, kind: kind, step: step, tries: 1}
}

// Retry allows a step up to n tries within one attempt, waiting backoff
// between them. A try is Check followed by Run.
func (s StepNode) Retry(n int, backoff time.Duration) StepNode {
	s.tries = n
	s.backoff = backoff

	return s
}

// Timeout bounds each try, and Undo, to d, by cancelling the context they
// are given. A step has to watch its context to be stopped: one that ignores
// it runs on. When it does return, the try has failed with a timeout,
// whatever it returns, because a step that took too long has not done what
// the plan allowed for. A step that returns after a cancel has failed in the
// same way.
func (s StepNode) Timeout(d time.Duration) StepNode {
	s.timeout = d

	return s
}

// Irreversible marks a step whose effect can't be undone, such as a schema
// change or a format: the point of no return. Once any attempt has called
// its Run, whether that Run passed, failed or was interrupted, [Rollback]
// refuses the journal with [ErrIrreversible], and undoes nothing, because
// undoing the steps around it would not put the system back where it
// started. [State.PointOfNoReturn] says whether that point has been passed.
//
// Until the step runs, a rollback goes ahead as usual. Once it has, the
// journal says so for good: a later build that takes the marker off, or
// takes the step out of the plan, is still refused.
//
// An irreversible step may not have an Undo, and may not be an Observe step.
func (s StepNode) Irreversible() StepNode {
	s.irreversible = true

	return s
}

// Labels sets labels on the step.
func (s StepNode) Labels(l L) StepNode {
	s.labels = maps.Clone(l)

	return s
}

// Description sets a human-readable summary of the step.
func (s StepNode) Description(d string) StepNode {
	s.description = d

	return s
}

func (StepNode) element() {}
