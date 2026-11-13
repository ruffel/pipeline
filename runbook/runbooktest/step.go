package runbooktest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/ruffel/pipeline/runbook"
)

// StepOption configures [RunStep].
type StepOption func(*stepConfig)

type stepConfig struct {
	ctx    context.Context //nolint:containedctx // RunStep's context, set by WithContext.
	parent string
	given  []given
	opts   []runbook.Option
}

type given struct {
	path, key string
	value     any
}

// Under runs the step inside the groups of parent, such as
// "upgrade/nodes/node1", so its path is parent's plus its own name, as in
// the real plan. The default parent is "test".
func Under(parent string) StepOption {
	return func(c *stepConfig) {
		c.parent = parent
	}
}

// Given has the step at path record value under key before the step under
// test runs, so the step under test can read it with Recorded. The path must
// be in the same plan as the step under test: it starts with the same group
// as [Under]'s parent.
func Given(path, key string, value any) StepOption {
	return func(c *stepConfig) {
		c.given = append(c.given, given{path: path, key: key, value: value})
	}
}

// WithRunOptions passes options to [runbook.Run], such as
// [runbook.WithInputs], [runbook.WithClock] or [runbook.WithRedactor].
func WithRunOptions(opts ...runbook.Option) StepOption {
	return func(c *stepConfig) {
		c.opts = append(c.opts, opts...)
	}
}

// WithContext runs the step with ctx instead of the test's context, so a
// test can cancel it.
func WithContext(ctx context.Context) StepOption {
	return func(c *stepConfig) {
		c.ctx = ctx
	}
}

// StepRun is what one step did when [RunStep] ran it.
type StepRun struct {
	// Path is the step's path.
	Path string
	// Err is the error [runbook.Run] returned: nil if the step passed or was
	// skipped, and otherwise the step's error in a [runbook.StepError], so
	// errors.Is and errors.As see the step's own error.
	Err error
	// Journal holds the run, for inspecting what it recorded.
	Journal *runbook.Journal

	state  *runbook.State
	events []runbook.Event
}

// RunStep runs step on its own, as a real run would: Check before Run, with
// its tries, timeout and output limits, against a new memory journal. It
// returns what the step recorded. Steps given with [Given] run first, and
// are left out of what it returns.
//
// It fails tb if the step can't be run at all, such as when its name isn't
// valid; a failure of the step itself is in [StepRun.Err].
func RunStep(tb testing.TB, step runbook.StepNode, opts ...StepOption) *StepRun {
	tb.Helper()

	cfg := stepConfig{ctx: tb.Context(), parent: "test"}
	for _, o := range opts {
		o(&cfg)
	}

	plan, path, err := stepPlan(step, cfg)
	if err != nil {
		tb.Fatalf("runbooktest: %v", err)
	}

	j := runbook.MemoryJournal()

	_, runErr := runbook.Run(cfg.ctx, plan, j, cfg.opts...)

	events, err := j.Events()
	if err != nil {
		tb.Fatalf("runbooktest: %v", err)
	}

	state, err := runbook.Replay(events)
	if err != nil {
		tb.Fatalf("runbooktest: %v", err)
	}

	if el := state.Element(path); el.Status == runbook.StatusNotReached {
		tb.Fatalf("runbooktest: %s was not run: %v", path, runErr)
	}

	r := &StepRun{Path: path, Err: runErr, Journal: j, state: state}

	for _, e := range events {
		if e.Path == path {
			r.events = append(r.events, e)
		}
	}

	return r
}

// Element returns where the step stands: its status, reason and error.
func (r *StepRun) Element() runbook.ElementState {
	return r.state.Element(r.Path)
}

// Events returns the step's own events, in the order they were recorded.
func (r *StepRun) Events() []runbook.Event {
	return slices.Clone(r.events)
}

// Recorded decodes into v the value the step recorded under key, and
// reports whether there was one.
func (r *StepRun) Recorded(key string, v any) (bool, error) {
	raw, ok := r.state.Value(r.Path, key)
	if !ok {
		return false, nil
	}

	return true, json.Unmarshal(raw, v)
}

// Items returns the step's latest result for each target.
func (r *StepRun) Items() map[string]runbook.ItemState {
	return r.state.Items(r.Path)
}

