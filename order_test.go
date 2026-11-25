package pipeline_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/ruffel/pipeline"
	"github.com/ruffel/pipeline/pipelinetest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// datacenter is what the tests of OrderFrom build their plans from: a step
// that records an order, and a group of nodes that runs in it.
//
//	p
//	├── decide (observe)
//	└── dc1 (in the order p/decide records as "dc1")
//	    ├── n1 (effect, undo)
//	    ├── n2 (effect, undo)
//	    └── n3 (effect, undo)
//
// decide runs on every attempt that reaches it, and records what order
// returns then, or nothing if that is nil. A node notes its name when it
// runs, and "undo" and its name when it is undone, and fails either while
// failing or undoFailing names it.
type datacenter struct {
	order       func() any
	failing     map[string]bool
	undoFailing map[string]bool

	mu  sync.Mutex
	ran []string
}

// names is an order that stays the same.
func names(order ...string) func() any {
	return func() any { return order }
}

func (d *datacenter) note(what string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.ran = append(d.ran, what)
}

// took returns what the nodes have noted since it was last asked.
func (d *datacenter) took() []string {
	d.mu.Lock()
	defer d.mu.Unlock()

	out := d.ran
	d.ran = nil

	return out
}

func (d *datacenter) decide() pipeline.StepNode {
	return pipeline.Observe("decide", pipeline.StepFunc(func(_ context.Context, s *pipeline.StepContext) error {
		if order := d.order(); order != nil {
			return s.Record("dc1", order)
		}

		return nil
	}))
}

// dc is the group, taking its order from what the step at from records.
func (d *datacenter) dc(from string, nodes ...string) pipeline.GroupNode {
	children := make([]pipeline.Element, 0, len(nodes))
	for _, name := range nodes {
		children = append(children, pipeline.Effect(name, node{d: d, name: name}))
	}

	return pipeline.Group("dc1", children...).OrderFrom(from, "dc1")
}

// plan is the tree above.
func (d *datacenter) plan(t *testing.T) *pipeline.Plan {
	t.Helper()

	return mustPlan(t, d.decide(), d.dc("p/decide", "n1", "n2", "n3"))
}

type node struct {
	d    *datacenter
	name string
}

func (n node) Run(context.Context, *pipeline.StepContext) error {
	n.d.note(n.name)

	if n.d.failing[n.name] {
		return errors.New(n.name + " failed")
	}

	return nil
}

func (n node) Undo(context.Context, *pipeline.StepContext) error {
	n.d.note("undo " + n.name)

	if n.d.undoFailing[n.name] {
		return errors.New("undo " + n.name + " failed")
	}

	return nil
}

// orderOf returns the order that the group at path was started with in
// events.
func orderOf(t *testing.T, events []pipeline.Event, path string) []string {
	t.Helper()

	for _, e := range kinds(events, pipeline.EventGroupStarted) {
		if e.Path == path {
			return e.Order
		}
	}

	require.Fail(t, "the group did not start", path)

	return nil
}

func stateOf(t *testing.T, j *pipeline.Journal) *pipeline.State {
	t.Helper()

	state, err := j.State()
	require.NoError(t, err)

	return state
}

// The group's children are the plan's, and run in the order the step
// recorded. The group's start says which that was, and so does the state.
func TestOrderFrom_RunsTheChildrenInTheRecordedOrder(t *testing.T) {
	t.Parallel()

	d := &datacenter{order: names("n3", "n1", "n2")}
	plan, j := d.plan(t), pipeline.MemoryJournal()

	out := resumeOn(t, j, plan)

	require.NoError(t, out.err)
	assert.Equal(t, []string{"n3", "n1", "n2"}, d.took())
	assert.Equal(t, []string{"n3", "n1", "n2"}, orderOf(t, out.events, "p/dc1"))

	state := stateOf(t, j)
	require.Equal(t, []string{"n3", "n1", "n2"}, state.Order("p/dc1"))
	assert.Nil(t, state.Order("p"), "a group that takes no order from a step has none")
	assert.Nil(t, state.Order("p/nowhere"))

	// The order is the state's own: a caller can't change it.
	state.Order("p/dc1")[0] = "n9"
	assert.Equal(t, []string{"n3", "n1", "n2"}, state.Order("p/dc1"))

	// The plan is as it was built: the order is how it ran.
	dc := state.Plan.Children[1]
	assert.Equal(t, &pipeline.OrderSource{Path: "p/decide", Key: "dc1"}, dc.OrderFrom)
	assert.Equal(t, []string{"n1", "n2", "n3"}, []string{dc.Children[0].Name, dc.Children[1].Name, dc.Children[2].Name})
}

