package runbook

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"time"
)

// Plan is a validated tree of groups and steps. Build one with [NewPlan]. A
// plan does not change after it is built.
type Plan struct {
	root *node
}

// node is one validated element, with its path and effective labels resolved.
type node struct {
	name        string
	path        string
	kind        Kind
	description string
	labels      L

	// Group fields.
	parallel bool
	limit    int
	failure  FailurePolicy
	children []*node
	// handler is the group's failure handler, if any, and recovers whether
	// it resets the group first.
	handler  *node
	recovers bool

	// Step fields.
	step     Step
	hasCheck bool
	hasUndo  bool
	tries    int
	backoff  time.Duration
	timeout  time.Duration

	// Gate fields.
	gate *gateSettings
}

// NewPlan validates elems and returns them as a plan named name. The plan is
// the root of the tree: a sequential group whose path is its name. It is
// [NewPlanFromGroup] of [Group](name, elems...).
//
// It reports every problem it finds, each prefixed with the path of the
// element at fault: an invalid or duplicate name, a nil step, a negative
// limit, retry count or timeout, an Observe step that has Check or Undo, a
// gate with a nil approver or a timeout and policy that don't go together,
// or a group's failure handler that is not an Effect step, is named like one
// of the group's children, or is one of two. Empty groups are allowed.
func NewPlan(name string, elems ...Element) (*Plan, error) {
	return NewPlanFromGroup(Group(name, elems...))
}

// NewPlanFromGroup validates root and returns it as a plan. The plan is the
// group, with all its settings: its name is the plan's name, and its labels,
// description, parallelism, failure policy and failure handler apply to the
// whole run. A failure handler on the root runs when the run fails, and a
// recovery handler there resets the whole run. It reports problems as
// [NewPlan] does.
func NewPlanFromGroup(root GroupNode) (*Plan, error) {
	b := &builder{}
	n := b.group(nil, root)

	if err := errors.Join(b.errs...); err != nil {
		return nil, err
	}

	return &Plan{root: n}, nil
}

// Name returns the plan's name, which is also the first segment of every path.
func (p *Plan) Name() string {
	return p.root.name
}

// Fingerprint identifies the plan's shape: the path and kind of every element,
// in order. Descriptions, labels and settings such as limits and timeouts are
// left out, so changing them does not change the fingerprint.
func (p *Plan) Fingerprint() string {
	h := sha256.New()

	p.root.walk(func(n *node) {
		_, _ = fmt.Fprintf(h, "%s %s\n", n.kind, n.path)
	})

	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// Spec returns the plan without its step functions, ready to serialise.
func (p *Plan) Spec() NodeSpec {
	return p.root.spec()
}

// NodeSpec describes one element of a plan without its step function: its
// identity, labels, description, and group or step settings. It is what the
// journal stores, [Describe] prints, and viewers render.
type NodeSpec struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Kind        Kind   `json:"kind"`
	Description string `json:"description,omitempty"`
	Labels      L      `json:"labels,omitempty"`

	// Group fields.
	Parallel bool          `json:"parallel,omitempty"`
	Limit    int           `json:"limit,omitempty"`
	Failure  FailurePolicy `json:"failure,omitempty"`
	Children []NodeSpec    `json:"children,omitempty"`
	// OnFailure is the group's failure handler, if any, and Recovers whether
	// it resets the group first.
	OnFailure *NodeSpec `json:"onFailure,omitempty"`
	Recovers  bool      `json:"recovers,omitempty"`

	// Step fields: whether the step implements [Checker] and [Undoer], and its
	// settings. Durations are in nanoseconds, as encoding/json writes them.
	Check   bool          `json:"check,omitempty"`
	Undo    bool          `json:"undo,omitempty"`
	Tries   int           `json:"tries,omitempty"`
	Backoff time.Duration `json:"backoff,omitempty"`
	Timeout time.Duration `json:"timeout,omitempty"`

	// Gate fields. Timeout above is the gate's.
	Prompt    string        `json:"prompt,omitempty"`
	OnTimeout TimeoutPolicy `json:"onTimeout,omitempty"`
}

func (n *node) spec() NodeSpec {
	s := NodeSpec{
		Name:        n.name,
		Path:        n.path,
		Kind:        n.kind,
		Description: n.description,
		Labels:      maps.Clone(n.labels),
		Parallel:    n.parallel,
		Limit:       n.limit,
		Check:       n.hasCheck,
		Undo:        n.hasUndo,
		Tries:       n.tries,
		Backoff:     n.backoff,
		Timeout:     n.timeout,
	}

	if n.gate != nil {
		s.Prompt, s.Timeout, s.OnTimeout = n.gate.prompt, n.gate.timeout, n.gate.onTimeout
	}

	if n.kind == KindGroup {
		s.Failure = n.failure

		for _, c := range n.children {
			s.Children = append(s.Children, c.spec())
		}

		if n.handler != nil {
			h := n.handler.spec()
			s.OnFailure, s.Recovers = &h, n.recovers
		}
	}

	return s
}

// canSettle reports whether n has an Effect, Ensure or Gate step beneath it.
// A group of only Observe steps has nothing to finish, so it runs on every
// attempt.
func (n *node) canSettle() bool {
	if n.kind == KindEffect || n.kind == KindEnsure || n.kind == KindGate {
		return true
	}

	return slices.ContainsFunc(n.children, (*node).canSettle)
}

// walk calls fn on n and its descendants, depth first, in plan order, with
// a group's failure handler after its children.
func (n *node) walk(fn func(*node)) {
	fn(n)

	for _, c := range n.children {
		c.walk(fn)
	}

	if n.handler != nil {
		n.handler.walk(fn)
	}
}

