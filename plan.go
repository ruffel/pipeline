package pipeline

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
	// it resets the group first. handles is the group that a failure handler
	// is the handler of.
	handler  *node
	recovers bool
	handles  *node
	// drain marks a DrainOnStop group. boundary is the DrainOnStop group
	// that an element is, or is inside, if any, and aroundDrain whether a
	// group has one beneath it.
	drain       bool
	boundary    *node
	aroundDrain bool
	// order is where the order of an OrderFrom group's children is recorded.
	order *OrderSource

	// Step fields.
	step     Step
	hasCheck bool
	hasUndo  bool
	tries    int
	backoff  time.Duration
	timeout  time.Duration
	// irreversible marks a step that Rollback will not go back past.
	irreversible bool

	// Gate fields.
	gate *gateSettings
}

// NewPlan validates elems and returns them as a plan named name. The plan is
// the root of the tree: a sequential group whose path is its name. It is
// [NewPlanFromGroup] of [Group](name, elems...).
//
// It reports every problem it finds, each prefixed with the path of the
// element at fault: an invalid or duplicate name, a nil step, a negative
// limit, retry count or timeout, an Observe step that has Check or Undo, an
// irreversible step that has Undo or is an Observe step, a
// gate with a nil approver or a timeout and policy that don't go together,
// a group's failure handler that is not an Effect step, is named like one
// of the group's children, or is one of two, a [GroupNode.DrainOnStop]
// group with something in it that could keep a stop waiting or end the group
// early, a failure handler without a timeout on a group around one, or a
// [GroupNode.OrderFrom] group that is parallel, or whose order is to come
// from a step that may not have run when it starts. Empty groups are
// allowed.
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
	b.orders(n)

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
//
// A failure handler is listed after its group's children, marked with its
// role, so moving a step between a group's children and its handler, or
// changing a handler between cleanup and recovery, changes the fingerprint.
func (p *Plan) Fingerprint() string {
	h := sha256.New()

	var write func(n *node, role string)

	write = func(n *node, role string) {
		_, _ = fmt.Fprintf(h, "%s %s%s\n", n.kind, n.path, role)

		for _, c := range n.children {
			write(c, "")
		}

		switch {
		case n.handler != nil && n.recovers:
			write(n.handler, " recovery")
		case n.handler != nil:
			write(n.handler, " cleanup")
		}
	}

	write(p.root, "")

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
	// DrainOnStop marks a group that a graceful stop lets finish. See
	// [GroupNode.DrainOnStop].
	DrainOnStop bool `json:"drainOnStop,omitempty"`
	// OrderFrom is where the order of the group's children is recorded, for
	// a group that takes it from a step. See [GroupNode.OrderFrom].
	OrderFrom *OrderSource `json:"orderFrom,omitempty"`

	// Step fields: whether the step implements [Checker] and [Undoer], and its
	// settings. Durations are in nanoseconds, as encoding/json writes them.
	Check bool `json:"check,omitempty"`
	Undo  bool `json:"undo,omitempty"`
	// Irreversible marks a step that [Rollback] will not go back past.
	Irreversible bool          `json:"irreversible,omitempty"`
	Tries        int           `json:"tries,omitempty"`
	Backoff      time.Duration `json:"backoff,omitempty"`
	Timeout      time.Duration `json:"timeout,omitempty"`

	// Gate fields. Timeout above is the gate's.
	Prompt    string        `json:"prompt,omitempty"`
	OnTimeout TimeoutPolicy `json:"onTimeout,omitempty"`
}