// The order is settled when the group first starts. A later attempt resumes
// in it, whatever the step has recorded since.
func TestOrderFrom_TheOrderIsSettledWhenTheGroupStarts(t *testing.T) {
	t.Parallel()

	first, second := []string{"n3", "n1", "n2"}, []string{"n2", "n1", "n3"}

	t.Run("a resume keeps it", func(t *testing.T) {
		t.Parallel()

		order := first
		d := &datacenter{order: func() any { return order }, failing: map[string]bool{"n1": true}}
		plan, j := d.plan(t), pipeline.MemoryJournal()

		out := resumeOn(t, j, plan)
		require.ErrorContains(t, out.err, "n1 failed")
		assert.Equal(t, []string{"n3", "n1"}, d.took())

		order, d.failing = second, nil
		out = resumeOn(t, j, plan)

		require.NoError(t, out.err)
		assert.Equal(t, []string{"n1", "n2"}, d.took(), "what was left, in the order the group started with")
		assert.Equal(t, first, orderOf(t, out.events, "p/dc1"), "every start of the group says the order")

		state := stateOf(t, j)
		assert.Equal(t, first, state.Order("p/dc1"))

		raw, ok := state.Value("p/decide", "dc1")
		require.True(t, ok)
		assert.JSONEq(t, `["n2","n1","n3"]`, string(raw), "the step did record another, and it changed nothing")
	})

	// The order is recorded with the group's start, in one write: a crash
	// on either side of it leaves the order settled, or not yet read.
	for name, tc := range map[string]struct {
		stored bool
		want   []string
	}{
		"a crash once the group has started keeps it":  {stored: true, want: first},
		"a crash before the group starts settles none": {stored: false, want: second},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			order := first
			d := &datacenter{order: func() any { return order }}
			plan := d.plan(t)

			var armed, crashed bool

			j := hooked(pipeline.MemoryJournal(), func(e pipeline.Event) error {
				switch {
				case !armed:
					return nil
				case crashed:
					return pipelinetest.ErrCrashed
				case e.Kind == pipeline.EventGroupStarted && e.Path == "p/dc1":
					crashed = true

					if !tc.stored {
						return pipelinetest.ErrCrashed
					}
				}

				return nil
			})

			armed = true
			out := resumeOn(t, j, plan)
			require.ErrorIs(t, out.err, pipelinetest.ErrCrashed)
			assert.Empty(t, d.took(), "no node had started")

			armed, order = false, second
			out = resumeOn(t, j, plan)

			require.NoError(t, out.err)
			assert.Equal(t, tc.want, d.took())
			assert.Equal(t, tc.want, stateOf(t, j).Order("p/dc1"))
		})
	}
}

// With no order it can use, the group fails without starting: none of its
// children runs, and its own failure handler has nothing to handle. The
// group around it has failed like any other. Once the step records an order
// that names every child once, a resume runs the group.
func TestOrderFrom_AGroupWithNoUsableOrderFailsWithoutStarting(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		order any
		want  string
	}{
		"nothing recorded":       {order: nil, want: `pipeline: p/region/decide has recorded no "dc1" for the group's order`},
		"not a list":             {order: "n1", want: `pipeline: what p/region/decide recorded as "dc1" is not a list of names`},
		"a list of numbers":      {order: []int{1, 2, 3}, want: `is not a list of names`},
		"a name that is nobody":  {order: []string{"n1", "n2", "n4"}, want: `names "n4", which is not one of the group's children`},
		"a name twice":           {order: []string{"n1", "n2", "n2"}, want: `the order p/region/decide recorded as "dc1" names "n2" twice`},
		"a child left out":       {order: []string{"n3", "n1"}, want: `leaves out n2`},
		"every child left out":   {order: []string{}, want: `leaves out n1, n2, n3`},
		"a child and a stranger": {order: []string{"n1", "n2", "n3", "n4"}, want: `names "n4", which is not one of the group's children`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var c calls

			order := tc.order
			d := &datacenter{order: func() any { return order }}
			plan, j := mustPlan(t, pipeline.Group("region",
				d.decide(),
				d.dc("p/region/decide", "n1", "n2", "n3").OnFailure(pipeline.Effect("own", counted(&c, "own", nil))),
			).OnFailure(pipeline.Effect("around", counted(&c, "around", nil)))), pipeline.MemoryJournal()

			out := resumeOn(t, j, plan)

			require.ErrorContains(t, out.err, tc.want)
			assert.Equal(t, pipeline.OutcomeFailed, out.res.Outcome)

			var failed *pipeline.StepError

			require.ErrorAs(t, out.err, &failed)
			assert.Equal(t, "p/region/dc1", failed.Path, "the error names the group")

			assert.Empty(t, d.took(), "no child ran")
			assert.Zero(t, c.get("own"), "the group did nothing for its handler to handle")
			assert.Equal(t, 1, c.get("around"), "the group around it failed, as with any failed child")

			steps := trace(out.events)
			assert.NotContains(t, steps, "group.started p/region/dc1")
			assert.Contains(t, steps, "group.finished p/region/dc1 failed")

			state := stateOf(t, j)
			assert.Equal(t, pipeline.StatusFailed, state.Element("p/region/dc1").Status)
			assert.Equal(t, pipeline.StatusNotReached, state.Element("p/region/dc1/n1").Status)
			assert.Nil(t, state.Order("p/region/dc1"), "nothing was settled")

			order = []string{"n2", "n1", "n3"}
			out = resumeOn(t, j, plan)

			require.NoError(t, out.err)
			assert.Equal(t, []string{"n2", "n1", "n3"}, d.took())
		})
	}
}

