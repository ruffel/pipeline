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
// [Ensure] or [Observe].
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