func (n *node) spec() NodeSpec {
	s := NodeSpec{
		Name:         n.name,
		Path:         n.path,
		Kind:         n.kind,
		Description:  n.description,
		Labels:       maps.Clone(n.labels),
		Parallel:     n.parallel,
		Limit:        n.limit,
		Check:        n.hasCheck,
		Undo:         n.hasUndo,
		Irreversible: n.irreversible,
		Tries:        n.tries,
		Backoff:      n.backoff,
		Timeout:      n.timeout,
	}

	if n.gate != nil {
		s.Prompt, s.Timeout, s.OnTimeout = n.gate.prompt, n.gate.timeout, n.gate.onTimeout
	}

	if n.kind == KindGroup {
		s.Failure = n.failure
		s.DrainOnStop = n.drain

		if n.order != nil {
			from := *n.order
			s.OrderFrom = &from
		}

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

// walk calls visit for n and everything beneath it, in plan order: a group's
// failure handler after its children.
func (n *node) walk(visit func(*node)) {
	visit(n)

	for _, c := range n.children {
		c.walk(visit)
	}

	if n.handler != nil {
		n.handler.walk(visit)
	}
}

// walkIn is walk, with each group's children in the order that order gives
// them.
func (n *node) walkIn(order func(*node) []*node, visit func(*node)) {
	visit(n)

	for _, c := range order(n) {
		c.walkIn(order, visit)
	}

	if n.handler != nil {
		n.handler.walkIn(order, visit)
	}
}

// arranged returns the group's children in the order names gives them. It
// reports what is wrong with an order that does not name each of them
// exactly once, as the rest of a sentence about the order.
func (n *node) arranged(names []string) ([]*node, error) {
	left := make(map[string]*node, len(n.children))
	for _, c := range n.children {
		left[c.name] = c
	}

	out := make([]*node, 0, len(n.children))

	for _, name := range names {
		c, ok := left[name]

		switch {
		case ok:
		case slices.ContainsFunc(n.children, func(c *node) bool { return c.name == name }):
			return nil, fmt.Errorf("names %q twice", name)
		default:
			return nil, fmt.Errorf("names %q, which is not one of the group's children", name)
		}

		delete(left, name)

		out = append(out, c)
	}

	if len(left) == 0 {
		return out, nil
	}

	missing := make([]string, 0, len(left))

	for _, c := range n.children {
		if _, ok := left[c.name]; ok {
			missing = append(missing, c.name)
		}
	}

	return nil, fmt.Errorf("leaves out %s", strings.Join(missing, ", "))
}

// version returns the schema version an attempt at the plan is written as:
// the oldest that says everything about it.
func (p *Plan) version() int {
	version := schemaBase

	p.root.walk(func(n *node) {
		if n.drain {
			version = max(version, schemaDrain)
		}

		if n.order != nil {
			version = max(version, schemaOrder)
		}
	})

	return version
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
		n.aroundDrain = n.aroundDrain || c.drain || c.aroundDrain
	}

	b.handler(n, g, seen)
	b.order(parent, n, g)

	if g.drain {
		n.drain = true

		if parent == nil {
			b.failf(n.path, "the plan's root may not be a DrainOnStop group: a stop would have nothing left to stop")
		}

		b.drain(n, n, "a DrainOnStop group")
	}

	return n
}

// order notes where an OrderFrom group's order is to come from, and reports
// a group that can't take its order from a step.
func (b *builder) order(parent, n *node, g GroupNode) {
	if g.order == nil {
		return
	}

	from := *g.order
	n.order = &from

	switch {
	case parent == nil:
		b.failf(n.path, "the plan's root may not take its order from a step: no step comes before it")
	case from.Path == "" || from.Key == "":
		b.failf(n.path, "OrderFrom needs the path of a step, and the key it records the order under")
	}

	if g.parallel {
		b.failf(n.path, "a group that takes its order from a step may not be parallel")
	}
}

// orders checks, once the whole plan is built, that every OrderFrom group
// takes its order from a step that has run by the time the group starts.
func (b *builder) orders(root *node) {
	byPath := map[string]*node{}

	root.walk(func(n *node) { byPath[n.path] = n })

	root.walk(func(n *node) {
		// The root and a group with no path to name have been reported.
		if n.order == nil || n == root || n.order.Path == "" || n.order.Key == "" {
			return
		}

		if why := sourceProblem(byPath, n); why != "" {
			b.failf(n.path, "its order is to come from %s, which %s", n.order.Path, why)
		}
	})
}

// sourceProblem says why the step that the order of n is to come from can't
// be relied on to have run when n starts, or "".
//
// Coming earlier in the plan is not enough. What decides it is the group
// where the two part ways, which holds the step in one of its children and
// n in another: that group has to run the step's child first, every time.
// One that runs its children at once does not, and nor does one whose own
// order is decided during the run.
func sourceProblem(byPath map[string]*node, n *node) string {
	source, ok := byPath[n.order.Path]
	if !ok || source.kind == KindGroup || source.kind == KindGate {
		return "is not a step in the plan"
	}

	from, to := strings.Split(source.path, "/"), strings.Split(n.path, "/")

	at := 0
	for at < len(from) && at < len(to) && from[at] == to[at] {
		at++
	}

	// The step is inside the group.
	if at == len(from) || at == len(to) {
		return "does not come before it"
	}

	fork := byPath[strings.Join(from[:at], "/")]

	// A failure handler runs after every child of its group.
	place := func(name string) int {
		if fork.handler != nil && fork.handler.name == name {
			return len(fork.children)
		}

		return slices.IndexFunc(fork.children, func(c *node) bool { return c.name == name })
	}

	switch {
	case place(from[at]) > place(to[at]):
		return "does not come before it"
	case fork.parallel:
		return fmt.Sprintf("may not have run when it starts: %s runs its children at once", fork.path)
	case fork.order != nil:
		return fmt.Sprintf("may not have run when it starts: %s takes its own order from a step", fork.path)
	default:
		return ""
	}
}

// drain marks n as part of the DrainOnStop group boundary, and reports what
// n has that could keep a stop waiting for ever, or end the group early
// without a failure. what says which group n is, for a group.
func (b *builder) drain(boundary, n *node, what string) {
	n.boundary = boundary

	switch n.kind { //nolint:exhaustive // Every other kind is a step.
	case KindGate:
		b.failf(n.path, "a gate may not be inside a DrainOnStop group: it could keep a stop waiting for ever")
	case KindGroup:
		switch {
		case n.drain && n != boundary:
			b.failf(n.path, "a DrainOnStop group may not be inside another")
		case n.parallel:
			b.failf(n.path, "%s may not be parallel", what)
		case n.failure != FailureStop:
			b.failf(n.path, "%s must stop at its first failure", what)
		}

		if n.handler != nil {
			b.failf(n.path, "%s may not have a failure handler", what)
		}

		for _, c := range n.children {
			b.drain(boundary, c, "a group inside a DrainOnStop group")
		}
	default:
		// A nil step has been reported, and has no timeout to speak of.
		if n.timeout == 0 && !isNil(n.step) {
			b.failf(n.path, "a step inside a DrainOnStop group needs a Timeout, so that it can't keep a stop waiting for ever")
		}
	}
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
	n.handler.handles = n

	if h.kind != KindEffect {
		b.failf(n.handler.path, "a failure handler must be an effect step, not %s", h.kind)
	}

	if names[h.name] {
		b.failf(n.handler.path, "a failure handler may not share its name with one of the group's children")
	}

	// A stop waits for the handler when the DrainOnStop group fails. A nil
	// step has been reported, and has no timeout to speak of.
	if n.aroundDrain && h.timeout == 0 && !isNil(h.step) {
		b.failf(n.handler.path, "the failure handler of a group around a DrainOnStop group needs a Timeout, "+
			"so that it can't keep a stop waiting for ever")
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

	n.irreversible = s.irreversible

	switch {
	case s.irreversible && s.kind == KindObserve:
		b.failf(n.path, "an observe step changes nothing, so it may not be irreversible")
	case s.irreversible && n.hasUndo:
		b.failf(n.path, "an irreversible step may not have Undo")
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