// The order lasts while anything in the group stands. A group that goes back
// to never started has forgotten it, and reads it again when it next starts.
func TestOrderFrom_AGroupBackAtNeverStartedReadsItsOrderAgain(t *testing.T) {
	t.Parallel()

	first, second := []string{"n3", "n1", "n2"}, []string{"n2", "n1", "n3"}
	restore := pipeline.Effect("restore", ok())

	for name, tc := range map[string]struct {
		plan func(d *datacenter) []pipeline.Element
		// fails is whether the first attempt fails at n1, and between is what
		// comes between the two attempts.
		fails   bool
		dc      string
		between func(t *testing.T, d *datacenter, j *pipeline.Journal, plan *pipeline.Plan)
		opts    []pipeline.Option
	}{
		"its own recovery step reset it": {
			plan: func(d *datacenter) []pipeline.Element {
				return []pipeline.Element{d.decide(), d.dc("p/decide", "n1", "n2", "n3").RecoverOnFailure(restore)}
			},
			fails: true, dc: "p/dc1",
		},
		"the recovery step of a group around it reset it": {
			plan: func(d *datacenter) []pipeline.Element {
				return []pipeline.Element{
					pipeline.Group("region", d.decide(), d.dc("p/region/decide", "n1", "n2", "n3")).RecoverOnFailure(restore),
				}
			},
			fails: true, dc: "p/region/dc1",
		},
		"a forced attempt starts over": {
			plan: func(d *datacenter) []pipeline.Element {
				return []pipeline.Element{d.decide(), d.dc("p/decide", "n1", "n2", "n3")}
			},
			dc: "p/dc1", opts: []pipeline.Option{pipeline.Force()},
		},
		"a rollback undid everything in it": {
			plan: func(d *datacenter) []pipeline.Element {
				return []pipeline.Element{d.decide(), d.dc("p/decide", "n1", "n2", "n3")}
			},
			fails: true, dc: "p/dc1",
			between: func(t *testing.T, d *datacenter, j *pipeline.Journal, plan *pipeline.Plan) {
				t.Helper()

				require.NoError(t, resumeRollback(t, j, plan).err)
				assert.Equal(t, []string{"undo n1", "undo n3"}, d.took())
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			order := first
			d := &datacenter{order: func() any { return order }}

			if tc.fails {
				d.failing = map[string]bool{"n1": true}
			}

			plan, j := mustPlan(t, tc.plan(d)...), pipeline.MemoryJournal()

			out := resumeOn(t, j, plan)
			require.Equal(t, tc.fails, out.err != nil)
			assert.Equal(t, first, orderOf(t, out.events, tc.dc))

			if tc.fails {
				assert.Equal(t, []string{"n3", "n1"}, d.took())
			} else {
				assert.Equal(t, first, d.took())
			}

			if tc.between != nil {
				tc.between(t, d, j, plan)
			}

			if len(tc.opts) == 0 {
				assert.Nil(t, stateOf(t, j).Order(tc.dc), "the group is back at never started")
			}

			order, d.failing = second, nil
			out = resumeOn(t, j, plan, tc.opts...)

			require.NoError(t, out.err)
			assert.Equal(t, second, d.took())
			assert.Equal(t, second, stateOf(t, j).Order(tc.dc))
		})
	}
}

// What was done before a forced attempt no longer counts, even if the forced
// attempt fails before it gets as far. That goes for a settled order too:
// the attempt after it finds the group never started, and reads its order.
func TestOrderFrom_AForcedAttemptThatFailsEarlyHasStillForgottenTheOrder(t *testing.T) {
	t.Parallel()

	fails := false
	order := []string{"n3", "n1", "n2"}
	d := &datacenter{order: func() any { return order }}
	plan := mustPlan(t,
		pipeline.Effect("prepare", pipeline.StepFunc(func(context.Context, *pipeline.StepContext) error {
			if fails {
				return errors.New("prepare failed")
			}

			return nil
		})),
		d.decide(),
		d.dc("p/decide", "n1", "n2", "n3"),
	)
	j := pipeline.MemoryJournal()

	require.NoError(t, resumeOn(t, j, plan).err)
	assert.Equal(t, []string{"n3", "n1", "n2"}, d.took())

	fails = true

	require.ErrorContains(t, resumeOn(t, j, plan, pipeline.Force()).err, "prepare failed")
	assert.Empty(t, d.took())
	assert.Nil(t, stateOf(t, j).Order("p/dc1"))

	fails, order = false, []string{"n2", "n1", "n3"}

	require.NoError(t, resumeOn(t, j, plan).err)
	assert.Equal(t, []string{"n2", "n1", "n3"}, d.took())
}

