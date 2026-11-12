package runbook_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ruffel/pipeline/runbook"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// attemptResult is one attempt's result, error, and the events it added.
type attemptResult struct {
	res    *runbook.Result
	err    error
	events []runbook.Event
}

// resumeOn makes one attempt at plan against j.
func resumeOn(t *testing.T, j *runbook.Journal, plan *runbook.Plan, opts ...runbook.Option) attemptResult {
	t.Helper()

	before, err := j.Events()
	require.NoError(t, err)

	res, runErr := runbook.Run(t.Context(), plan, j, opts...)

	after, err := j.Events()
	require.NoError(t, err)

	return attemptResult{res: res, err: runErr, events: after[len(before):]}
}

// calls counts calls by name, safely.
type calls struct {
	mu sync.Mutex
	n  map[string]int
}

func (c *calls) add(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.n == nil {
		c.n = map[string]int{}
	}

	c.n[name]++
}

func (c *calls) get(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.n[name]
}

func (c *calls) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.n = nil
}

// counted is a step that counts its Run calls and fails while failing is set.
func counted(c *calls, name string, failing *bool) runbook.Step {
	return runbook.StepFunc(func(context.Context, *runbook.StepContext) error {
		c.add(name)

		if failing != nil && *failing {
			return errors.New(name + " failed")
		}

		return nil
	})
}

func TestResume_DoesOnlyWhatIsLeft(t *testing.T) {
	t.Parallel()

	c := &calls{}
	failing := true

	plan := mustPlan(t,
		runbook.Effect("a", counted(c, "a", nil)),
		runbook.Effect("b", counted(c, "b", &failing)),
		runbook.Effect("c", counted(c, "c", nil)),
	)
	j := runbook.MemoryJournal()

	first := resumeOn(t, j, plan)
	require.ErrorContains(t, first.err, "p/b: b failed")
	assert.Equal(t, 1, first.res.Attempt)

	failing = false

	second := resumeOn(t, j, plan)
	require.NoError(t, second.err)
	assert.Equal(t, 2, second.res.Attempt)
	assert.Equal(t, runbook.OutcomePassed, second.res.Outcome)
	assert.NotEqual(t, first.res.Run, second.res.Run)

	assert.Equal(t, []string{
		"step.finished p/a skipped/earlier-attempt",
		"step.started p/b", "step.running p/b", "step.finished p/b passed",
		"step.started p/c", "step.running p/c", "step.finished p/c passed",
	}, trace(second.events))
	assert.Equal(t, 1, c.get("a"), "a is not run again")
	assert.Equal(t, 2, c.get("b"))
	assert.Equal(t, 1, c.get("c"))

	state, err := j.State()
	require.NoError(t, err)
	require.Len(t, state.Attempts, 2)
	assert.False(t, state.Unfinished())
	assert.True(t, state.Element("p/a").Done)
}

func TestResume_WholeRunRules(t *testing.T) {
	t.Parallel()

	c := &calls{}
	plan := func(extra ...runbook.Element) *runbook.Plan {
		return mustPlan(t, append([]runbook.Element{
			runbook.Effect("a", counted(c, "a", nil)).Description("first words"),
		}, extra...)...)
	}

	j := runbook.MemoryJournal()
	require.NoError(t, resumeOn(t, j, plan()).err)

	// A passed run is not repeated.
	require.ErrorIs(t, resumeOn(t, j, plan()).err, runbook.ErrAlreadyComplete)

	// A plan of a different shape is refused, forced or not.
	changed := plan(runbook.Effect("new", ok()))
	require.ErrorIs(t, resumeOn(t, j, changed).err, runbook.ErrPlanChanged)
	require.ErrorIs(t, resumeOn(t, j, changed, runbook.Force()).err, runbook.ErrPlanChanged)

	// Force runs it all again, and says so in the journal.
	forced := resumeOn(t, j, plan(), runbook.Force())
	require.NoError(t, forced.err)
	assert.Equal(t, 2, forced.res.Attempt)
	assert.True(t, forced.events[0].Forced)
	assert.Equal(t, 2, c.get("a"))
}

