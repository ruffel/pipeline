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

	// Step fields.
	step     Step
	hasCheck bool
	hasUndo  bool
	tries    int
	backoff  time.Duration
	timeout  time.Duration
}

// NewPlan validates elems and returns them as a plan named name. The plan is
// the root of the tree: a sequential group whose path is its name.
//
// It reports every problem it finds, each prefixed with the path of the
// element at fault: an invalid or duplicate name, a nil step, a negative
// limit, retry count or timeout, or an Observe step that has Check or Undo.
// Empty groups are allowed.
func NewPlan(name string, elems ...Element) (*Plan, error) {
	b := &builder{}
	root := b.group(nil, Group(name, elems...))

	if err := errors.Join(b.errs...); err != nil {
		return nil, err
	}

	return &Plan{root: root}, nil
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

	// Step fields: whether the step implements [Checker] and [Undoer], and its
	// settings. Durations are in nanoseconds, as encoding/json writes them.
	Check   bool          `json:"check,omitempty"`
	Undo    bool          `json:"undo,omitempty"`
	Tries   int           `json:"tries,omitempty"`
	Backoff time.Duration `json:"backoff,omitempty"`
	Timeout time.Duration `json:"timeout,omitempty"`
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

	if n.kind == KindGroup {
		s.Failure = n.failure

		for _, c := range n.children {
			s.Children = append(s.Children, c.spec())
		}
	}

	return s
}

// canSettle reports whether n has an Effect or Ensure step beneath it. A
// group of only Observe steps has nothing to finish, so it runs on every
// attempt.
func (n *node) canSettle() bool {
	if n.kind == KindEffect || n.kind == KindEnsure {
		return true
	}

	return slices.ContainsFunc(n.children, (*node).canSettle)
}

// walk calls fn on n and its descendants, depth first, in plan order.
func (n *node) walk(fn func(*node)) {
	fn(n)

	for _, c := range n.children {
		c.walk(fn)
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

	return n
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

// isNil reports whether step is nil, or holds a nil func, pointer, map,
// slice, channel or interface, any of which would panic when Run is called.
func isNil(step Step) bool {
	if step == nil {
		return true
	}

	v := reflect.ValueOf(step)

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
