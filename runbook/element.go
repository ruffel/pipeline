package runbook

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
// A group has one failure handler: OnFailure or [GroupNode.RecoverOnFailure].
func (g GroupNode) OnFailure(step StepNode) GroupNode {
	g.cleanup = &step

	return g
}

// RecoverOnFailure names a recovery step: an Effect step run when the group
// fails, to take the group back to where it started, such as putting a node
// back on its old version. It runs as an [GroupNode.OnFailure] step does,
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
	name         string
	description  string
	kind         Kind
	step         Step
	labels       L
	tries        int
	backoff      time.Duration
	timeout      time.Duration
	irreversible bool
}

// Effect returns a step that changes the system once. On later attempts it is
// checked with its Check, or skipped if it has none.
func Effect(name string, step Step) StepNode {
	return newStepNode(name, KindEffect, step)
}

// Ensure returns a step that enforces a setting which can drift between
// attempts. It is applied again on every attempt that enters its group.
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

// Timeout bounds each try, and Undo, to d.
func (s StepNode) Timeout(d time.Duration) StepNode {
	s.timeout = d

	return s
}

// Irreversible marks an Effect step as a point of no return: once its Run
// has passed, or its Check has found its work done, [Rollback] refuses with
// [ErrIrreversible]. An Irreversible step may not have an Undo.
//
// One whose Run was called and did not pass, interrupted or failed, is in
// doubt: Rollback refuses with [ErrIrreversibleInDoubt] unless the step's
// Check, or the caller with [AssumeNotCommitted], says it did not commit.
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
