package pipeline_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ruffel/pipeline"
	"github.com/ruffel/pipeline/pipelinetest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// attemptResult is one attempt's result, error, and the events it added.
type attemptResult struct {
	res    *pipeline.Result
	err    error
	events []pipeline.Event
}

// resumeOn makes one attempt at plan against j.
func resumeOn(t *testing.T, j *pipeline.Journal, plan *pipeline.Plan, opts ...pipeline.Option) attemptResult {
	t.Helper()

	before, err := j.Events()
	require.NoError(t, err)

	res, runErr := pipeline.Run(t.Context(), plan, j, opts...)

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
func counted(c *calls, name string, failing *bool) pipeline.Step {
	return pipeline.StepFunc(func(context.Context, *pipeline.StepContext) error {
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
		pipeline.Effect("a", counted(c, "a", nil)),
		pipeline.Effect("b", counted(c, "b", &failing)),
		pipeline.Effect("c", counted(c, "c", nil)),
	)
	j := pipeline.MemoryJournal()

	first := resumeOn(t, j, plan)
	require.ErrorContains(t, first.err, "p/b: b failed")
	assert.Equal(t, 1, first.res.Attempt)

	failing = false

	second := resumeOn(t, j, plan)
	require.NoError(t, second.err)
	assert.Equal(t, 2, second.res.Attempt)
	assert.Equal(t, pipeline.OutcomePassed, second.res.Outcome)
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
	plan := func(extra ...pipeline.Element) *pipeline.Plan {
		return mustPlan(t, append([]pipeline.Element{
			pipeline.Effect("a", counted(c, "a", nil)).Description("first words"),
		}, extra...)...)
	}

	j := pipeline.MemoryJournal()
	require.NoError(t, resumeOn(t, j, plan()).err)

	// A passed run is not repeated.
	require.ErrorIs(t, resumeOn(t, j, plan()).err, pipeline.ErrAlreadyComplete)

	// A plan of a different shape is refused, forced or not.
	changed := plan(pipeline.Effect("new", ok()))
	require.ErrorIs(t, resumeOn(t, j, changed).err, pipeline.ErrPlanChanged)
	require.ErrorIs(t, resumeOn(t, j, changed, pipeline.Force()).err, pipeline.ErrPlanChanged)

	// Force runs it all again, and says so in the journal.
	forced := resumeOn(t, j, plan(), pipeline.Force())
	require.NoError(t, forced.err)
	assert.Equal(t, 2, forced.res.Attempt)
	assert.True(t, forced.events[0].Forced)
	assert.Equal(t, 2, c.get("a"))
}

func TestResume_ChangedWordingAndSettingsAreNotAChangedPlan(t *testing.T) {
	t.Parallel()

	failing := true
	c := &calls{}
	j := pipeline.MemoryJournal()

	require.Error(t, resumeOn(t, j, mustPlan(t,
		pipeline.Group("g", pipeline.Effect("a", counted(c, "a", &failing))).Parallel(8),
	)).err)

	failing = false

	// A hotfix build: reworded, with a lower limit and a retry.
	require.NoError(t, resumeOn(t, j, mustPlan(t,
		pipeline.Group("g",
			pipeline.Effect("a", counted(c, "a", &failing)).Description("reworded").Retry(3, time.Millisecond),
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

func (s setter) Check(context.Context, *pipeline.StepContext) (bool, error) {
	s.c.add("check " + s.label)

	return s.sys.is(s.name) == s.want, nil
}

func (s setter) Run(context.Context, *pipeline.StepContext) error {
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

	node := func(name string, apply *bool) pipeline.GroupNode {
		return pipeline.Group(name,
			pipeline.Ensure("maintenance", setter{sys: sys, c: c, label: name + "/maintenance", name: name + ".maint", want: true}),
			pipeline.Effect("stop", setter{sys: sys, c: c, label: name + "/stop", name: name + ".running", want: false}),
			pipeline.Effect("apply", setter{sys: sys, c: c, label: name + "/apply", name: name + ".upgraded", want: true, failing: apply}),
			pipeline.Effect("start", setter{sys: sys, c: c, label: name + "/start", name: name + ".running", want: true}),
			pipeline.Ensure("normal", setter{sys: sys, c: c, label: name + "/normal", name: name + ".maint", want: false}),
		)
	}

	sys.set("node1.running", true)
	sys.set("node2.running", true)

	plan := mustPlan(t,
		pipeline.Group("prechecks", pipeline.Observe("ring", counted(c, "ring", nil))),
		pipeline.Ensure("auto-repair-off", setter{sys: sys, c: c, label: "auto-repair-off", name: "auto-repair", want: false}),
		pipeline.Group("nodes", node("node1", nil), node("node2", &failing)),
	)
	j := pipeline.MemoryJournal()

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
		mode      pipeline.ResumeMode
		wantTrace []string
		wantRuns  int
	}{
		"verify finds the drift and applies again": {
			mode: pipeline.ResumeVerify,
			wantTrace: []string{
				"step.started p/config", "step.running p/config", "step.finished p/config passed",
				"step.started p/after", "step.running p/after", "step.finished p/after passed",
			},
			wantRuns: 2,
		},
		"trust skips without checking": {
			mode: pipeline.ResumeTrust,
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
				pipeline.Effect("config", setter{sys: sys, c: c, label: "config", name: "config", want: true}),
				pipeline.Effect("after", counted(c, "after", &failing)),
			)
			j := pipeline.MemoryJournal()

			require.Error(t, resumeOn(t, j, plan).err)

			sys.set("config", false) // drift between attempts
			c.reset()

			failing = false

			second := resumeOn(t, j, plan, pipeline.WithResume(tc.mode))
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
		pipeline.Effect("config", setter{sys: sys, c: c, label: "config", name: "config", want: true}),
		pipeline.Effect("after", counted(c, "after", &failing)),
	)
	j := pipeline.MemoryJournal()

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
		pipeline.Group("g",
			pipeline.Effect("broken", counted(c, "broken", &failing)),
			pipeline.Effect("signal", signals(pipeline.ErrSkipGroup)),
			pipeline.Effect("rest", counted(c, "rest", nil)),
		).ContinueOnError(),
		pipeline.Effect("after", counted(c, "after", nil)),
	)
	j := pipeline.MemoryJournal()

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
		node1, node2 pipeline.ElementState
		items        map[string]pipeline.ItemState
		dir          string
	}

	var got []seen

	j := pipeline.MemoryJournal()
	busy := true
	plan := mustPlan(t,
		pipeline.Ensure("reconcile", pipeline.StepFunc(func(_ context.Context, s *pipeline.StepContext) error {
			var dir string

			_, err := s.Recorded("p/nodes/node1/work", "dir", &dir)

			got = append(got, seen{node1: s.Prior("p/nodes/node1"), node2: s.Prior("p/nodes/node2"), dir: dir})

			return err
		})),
		pipeline.Group("nodes",
			pipeline.Group("node1", pipeline.Effect("work", pipeline.StepFunc(
				func(_ context.Context, s *pipeline.StepContext) error { return s.Record("dir", "/backups/1") }))),
			pipeline.Group("node2", pipeline.Effect("work", pipeline.StepFunc(
				func(_ context.Context, s *pipeline.StepContext) error {
					got[len(got)-1].items = s.PreviousItems()

					if err := s.Item("disk1", pipeline.ItemPassed, ""); err != nil {
						return err
					}

					if busy {
						_ = s.Item("disk2", pipeline.ItemFailed, "busy")

						return errors.New("disk2 busy")
					}

					return s.Item("disk2", pipeline.ItemPassed, "")
				}))),
		),
	)

	require.Error(t, resumeOn(t, j, plan).err)

	busy = false

	require.NoError(t, resumeOn(t, j, plan).err)
	require.NoError(t, resumeOn(t, j, plan, pipeline.Force()).err)
	require.Len(t, got, 3)

	// First attempt: nothing has happened yet.
	assert.Equal(t, pipeline.StatusNotReached, got[0].node1.Status)
	assert.Empty(t, got[0].items)
	assert.Empty(t, got[0].dir)

	// Resume: node1 finished, node2 failed, and its items are there to retry from.
	assert.True(t, got[1].node1.Done)
	assert.Equal(t, pipeline.StatusFailed, got[1].node2.Status)
	assert.Equal(t, pipeline.ItemPassed, got[1].items["disk1"].Status)
	assert.Equal(t, pipeline.ItemFailed, got[1].items["disk2"].Status)
	assert.Equal(t, "/backups/1", got[1].dir, "values from earlier attempts are readable")

	// Forced: decided as if nothing had happened, but values stay readable.
	assert.Equal(t, pipeline.StatusNotReached, got[2].node1.Status)
	assert.Empty(t, got[2].items)
	assert.Equal(t, "/backups/1", got[2].dir)
}

// A forced attempt starts everything over. If it fails part-way, the next
// ordinary resume must not fall back on what was done before the force.
func TestResume_AfterAForcedAttemptThatFailed(t *testing.T) {
	t.Parallel()

	c := &calls{}
	failing := false

	var priorDone []bool

	plan := mustPlan(t,
		pipeline.Effect("first", pipeline.StepFunc(func(_ context.Context, s *pipeline.StepContext) error {
			priorDone = append(priorDone, s.Prior("p/g").Done)

			if failing {
				return errors.New("first failed")
			}

			return s.Record("dir", "/backups/1")
		})),
		pipeline.Group("g",
			pipeline.Effect("work", pipeline.StepFunc(func(_ context.Context, s *pipeline.StepContext) error {
				c.add("work")

				return s.Item("disk1", pipeline.ItemPassed, "")
			})),
		),
	)
	j := pipeline.MemoryJournal()

	require.NoError(t, resumeOn(t, j, plan).err)

	// Forced, and it fails before it reaches g again.
	failing = true

	require.Error(t, resumeOn(t, j, plan, pipeline.Force()).err)

	failing = false

	state, err := j.State()
	require.NoError(t, err)
	assert.False(t, state.Element("p/g").Done, "the force undid g's completion")
	assert.Empty(t, state.Items("p/g/work"), "and its item progress")
	assert.True(t, state.Element("p/g/work").RunCalled, "its Run was still called once, for rollback")

	v, found := state.Value("p/first", "dir")
	require.True(t, found, "recorded values are kept")
	assert.JSONEq(t, `"/backups/1"`, string(v))

	third := resumeOn(t, j, plan)
	require.NoError(t, third.err)
	assert.Equal(t, 2, c.get("work"), "g runs again: the forced attempt never redid it")
	assert.Equal(t, []bool{false, false, false}, priorDone)
	assert.Contains(t, trace(third.events), "group.started p/g")
}

// A run-wide skip recorded inside a group still applies when that group is
// settled: if the process died after the group finished and before the run
// did, a resume must not run what the signal skipped.
func TestResume_ASettledGroupReappliesItsRunWideSkip(t *testing.T) {
	t.Parallel()

	c := &calls{}
	plan := mustPlan(t,
		pipeline.Group("g",
			pipeline.Effect("signal", signals(pipeline.ErrSkipRun)),
			pipeline.Effect("rest", counted(c, "rest", nil)),
		),
		pipeline.Effect("after", counted(c, "after", nil)),
	)

	crash := pipelinetest.CrashAfter("p/g", pipelinetest.GroupFinished)

	_, err := pipeline.Run(t.Context(), plan, crash.Journal)
	require.ErrorIs(t, err, pipelinetest.ErrCrashed)

	crash.Restart()

	second := resumeOn(t, crash.Journal, plan)
	require.NoError(t, second.err)
	assert.Equal(t, pipeline.OutcomePassed, second.res.Outcome)
	assert.Equal(t, []string{
		"group.finished p/g skipped/settled",
		"step.finished p/after skipped/run-skipped",
	}, trace(second.events))
	assert.Zero(t, c.get("after"))
	assert.Zero(t, c.get("rest"))
}

// A group-local skip inside a settled group stays inside it.
func TestResume_ASettledGroupKeepsItsGroupSkipToItself(t *testing.T) {
	t.Parallel()

	c := &calls{}
	failing := true

	plan := mustPlan(t,
		pipeline.Group("g",
			pipeline.Effect("signal", signals(pipeline.ErrSkipGroup)),
			pipeline.Effect("rest", counted(c, "rest", nil)),
		),
		pipeline.Effect("after", counted(c, "after", &failing)),
	)
	j := pipeline.MemoryJournal()

	require.Error(t, resumeOn(t, j, plan).err)

	failing = false

	second := resumeOn(t, j, plan)
	require.NoError(t, second.err)
	assert.Equal(t, 2, c.get("after"), "after runs: the skip was only for g")
	assert.Zero(t, c.get("rest"))
}