// A rollback undoes the group's steps in the reverse of the order they ran
// in, not of the plan's. While any of them still stands, the order does too.
func TestOrderFrom_ARollbackGoesBackTheWayTheGroupRan(t *testing.T) {
	t.Parallel()

	d := &datacenter{order: names("n3", "n1", "n2")}
	plan, j := d.plan(t), pipeline.MemoryJournal()

	require.NoError(t, resumeOn(t, j, plan).err)
	assert.Equal(t, []string{"n3", "n1", "n2"}, d.took())

	d.undoFailing = map[string]bool{"n1": true}
	back := resumeRollback(t, j, plan)

	require.ErrorContains(t, back.err, "undo n1 failed")
	assert.Equal(t, []string{"undo n2", "undo n1"}, d.took())
	assert.Equal(t, []string{"n3", "n1", "n2"}, stateOf(t, j).Order("p/dc1"), "n1 and n3 still stand")

	d.undoFailing = nil
	back = resumeRollback(t, j, plan)

	require.NoError(t, back.err)
	assert.Equal(t, []string{"undo n1", "undo n3"}, d.took(), "it carries on in the same order")
	assert.Nil(t, stateOf(t, j).Order("p/dc1"))
}

// A step that ends the group early ends it in the order it is running in.
func TestOrderFrom_ASkipEndsTheGroupInItsOrder(t *testing.T) {
	t.Parallel()

	skips := pipeline.StepFunc(func(context.Context, *pipeline.StepContext) error { return pipeline.ErrSkipGroup })
	d := &datacenter{order: names("c", "b", "a")}
	plan := mustPlan(t,
		d.decide(),
		pipeline.Group("dc1",
			pipeline.Effect("a", ok()), pipeline.Effect("b", ok()), pipeline.Effect("c", skips),
		).OrderFrom("p/decide", "dc1"),
	)

	out := resumeOn(t, pipeline.MemoryJournal(), plan)

	require.NoError(t, out.err)

	steps := trace(out.events)
	require.Greater(t, len(steps), 3)
	assert.Equal(t, []string{
		"step.finished p/dc1/b skipped/group-skipped",
		"step.finished p/dc1/a skipped/group-skipped",
		"group.finished p/dc1 passed",
	}, steps[len(steps)-3:])
}

// The option is a setting, not part of the plan's shape, so a later build
// can add it to a group that is part-way through. The group settles its
// order when it next starts, and what is left runs in it.
func TestOrderFrom_AddedToAGroupThatIsPartWayThrough(t *testing.T) {
	t.Parallel()

	d := &datacenter{order: names("n3", "n2", "n1"), failing: map[string]bool{"n2": true}}

	plain := pipeline.Group("dc1",
		pipeline.Effect("n1", node{d: d, name: "n1"}),
		pipeline.Effect("n2", node{d: d, name: "n2"}),
		pipeline.Effect("n3", node{d: d, name: "n3"}),
	)
	before := mustPlan(t, d.decide(), plain)
	after := mustPlan(t, d.decide(), plain.OrderFrom("p/decide", "dc1"))
	j := pipeline.MemoryJournal()

	require.Equal(t, before.Fingerprint(), after.Fingerprint())

	out := resumeOn(t, j, before)
	require.ErrorContains(t, out.err, "n2 failed")
	assert.Equal(t, []string{"n1", "n2"}, d.took(), "the plan's order")
	assert.Nil(t, stateOf(t, j).Order("p/dc1"))

	d.failing = nil
	out = resumeOn(t, j, after)

	require.NoError(t, out.err)
	assert.Equal(t, []string{"n3", "n2"}, d.took())
	assert.Equal(t, []string{"n3", "n2", "n1"}, stateOf(t, j).Order("p/dc1"))
}