func TestResume_ChangedWordingAndSettingsAreNotAChangedPlan(t *testing.T) {
	t.Parallel()

	failing := true
	c := &calls{}
	j := runbook.MemoryJournal()

	require.Error(t, resumeOn(t, j, mustPlan(t,
		runbook.Group("g", runbook.Effect("a", counted(c, "a", &failing))).Parallel(8),
	)).err)

	failing = false

	// A hotfix build: reworded, with a lower limit and a retry.
	require.NoError(t, resumeOn(t, j, mustPlan(t,
		runbook.Group("g",
			runbook.Effect("a", counted(c, "a", &failing)).Description("reworded").Retry(3, time.Millisecond),
		).Parallel(2).Description("slower"),
	)).err)
}

// state is a fake system: which named things are currently true.
type system struct {
	mu sync.Mutex
	on map[string]bool
}

func (s *system) set(name string, v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.on == nil {
		s.on = map[string]bool{}
	}

	s.on[name] = v
}

func (s *system) is(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.on[name]
}

// setter is a step with a Check: it makes name equal want.
type setter struct {
	sys     *system
	c       *calls
	label   string
	name    string
	want    bool
	failing *bool
}

func (s setter) Check(context.Context, *runbook.StepContext) (bool, error) {
	s.c.add("check " + s.label)

	return s.sys.is(s.name) == s.want, nil
}

func (s setter) Run(context.Context, *runbook.StepContext) error {
	s.c.add("run " + s.label)

	if s.failing != nil && *s.failing {
		return errors.New(s.label + " failed")
	}

	s.sys.set(s.name, s.want)

	return nil
}

// The reason for settled groups: once a node has finished, its stop step's
// Check says "not stopped", because start ran afterwards. Deciding step by
// step would stop and start every finished node again on every resume.
func TestResume_AFinishedGroupIsNotEnteredAgain(t *testing.T) {
	t.Parallel()

	sys, c := &system{}, &calls{}
	failing := true

	node := func(name string, apply *bool) runbook.GroupNode {
		return runbook.Group(name,
			runbook.Ensure("maintenance", setter{sys: sys, c: c, label: name + "/maintenance", name: name + ".maint", want: true}),
			runbook.Effect("stop", setter{sys: sys, c: c, label: name + "/stop", name: name + ".running", want: false}),
			runbook.Effect("apply", setter{sys: sys, c: c, label: name + "/apply", name: name + ".upgraded", want: true, failing: apply}),
			runbook.Effect("start", setter{sys: sys, c: c, label: name + "/start", name: name + ".running", want: true}),
			runbook.Ensure("normal", setter{sys: sys, c: c, label: name + "/normal", name: name + ".maint", want: false}),
		)
	}

	sys.set("node1.running", true)
	sys.set("node2.running", true)

	plan := mustPlan(t,
		runbook.Group("prechecks", runbook.Observe("ring", counted(c, "ring", nil))),
		runbook.Ensure("auto-repair-off", setter{sys: sys, c: c, label: "auto-repair-off", name: "auto-repair", want: false}),
		runbook.Group("nodes", node("node1", nil), node("node2", &failing)),
	)
	j := runbook.MemoryJournal()

	first := resumeOn(t, j, plan)
	require.ErrorContains(t, first.err, "node2/apply failed")
	assert.True(t, sys.is("node1.running"))
	assert.False(t, sys.is("node2.running"), "node2 is stopped, part-way through")

	// Between attempts something turns auto-repair back on.
	sys.set("auto-repair", true)
	c.reset()

	failing = false

	second := resumeOn(t, j, plan)
	require.NoError(t, second.err)

	assert.Equal(t, []string{
		"group.started p/prechecks",
		"step.started p/prechecks/ring", "step.running p/prechecks/ring", "step.finished p/prechecks/ring passed",
		"group.finished p/prechecks passed",
		"step.started p/auto-repair-off", "step.running p/auto-repair-off", "step.finished p/auto-repair-off passed",
		"group.started p/nodes",
		"group.finished p/nodes/node1 skipped/settled",
		"group.started p/nodes/node2",
		"step.started p/nodes/node2/maintenance", "step.finished p/nodes/node2/maintenance skipped/done",
		"step.started p/nodes/node2/stop", "step.finished p/nodes/node2/stop skipped/done",
		"step.started p/nodes/node2/apply", "step.running p/nodes/node2/apply", "step.finished p/nodes/node2/apply passed",
		"step.started p/nodes/node2/start", "step.running p/nodes/node2/start", "step.finished p/nodes/node2/start passed",
		"step.started p/nodes/node2/normal", "step.running p/nodes/node2/normal", "step.finished p/nodes/node2/normal passed",
		"group.finished p/nodes/node2 passed",
		"group.finished p/nodes passed",
	}, trace(second.events))

	for _, step := range []string{"maintenance", "stop", "apply", "start", "normal"} {
		assert.Zero(t, c.get("check node1/"+step), "node1's %s is not even checked", step)
		assert.Zero(t, c.get("run node1/"+step))
	}

	assert.Equal(t, 1, c.get("ring"), "a group of only Observe steps runs on every attempt")
	assert.Equal(t, 1, c.get("run auto-repair-off"), "a top-level Ensure is applied again")
	assert.True(t, sys.is("node1.running"))
	assert.True(t, sys.is("node2.running"))
	assert.True(t, sys.is("node2.upgraded"))
	assert.False(t, sys.is("auto-repair"))
}