// builder turns elements into nodes, collecting every validation error.
type builder struct {
	errs []error
}

func (b *builder) failf(path, format string, args ...any) {
	b.errs = append(b.errs, fmt.Errorf("%s: %s", path, fmt.Sprintf(format, args...)))
}

// base fills the fields every node has, and validates the name.
func (b *builder) base(parent *node, name, description string, kind Kind, labels L) *node {
	path := name
	if parent != nil {
		path = parent.path + "/" + name
	}

	if err := validateName(name); err != nil {
		b.failf(path, "%v", err)
	}

	effective := L{}
	if parent != nil {
		maps.Copy(effective, parent.labels)
	}

	maps.Copy(effective, labels)

	if len(effective) == 0 {
		effective = nil
	}

	return &node{name: name, path: path, kind: kind, description: description, labels: effective}
}

func (b *builder) group(parent *node, g GroupNode) *node {
	n := b.base(parent, g.name, g.description, KindGroup, g.labels)
	n.parallel = g.parallel
	n.limit = g.limit
	n.failure = g.failure

	if g.limit < 0 {
		b.failf(n.path, "parallel limit %d is negative", g.limit)
	}

	seen := make(map[string]bool, len(g.children))

	for _, e := range g.children {
		var c *node

		switch el := e.(type) {
		case GroupNode:
			c = b.group(n, el)
		case StepNode:
			c = b.step(n, el)
		case GateNode:
			c = b.gate(n, el)
		default:
			b.failf(n.path, "unsupported element %T", e)

			continue
		}

		if seen[c.name] {
			b.failf(c.path, "duplicate name")
		}

		seen[c.name] = true
		n.children = append(n.children, c)
	}

	b.handler(n, g, seen)

	return n
}

// handler builds a group's failure handler, if it has one.
func (b *builder) handler(n *node, g GroupNode, names map[string]bool) {
	h, recovers := g.cleanup, false

	switch {
	case g.cleanup != nil && g.recovery != nil:
		b.failf(n.path, "a group has one failure handler, OnFailure or RecoverOnFailure")

		return
	case g.recovery != nil:
		h, recovers = g.recovery, true
	case h == nil:
		return
	}

	n.handler, n.recovers = b.step(n, *h), recovers

	if h.kind != KindEffect {
		b.failf(n.handler.path, "a failure handler must be an effect step, not %s", h.kind)
	}

	if names[h.name] {
		b.failf(n.handler.path, "a failure handler may not share its name with one of the group's children")
	}
}

func (b *builder) step(parent *node, s StepNode) *node {
	n := b.base(parent, s.name, s.description, s.kind, s.labels)
	n.step = s.step
	n.tries = s.tries
	n.backoff = s.backoff
	n.timeout = s.timeout

	if isNil(s.step) {
		b.failf(n.path, "step is nil")

		return n
	}

	_, n.hasCheck = s.step.(Checker)
	_, n.hasUndo = s.step.(Undoer)

	if s.kind == KindObserve && (n.hasCheck || n.hasUndo) {
		b.failf(n.path, "an observe step may not have Check or Undo")
	}

	if s.tries < 1 {
		b.failf(n.path, "retry allows %d tries; it must allow at least 1", s.tries)
	}

	if s.backoff < 0 || s.timeout < 0 {
		b.failf(n.path, "backoff and timeout may not be negative")
	}

	return n
}

func (b *builder) gate(parent *node, g GateNode) *node {
	n := b.base(parent, g.name, g.description, KindGate, g.labels)
	o := g.opts
	n.gate = &gateSettings{prompt: o.Prompt, approvers: o.Approvers, timeout: o.Timeout, onTimeout: o.OnTimeout}

	for i, a := range o.Approvers {
		if isNilValue(a) {
			b.failf(n.path, "approver %d is nil", i)
		}
	}

	switch {
	case o.Timeout < 0:
		b.failf(n.path, "timeout may not be negative")
	case o.Timeout > 0 && o.OnTimeout == "":
		b.failf(n.path, "a timeout needs OnTimeout")
	case o.Timeout == 0 && o.OnTimeout != "":
		b.failf(n.path, "OnTimeout needs a timeout")
	}

	switch o.OnTimeout {
	case "", TimeoutContinue, TimeoutDecline, TimeoutStop:
	default:
		b.failf(n.path, "unknown timeout policy %q", o.OnTimeout)
	}

	return n
}

// isNil reports whether step is nil, or holds a nil func, pointer, map,
// slice, channel or interface, any of which would panic when Run is called.
func isNil(step Step) bool {
	return isNilValue(step)
}

// isNilValue reports whether x is nil, or holds a nil func, pointer, map,
// slice, channel or interface.
func isNilValue(x any) bool {
	if x == nil {
		return true
	}

	v := reflect.ValueOf(x)

	switch v.Kind() { //nolint:exhaustive // Only nil-capable kinds can hold nil.
	case reflect.Func, reflect.Pointer, reflect.Map, reflect.Slice, reflect.Chan, reflect.Interface:
		return v.IsNil()
	default:
		return false
	}
}

// validateName checks a name is usable as a path segment and a file name:
// letters, digits, '.', '_' and '-', starting with a letter or digit.
func validateName(name string) error {
	if name == "" {
		return errors.New("name is empty")
	}

	for i, r := range name {
		alnum := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'

		switch {
		case alnum:
		case i > 0 && strings.ContainsRune("._-", r):
		default:
			return fmt.Errorf("name %q may only use letters, digits, '.', '_' and '-', starting with a letter or digit", name)
		}
	}

	return nil
}