// Under WithPlanChange, the children of a group that has settled its order
// must still be those the order names. A change anywhere else is taken as
// usual, and a forced attempt settles a new order for the children it finds.
func TestOrderFrom_AChangedPlanKeepsTheChildrenTheOrderNames(t *testing.T) {
	t.Parallel()

	start := func(t *testing.T) (*datacenter, *pipeline.Journal) {
		t.Helper()

		d := &datacenter{order: names("n3", "n1", "n2"), failing: map[string]bool{"n1": true}}
		j := pipeline.MemoryJournal()

		require.ErrorContains(t, resumeOn(t, j, d.plan(t)).err, "n1 failed")
		assert.Equal(t, []string{"n3", "n1"}, d.took())

		d.failing = nil

		return d, j
	}

	for name, nodes := range map[string][]string{
		"a child added":   {"n1", "n2", "n3", "n4"},
		"a child removed": {"n1", "n2"},
		"a child renamed": {"n1", "n2", "n9"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			d, j := start(t)
			changed := mustPlan(t, d.decide(), d.dc("p/decide", nodes...))

			out := resumeOn(t, j, changed, matchByPath())

			require.ErrorIs(t, out.err, pipeline.ErrPlanChanged)
			require.ErrorContains(t, out.err, "the children of p/dc1 are no longer those its order names")
			assert.Nil(t, out.res)
			assert.Empty(t, out.events, "refused before anything is recorded")

			// A rollback is refused too: for this, or because a child that
			// ran has gone, and its Undo with it.
			back := resumeRollback(t, j, changed, matchByPath())

			require.ErrorIs(t, back.err, pipeline.ErrPlanChanged)
			assert.Empty(t, back.events)
			assert.Empty(t, d.took())
		})
	}

	t.Run("a change elsewhere", func(t *testing.T) {
		t.Parallel()

		d, j := start(t)
		changed := mustPlan(t, d.decide(), d.dc("p/decide", "n1", "n2", "n3"), pipeline.Effect("after", ok()))

		out := resumeOn(t, j, changed, matchByPath())

		require.NoError(t, out.err)
		assert.Equal(t, []string{"n1", "n2"}, d.took())
	})

	t.Run("a forced attempt", func(t *testing.T) {
		t.Parallel()

		d, j := start(t)
		d.order = names("n4", "n3", "n2", "n1")
		changed := mustPlan(t, d.decide(), d.dc("p/decide", "n1", "n2", "n3", "n4"))

		out := resumeOn(t, j, changed, matchByPath(), pipeline.Force())

		require.NoError(t, out.err)
		assert.Equal(t, []string{"n4", "n3", "n2", "n1"}, d.took())
	})
}

// The journal's order outlives the setting. A later build that takes the
// option off a group does not unsettle it: the group carries on, and is
// rolled back, in the order it settled, and its children must still be
// those the order names. Nor may the group now run them at once.
func TestOrderFrom_ASettledOrderStandsWithoutTheOption(t *testing.T) {
	t.Parallel()

	start := func(t *testing.T) (*datacenter, *pipeline.Journal, func(nodes ...string) pipeline.GroupNode) {
		t.Helper()

		d := &datacenter{order: names("n3", "n1", "n2"), failing: map[string]bool{"n1": true}}
		j := pipeline.MemoryJournal()

		require.ErrorContains(t, resumeOn(t, j, d.plan(t)).err, "n1 failed")
		assert.Equal(t, []string{"n3", "n1"}, d.took())

		d.failing = nil

		// The same group, as a build without the option has it.
		plain := func(nodes ...string) pipeline.GroupNode {
			children := make([]pipeline.Element, 0, len(nodes))
			for _, name := range nodes {
				children = append(children, pipeline.Effect(name, node{d: d, name: name}))
			}

			return pipeline.Group("dc1", children...)
		}

		return d, j, plain
	}

	t.Run("a resume and a rollback keep to it", func(t *testing.T) {
		t.Parallel()

		d, j, plain := start(t)
		plan := mustPlan(t, d.decide(), plain("n1", "n2", "n3"))

		require.Equal(t, d.plan(t).Fingerprint(), plan.Fingerprint(), "nothing refuses the build")

		out := resumeOn(t, j, plan)

		require.NoError(t, out.err)
		assert.Equal(t, []string{"n1", "n2"}, d.took(), "not n2 before n1, as the plan lists them")
		assert.Equal(t, []string{"n3", "n1", "n2"}, orderOf(t, out.events, "p/dc1"))
		assert.Equal(t, []string{"n3", "n1", "n2"}, stateOf(t, j).Order("p/dc1"))

		require.NoError(t, resumeRollback(t, j, plan).err)
		assert.Equal(t, []string{"undo n2", "undo n1", "undo n3"}, d.took())

		// Back at never started, the group is what the plan now says.
		require.NoError(t, resumeOn(t, j, plan).err)
		assert.Equal(t, []string{"n1", "n2", "n3"}, d.took())
		assert.Nil(t, stateOf(t, j).Order("p/dc1"))
	})

	// A forced attempt unsettles the order. With the option gone, the group
	// then runs in the plan's order, and that is the way back too.
	t.Run("a forced attempt without it runs in the plan's order", func(t *testing.T) {
		t.Parallel()

		d, j, plain := start(t)
		plan := mustPlan(t, d.decide(), plain("n1", "n2", "n3"))

		out := resumeOn(t, j, plan, pipeline.Force())

		require.NoError(t, out.err)
		assert.Equal(t, []string{"n1", "n2", "n3"}, d.took())
		assert.Nil(t, orderOf(t, out.events, "p/dc1"))

		require.NoError(t, resumeRollback(t, j, plan).err)
		assert.Equal(t, []string{"undo n3", "undo n2", "undo n1"}, d.took())
	})

	t.Run("its children must still be those it names", func(t *testing.T) {
		t.Parallel()

		d, j, plain := start(t)

		out := resumeOn(t, j, mustPlan(t, d.decide(), plain("n1", "n2", "n3", "n4")), matchByPath())

		require.ErrorIs(t, out.err, pipeline.ErrPlanChanged)
		require.ErrorContains(t, out.err, "the children of p/dc1 are no longer those its order names")
		assert.Empty(t, out.events)
	})

	t.Run("it may not become parallel", func(t *testing.T) {
		t.Parallel()

		d, j, plain := start(t)

		out := resumeOn(t, j, mustPlan(t, d.decide(), plain("n1", "n2", "n3").Parallel(0)))

		require.ErrorIs(t, out.err, pipeline.ErrPlanChanged)
		require.ErrorContains(t, out.err, "p/dc1 has settled the order of its children, and may not now be parallel")
		assert.Empty(t, out.events)
		assert.Empty(t, d.took())
	})
}