func TestResume_VerifyRunsCheckAgainAndTrustDoesNot(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		mode      runbook.ResumeMode
		wantTrace []string
		wantRuns  int
	}{
		"verify finds the drift and applies again": {
			mode: runbook.ResumeVerify,
			wantTrace: []string{
				"step.started p/config", "step.running p/config", "step.finished p/config passed",
				"step.started p/after", "step.running p/after", "step.finished p/after passed",
			},
			wantRuns: 2,
		},
		"trust skips without checking": {
			mode: runbook.ResumeTrust,
			wantTrace: []string{
				"step.finished p/config skipped/earlier-attempt",
				"step.started p/after", "step.running p/after", "step.finished p/after passed",
			},
			wantRuns: 1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			sys, c := &system{}, &calls{}
			failing := true

			plan := mustPlan(t,
				runbook.Effect("config", setter{sys: sys, c: c, label: "config", name: "config", want: true}),
				runbook.Effect("after", counted(c, "after", &failing)),
			)
			j := runbook.MemoryJournal()

			require.Error(t, resumeOn(t, j, plan).err)

			sys.set("config", false) // drift between attempts
			c.reset()

			failing = false

			second := resumeOn(t, j, plan, runbook.WithResume(tc.mode))
			require.NoError(t, second.err)
			assert.Equal(t, tc.wantTrace, trace(second.events))
			assert.Equal(t, tc.wantRuns-1, c.get("run config"))
		})
	}
}

func TestResume_AVerifiedStepThatIsStillDoneIsSkipped(t *testing.T) {
	t.Parallel()

	sys, c := &system{}, &calls{}
	failing := true

	plan := mustPlan(t,
		runbook.Effect("config", setter{sys: sys, c: c, label: "config", name: "config", want: true}),
		runbook.Effect("after", counted(c, "after", &failing)),
	)
	j := runbook.MemoryJournal()

	require.Error(t, resumeOn(t, j, plan).err)

	failing = false

	second := resumeOn(t, j, plan)
	require.NoError(t, second.err)
	assert.Equal(t, []string{"step.started p/config", "step.finished p/config skipped/done"}, trace(second.events)[:2])
	assert.Equal(t, 1, c.get("run config"))

	state, err := j.State()
	require.NoError(t, err)
	assert.True(t, state.Element("p/config").RunCalled, "an earlier attempt's Run still counts, for rollback")
}