// Messages returns the messages the step recorded with Info and Warn.
func (r *StepRun) Messages() []string {
	var out []string

	for _, e := range r.events {
		if e.Kind == runbook.EventMessage {
			out = append(out, e.Message)
		}
	}

	return out
}

// Stdout returns the lines the step wrote to its standard output, with long
// lines that were recorded in pieces joined again.
func (r *StepRun) Stdout() []string {
	return r.output("stdout")
}

// Stderr returns the lines the step wrote to its standard error, as Stdout
// does.
func (r *StepRun) Stderr() []string {
	return r.output("stderr")
}

func (r *StepRun) output(stream string) []string {
	var out []string

	for _, e := range r.events {
		if e.Kind != runbook.EventOutput || e.Stream != stream {
			continue
		}

		if e.Continued && len(out) > 0 {
			out[len(out)-1] += e.Line

			continue
		}

		out = append(out, e.Line)
	}

	return out
}

// tree is a group of the plan RunStep builds, or a step in it.
type tree struct {
	name     string
	children []*tree
	// A step: the step under test, or one that records given values.
	step   *runbook.StepNode
	values []given
}

// stepPlan builds a plan with the given steps first and the step under test
// last, each at its path, and returns it with the step's path.
func stepPlan(step runbook.StepNode, cfg stepConfig) (*runbook.Plan, string, error) {
	probe, err := runbook.NewPlan("probe", step)
	if err != nil {
		return nil, "", err
	}

	path := cfg.parent + "/" + probe.Spec().Children[0].Name
	parts := strings.Split(path, "/")
	root := &tree{name: parts[0]}

	for _, g := range cfg.given {
		gp := strings.Split(g.path, "/")
		if gp[0] != root.name || len(gp) < 2 {
			return nil, "", fmt.Errorf("given path %q is not in the plan %q", g.path, root.name)
		}

		leaf, err := root.place(gp[1:])
		if err != nil {
			return nil, "", fmt.Errorf("given path %q: %w", g.path, err)
		}

		leaf.values = append(leaf.values, g)
	}

	// The step under test goes last at every level, so every given step
	// runs before it.
	leaf, err := root.place(parts[1:])
	if err != nil {
		return nil, "", fmt.Errorf("step path %q: %w", path, err)
	}

	if leaf.values != nil {
		return nil, "", fmt.Errorf("given path %q is the step under test", path)
	}

	leaf.step = &step

	plan, err := runbook.NewPlan(root.name, root.elements()...)
	if err != nil {
		return nil, "", err
	}

	return plan, path, nil
}

// place finds or makes the node at the path below t, moving each node on the
// way to the end of its group. It refuses a path that runs through a step or
// stops at a group.
func (t *tree) place(path []string) (*tree, error) {
	if t.step != nil || t.values != nil {
		return nil, fmt.Errorf("%q is a step, not a group", t.name)
	}

	i := slices.IndexFunc(t.children, func(c *tree) bool { return c.name == path[0] })

	var c *tree

	if i < 0 {
		c = &tree{name: path[0]}
	} else {
		c = t.children[i]
		t.children = slices.Delete(t.children, i, i+1)
	}

	t.children = append(t.children, c)

	if len(path) == 1 {
		if len(c.children) > 0 {
			return nil, fmt.Errorf("%q is a group, not a step", c.name)
		}

		return c, nil
	}

	return c.place(path[1:])
}

func (t *tree) elements() []runbook.Element {
	out := make([]runbook.Element, 0, len(t.children))

	for _, c := range t.children {
		switch {
		case c.step != nil:
			out = append(out, *c.step)
		case c.values != nil:
			out = append(out, runbook.Effect(c.name, record(c.values)))
		default:
			out = append(out, runbook.Group(c.name, c.elements()...))
		}
	}

	return out
}

// record is a step that records the given values.
func record(values []given) runbook.StepFunc {
	return func(_ context.Context, sc *runbook.StepContext) error {
		errs := make([]error, 0, len(values))

		for _, v := range values {
			errs = append(errs, sc.Record(v.key, v.value))
		}

		return errors.Join(errs...)
	}
}