// A forced attempt unsettles the order, and forgets nothing that a rollback
// needs: the steps that ran are still to undo, the way they ran.
func TestOrderFrom_ARollbackAfterAForcedAttemptStillGoesBackTheWayItRan(t *testing.T) {
	t.Parallel()

	fails := false
	d := &datacenter{order: names("n3", "n1", "n2")}
	plan := mustPlan(t,
		pipeline.Effect("prepare", pipeline.StepFunc(func(context.Context, *pipeline.StepContext) error {
			if fails {
				return errors.New("prepare failed")
			}

			return nil
		})),
		d.decide(),
		d.dc("p/decide", "n1", "n2", "n3"),
	)
	j := pipeline.MemoryJournal()

	require.NoError(t, resumeOn(t, j, plan).err)
	assert.Equal(t, []string{"n3", "n1", "n2"}, d.took())

	fails = true

	require.ErrorContains(t, resumeOn(t, j, plan, pipeline.Force()).err, "prepare failed")
	assert.Nil(t, stateOf(t, j).Order("p/dc1"), "the forced attempt never reached the group")

	require.NoError(t, resumeRollback(t, j, plan).err)
	assert.Equal(t, []string{"undo n2", "undo n1", "undo n3"}, d.took(), "not the reverse of the plan's order")
}

// A group with no children has nothing to run in any order, so it settles
// none. Nothing in it stands, and an order lasts only while something does:
// when a later build gives it children, it reads its order then.
func TestOrderFrom_AGroupWithNoChildrenSettlesNoOrder(t *testing.T) {
	t.Parallel()

	fails := true
	order := []string{}
	d := &datacenter{order: func() any { return order }}
	after := pipeline.Effect("after", counted(&calls{}, "after", &fails))
	j := pipeline.MemoryJournal()

	out := resumeOn(t, j, mustPlan(t, d.decide(), d.dc("p/decide"), after))

	require.ErrorContains(t, out.err, "after failed")
	assert.Contains(t, trace(out.events), "group.finished p/dc1 passed", "an empty list is the order of no children")
	assert.Nil(t, orderOf(t, out.events, "p/dc1"))
	assert.Nil(t, stateOf(t, j).Order("p/dc1"))

	fails, order = false, []string{"n2", "n1"}
	out = resumeOn(t, j, mustPlan(t, d.decide(), d.dc("p/decide", "n1", "n2"), after), matchByPath())

	require.NoError(t, out.err)
	assert.Equal(t, []string{"n2", "n1"}, d.took())
	assert.Equal(t, []string{"n2", "n1"}, stateOf(t, j).Order("p/dc1"))
}