func TestResume_ReplaysASkippedStepsSignal(t *testing.T) {
	t.Parallel()

	c := &calls{}
	failing := true

	plan := mustPlan(t,
		runbook.Group("g",
			runbook.Effect("broken", counted(c, "broken", &failing)),
			runbook.Effect("signal", signals(runbook.ErrSkipGroup)),
			runbook.Effect("rest", counted(c, "rest", nil)),
		).ContinueOnError(),
		runbook.Effect("after", counted(c, "after", nil)),
	)
	j := runbook.MemoryJournal()

	require.Error(t, resumeOn(t, j, plan).err)

	failing = false

	second := resumeOn(t, j, plan)
	require.NoError(t, second.err)
	assert.Equal(t, []string{
		"group.started p/g",
		"step.started p/g/broken", "step.running p/g/broken", "step.finished p/g/broken passed",
		"step.finished p/g/signal skipped/earlier-attempt",
		"step.finished p/g/rest skipped/group-skipped",
		"group.finished p/g passed",
		"step.started p/after", "step.running p/after", "step.finished p/after passed",
	}, trace(second.events))
	assert.Zero(t, c.get("rest"), "the signal still ends the group, without running the step that gave it")
}

func TestResume_PriorAndPreviousItems(t *testing.T) {
	t.Parallel()

	type seen struct {
		node1, node2 runbook.ElementState
		items        map[string]runbook.ItemState
		dir          string
	}

	var got []seen

	j := runbook.MemoryJournal()
	busy := true
	plan := mustPlan(t,
		runbook.Ensure("reconcile", runbook.StepFunc(func(_ context.Context, s *runbook.StepContext) error {
			var dir string

			_, err := s.Recorded("p/nodes/node1/work", "dir", &dir)

			got = append(got, seen{node1: s.Prior("p/nodes/node1"), node2: s.Prior("p/nodes/node2"), dir: dir})

			return err
		})),
		runbook.Group("nodes",
			runbook.Group("node1", runbook.Effect("work", runbook.StepFunc(
				func(_ context.Context, s *runbook.StepContext) error { return s.Record("dir", "/backups/1") }))),
			runbook.Group("node2", runbook.Effect("work", runbook.StepFunc(
				func(_ context.Context, s *runbook.StepContext) error {
					got[len(got)-1].items = s.PreviousItems()

					if err := s.Item("disk1", runbook.ItemPassed, ""); err != nil {
						return err
					}

					if busy {
						_ = s.Item("disk2", runbook.ItemFailed, "busy")

						return errors.New("disk2 busy")
					}

					return s.Item("disk2", runbook.ItemPassed, "")
				}))),
		),
	)

	require.Error(t, resumeOn(t, j, plan).err)

	busy = false

	require.NoError(t, resumeOn(t, j, plan).err)
	require.NoError(t, resumeOn(t, j, plan, runbook.Force()).err)
	require.Len(t, got, 3)

	// First attempt: nothing has happened yet.
	assert.Equal(t, runbook.StatusNotReached, got[0].node1.Status)
	assert.Empty(t, got[0].items)
	assert.Empty(t, got[0].dir)

	// Resume: node1 finished, node2 failed, and its items are there to retry from.
	assert.True(t, got[1].node1.Done)
	assert.Equal(t, runbook.StatusFailed, got[1].node2.Status)
	assert.Equal(t, runbook.ItemPassed, got[1].items["disk1"].Status)
	assert.Equal(t, runbook.ItemFailed, got[1].items["disk2"].Status)
	assert.Equal(t, "/backups/1", got[1].dir, "values from earlier attempts are readable")

	// Forced: decided as if nothing had happened, but values stay readable.
	assert.Equal(t, runbook.StatusNotReached, got[2].node1.Status)
	assert.Empty(t, got[2].items)
	assert.Equal(t, "/backups/1", got[2].dir)
}