// An attempt whose plan has an OrderFrom group is written as version 3,
// which a program built before the option refuses. So is one that carries on
// in an order the journal has settled, whatever its own plan asks for. Any
// other attempt is written as the oldest version that will do.
func TestOrderFrom_AnAttemptThatNeedsItIsWrittenAsVersion3(t *testing.T) {
	t.Parallel()

	d := &datacenter{order: names("n3", "n1", "n2")}
	plan := func(ordered, drain bool) *pipeline.Plan {
		dc := pipeline.Group("dc1",
			bounded(pipeline.Effect("n1", node{d: d, name: "n1"})),
			bounded(pipeline.Effect("n2", node{d: d, name: "n2"})),
			bounded(pipeline.Effect("n3", node{d: d, name: "n3"})),
		)

		if ordered {
			dc = dc.OrderFrom("p/decide", "dc1")
		}

		if drain {
			dc = dc.DrainOnStop()
		}

		return mustPlan(t, d.decide(), dc)
	}

	version := func(events []pipeline.Event) int {
		require.NotEmpty(t, events)

		for _, e := range events {
			require.Equal(t, events[0].V, e.V, "an attempt is written as one version")
		}

		return events[0].V
	}

	assert.Equal(t, 1, version(resumeOn(t, pipeline.MemoryJournal(), plan(false, false)).events))
	assert.Equal(t, 2, version(resumeOn(t, pipeline.MemoryJournal(), plan(false, true)).events))
	assert.Equal(t, 3, version(resumeOn(t, pipeline.MemoryJournal(), plan(true, false)).events))
	assert.Equal(t, 3, version(resumeOn(t, pipeline.MemoryJournal(), plan(true, true)).events), "the newest any group needs")
	assert.Equal(t, 3, pipeline.SchemaVersion)

	// One journal holds attempts of several versions.
	j := pipeline.MemoryJournal()
	d.failing = map[string]bool{"n1": true}

	assert.Equal(t, 1, version(resumeOn(t, j, plan(false, false)).events), "the plan's own order, and no order settled")
	assert.Equal(t, 3, version(resumeOn(t, j, plan(true, false)).events))
	assert.Equal(t, 3, version(resumeOn(t, j, plan(false, false)).events), "it carries on in the settled order, and says so")
	assert.Equal(t, 3, version(resumeRollback(t, j, plan(true, false)).events), "a rollback's attempt too")

	assert.Len(t, stateOf(t, j).Attempts, 4, "this reader reads them all")
}

// A viewer in another process reads the order from a file journal.
func TestOrderFrom_InspectAndFollow(t *testing.T) {
	t.Parallel()

	d := &datacenter{order: names("n3", "n1", "n2")}
	path := t.TempDir() + "/upgrade.jsonl"

	j, err := pipeline.OpenJournal(path)
	require.NoError(t, err)

	_, err = pipeline.Run(t.Context(), d.plan(t), j)
	require.NoError(t, err)
	require.NoError(t, j.Close())

	state, err := pipeline.Inspect(path)
	require.NoError(t, err)
	assert.Equal(t, []string{"n3", "n1", "n2"}, state.Order("p/dc1"))

	var seen []string

	watch := pipeline.ObserverFunc(func(_ context.Context, e pipeline.Event) {
		if e.Kind == pipeline.EventGroupStarted {
			seen = e.Order
		}
	})

	followed, err := pipeline.Follow(t.Context(), path, pipeline.FollowOptions{}, watch)
	require.NoError(t, err)
	assert.Equal(t, []string{"n3", "n1", "n2"}, seen)
	assert.Equal(t, []string{"n3", "n1", "n2"}, followed.Order("p/dc1"))
}

// A group can take its order only from a step that has run by the time it
// starts, and only if it runs its children one at a time.
func TestNewPlan_OrderFrom(t *testing.T) {
	t.Parallel()

	decide := pipeline.Observe("decide", ok())
	dc := pipeline.Group("dc1", pipeline.Effect("n1", ok()), pipeline.Effect("n2", ok()))

	const (
		notAStep  = ", which is not a step in the plan"
		notBefore = ", which does not come before it"
		notYet    = ", which may not have run when it starts: "
	)

	for name, tc := range map[string]struct {
		elems []pipeline.Element
		want  string
	}{
		"no step named": {
			elems: []pipeline.Element{decide, dc.OrderFrom("", "dc1")},
			want:  "p/dc1: OrderFrom needs the path of a step, and the key it records the order under",
		},
		"no key named": {
			elems: []pipeline.Element{decide, dc.OrderFrom("p/decide", "")},
			want:  "p/dc1: OrderFrom needs the path of a step, and the key it records the order under",
		},
		"parallel": {
			elems: []pipeline.Element{decide, dc.Parallel(2).OrderFrom("p/decide", "dc1")},
			want:  "p/dc1: a group that takes its order from a step may not be parallel",
		},
		"a path the plan does not have": {
			elems: []pipeline.Element{decide, dc.OrderFrom("p/elsewhere", "dc1")},
			want:  "p/dc1: its order is to come from p/elsewhere" + notAStep,
		},
		"a step after it": {
			elems: []pipeline.Element{dc.OrderFrom("p/decide", "dc1"), decide},
			want:  "p/dc1: its order is to come from p/decide" + notBefore,
		},
		"a step inside it": {
			elems: []pipeline.Element{pipeline.Group("dc1", decide, pipeline.Effect("n1", ok())).OrderFrom("p/dc1/decide", "dc1")},
			want:  "p/dc1: its order is to come from p/dc1/decide" + notBefore,
		},
		"a group": {
			elems: []pipeline.Element{pipeline.Group("prepare", decide), dc.OrderFrom("p/prepare", "dc1")},
			want:  "p/dc1: its order is to come from p/prepare" + notAStep,
		},
		"a gate": {
			elems: []pipeline.Element{pipeline.Gate("go", pipeline.GateOptions{}), dc.OrderFrom("p/go", "dc1")},
			want:  "p/dc1: its order is to come from p/go" + notAStep,
		},
		"the failure handler of a group around it": {
			elems: []pipeline.Element{
				pipeline.Group("region", dc.OrderFrom("p/region/cleanup", "dc1")).OnFailure(pipeline.Effect("cleanup", ok())),
			},
			want: "p/region/dc1: its order is to come from p/region/cleanup" + notBefore,
		},
		// Coming earlier in the plan is not enough: the group that holds
		// both has to run the step's side first, every time.
		"a step alongside it in a parallel group": {
			elems: []pipeline.Element{pipeline.Group("region", decide, dc.OrderFrom("p/region/decide", "dc1")).Parallel(0)},
			want:  "p/region/dc1: its order is to come from p/region/decide" + notYet + "p/region runs its children at once",
		},
		"a step in another branch of a parallel group": {
			elems: []pipeline.Element{pipeline.Group("region",
				pipeline.Group("prepare", decide),
				pipeline.Group("nodes", dc.OrderFrom("p/region/prepare/decide", "dc1")),
			).Parallel(2)},
			want: "p/region/nodes/dc1: its order is to come from p/region/prepare/decide" + notYet + "p/region runs its children at once",
		},
		"a step alongside it in a group whose own order is decided later": {
			elems: []pipeline.Element{
				pipeline.Observe("first", ok()),
				pipeline.Group("region", decide, dc.OrderFrom("p/region/decide", "dc1")).OrderFrom("p/first", "region"),
			},
			want: "p/region/dc1: its order is to come from p/region/decide" + notYet + "p/region takes its own order from a step",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := pipeline.NewPlan("p", tc.elems...)
			require.ErrorContains(t, err, tc.want)
		})
	}

	_, err := pipeline.NewPlanFromGroup(pipeline.Group("p", pipeline.Effect("n1", ok())).OrderFrom("p/decide", "dc1"))
	require.ErrorContains(t, err, "p: the plan's root may not take its order from a step")
	require.NotContains(t, err.Error(), "its order is to come from", "one problem is reported once")

	_, err = pipeline.NewPlanFromGroup(pipeline.Group("p", decide, dc.OrderFrom("p/decide", "dc1")).Parallel(0))
	require.ErrorContains(t, err, "p/dc1: its order is to come from p/decide"+notYet+"p runs its children at once")

	// What matters is the group where the two part ways. A parallel group
	// further out, or one inside the step's side or the group's, changes
	// nothing: the step's side has finished before the group's starts.
	_, err = pipeline.NewPlan("p", pipeline.Group("regions",
		pipeline.Group("region1",
			pipeline.Group("prepare", decide, pipeline.Observe("other", ok())).Parallel(0),
			pipeline.Group("nodes", dc.OrderFrom("p/regions/region1/prepare/decide", "dc1")).Parallel(0),
		),
		pipeline.Group("region2", pipeline.Effect("n1", ok())),
	).Parallel(2))
	require.NoError(t, err)

	// The step may be anywhere before the group, the group may be one that a
	// stop waits for, and one such group may be inside another. The option
	// changes nothing about the plan's shape.
	step := func(name string) pipeline.StepNode { return bounded(pipeline.Effect(name, ok())) }
	build := func(ordered bool) *pipeline.Plan {
		racks := pipeline.Group("racks", step("r1"), step("r2"))
		nodes := pipeline.Group("dc1", step("n1"), racks).DrainOnStop()

		if ordered {
			nodes = pipeline.Group("dc1", step("n1"), racks.OrderFrom("p/prepare/decide", "racks")).
				DrainOnStop().OrderFrom("p/prepare/decide", "dc1")
		}

		return mustPlan(t, pipeline.Group("prepare", decide), nodes)
	}

	assert.Equal(t, build(false).Fingerprint(), build(true).Fingerprint())
	assert.Equal(t, `p
├── prepare
│   └── decide (observe)
└── dc1 (finishes before a stop, in the order p/prepare/decide records as "dc1")
    ├── n1 (effect)
    └── racks (in the order p/prepare/decide records as "racks")
        ├── r1 (effect)
        └── r2 (effect)
`, pipeline.Describe(build(true)))
}
