package pipeline_test

import (
	"testing"

	"github.com/ruffel/pipeline"
	"github.com/ruffel/pipeline/pipelinetest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// upgradePlan is a backup that can be undone, a schema change that can't,
// and a step that fails while fails is set.
func upgradePlan(t *testing.T, c *calls, fails *bool) *pipeline.Plan {
	t.Helper()

	return mustPlan(t,
		pipeline.Effect("backup", undoable{c: c, name: "backup"}),
		pipeline.Effect("alter-table", counted(c, "alter-table", nil)).Irreversible(),
		pipeline.Effect("node1", undoable{c: c, name: "node1", runFails: fails}),
	)
}

// Once an irreversible step has run, a rollback would not put the system
// back where it started, so it is refused before it undoes anything.
func TestRollback_RefusesOnceAnIrreversibleStepHasRun(t *testing.T) {
	t.Parallel()

	var c calls

	fails := true
	plan := upgradePlan(t, &c, &fails)
	j := pipeline.MemoryJournal()

	require.Error(t, resumeOn(t, j, plan).err)

	before, err := j.Events()
	require.NoError(t, err)

	back := resumeRollback(t, j, plan)
	require.ErrorIs(t, back.err, pipeline.ErrIrreversible)
	assert.Nil(t, back.res)

	var refused *pipeline.IrreversibleError

	require.ErrorAs(t, back.err, &refused)
	assert.Equal(t, "p/alter-table", refused.Path)
	assert.Equal(t, 1, refused.Attempt)
	require.ErrorContains(t, back.err, "p/alter-table, in attempt 1")

	assert.Zero(t, c.get("undo backup"), "nothing is undone")
	assert.Zero(t, c.get("undo node1"))

	after, err := j.Events()
	require.NoError(t, err)
	assert.Len(t, after, len(before), "the refusal writes nothing")

	state, err := j.State()
	require.NoError(t, err)
	assert.Equal(t, "p/alter-table", state.PointOfNoReturn())

	// The journal has no unfinished rollback in it, so the run resumes.
	fails = false

	require.NoError(t, resumeOn(t, j, plan).err)
	require.ErrorIs(t, resumeRollback(t, j, plan).err, pipeline.ErrIrreversible)
}

// A step whose Run was called counts, however that Run ended.
func TestRollback_AnIrreversibleStepThatFailedCounts(t *testing.T) {
	t.Parallel()

	var c calls

	plan := mustPlan(t,
		pipeline.Effect("backup", undoable{c: &c, name: "backup"}),
		pipeline.Effect("alter-table", fail("boom")).Irreversible(),
	)
	j := pipeline.MemoryJournal()

	require.Error(t, resumeOn(t, j, plan).err)
	require.ErrorIs(t, resumeRollback(t, j, plan).err, pipeline.ErrIrreversible)
	assert.Zero(t, c.get("undo backup"))

	// And whatever a later plan calls the step.
	unmarked := mustPlan(t,
		pipeline.Effect("backup", undoable{c: &c, name: "backup"}),
		pipeline.Effect("alter-table", undoable{c: &c, name: "alter-table"}),
	)

	require.ErrorIs(t, resumeRollback(t, j, unmarked).err, pipeline.ErrIrreversible)
	assert.Zero(t, c.get("undo backup"))
	assert.Zero(t, c.get("undo alter-table"))
}

// Before the irreversible step runs, a rollback goes ahead as usual.
func TestRollback_BeforeAnIrreversibleStepRunsPasses(t *testing.T) {
	t.Parallel()

	var c calls

	fails := true
	plan := mustPlan(t,
		pipeline.Effect("backup", undoable{c: &c, name: "backup"}),
		pipeline.Effect("node1", undoable{c: &c, name: "node1", runFails: &fails}),
		pipeline.Effect("alter-table", ok()).Irreversible(),
	)
	j := pipeline.MemoryJournal()

	require.Error(t, resumeOn(t, j, plan).err)

	state, err := j.State()
	require.NoError(t, err)
	assert.Empty(t, state.PointOfNoReturn())

	back := resumeRollback(t, j, plan)
	require.NoError(t, back.err)
	assert.Equal(t, pipeline.OutcomePassed, back.res.Outcome)
	assert.Equal(t, 1, c.get("undo backup"))
	assert.Equal(t, 1, c.get("undo node1"))
}

// An irreversible step that Check found already done was never run.
func TestRollback_AnIrreversibleStepFoundDoneWasNotRun(t *testing.T) {
	t.Parallel()

	var c calls

	plan := mustPlan(t,
		pipeline.Effect("backup", undoable{c: &c, name: "backup"}),
		pipeline.Effect("alter-table", &checkStep{done: func() bool { return true }}).Irreversible(),
	)
	j := pipeline.MemoryJournal()

	require.NoError(t, resumeOn(t, j, plan).err)
	require.NoError(t, resumeRollback(t, j, plan).err)
	assert.Equal(t, 1, c.get("undo backup"))
}

// A recovery step resets its group, and the journal forgets what ran in it.
// It can't take back what an irreversible step did, so that is remembered.
func TestRollback_ARecoveryResetDoesNotForgetAnIrreversibleStep(t *testing.T) {
	t.Parallel()

	var c calls

	fails := true
	plan := mustPlan(t,
		pipeline.Effect("backup", undoable{c: &c, name: "backup"}),
		pipeline.Group("node",
			pipeline.Effect("format", ok()).Irreversible(),
			pipeline.Effect("apply", undoable{c: &c, name: "apply", runFails: &fails}),
		).RecoverOnFailure(pipeline.Effect("recover", undoable{c: &c, name: "recover"})),
	)
	j := pipeline.MemoryJournal()

	require.Error(t, resumeOn(t, j, plan).err)

	state, err := j.State()
	require.NoError(t, err)
	assert.Equal(t, pipeline.StatusNotReached, state.Element("p/node/format").Status, "the group was reset")
	assert.Equal(t, "p/node/format", state.PointOfNoReturn())

	back := resumeRollback(t, j, plan)
	require.ErrorIs(t, back.err, pipeline.ErrIrreversible)
	assert.Zero(t, c.get("undo backup"))
	assert.Zero(t, c.get("undo recover"))

	// A forced attempt starts over, and doesn't forget it either.
	fails = false

	require.NoError(t, resumeOn(t, j, plan).err)
	require.NoError(t, resumeOn(t, j, plan, pipeline.Force()).err)

	var refused *pipeline.IrreversibleError

	require.ErrorAs(t, resumeRollback(t, j, plan).err, &refused)
	assert.Equal(t, 1, refused.Attempt, "the first attempt that ran it")
}

// A crash at any point of the irreversible step leaves the journal saying
// what it would have said without one: once step.running is written, Run may
// have been called.
func TestRollback_ACrashAroundAnIrreversibleStep(t *testing.T) {
	t.Parallel()

	for point, want := range map[pipelinetest.Point]string{
		pipelinetest.Started:  "",
		pipelinetest.Running:  "p/alter-table",
		pipelinetest.Finished: "p/alter-table",
	} {
		t.Run(string(point), func(t *testing.T) {
			t.Parallel()

			var c calls

			fails := false
			plan := upgradePlan(t, &c, &fails)
			crash := pipelinetest.CrashAfter("p/alter-table", point)

			_, err := pipeline.Run(t.Context(), plan, crash.Journal)
			require.ErrorIs(t, err, pipelinetest.ErrCrashed)

			crash.Restart()

			state, err := crash.Journal.State()
			require.NoError(t, err)
			assert.Equal(t, want, state.PointOfNoReturn())

			back := resumeRollback(t, crash.Journal, plan)

			if want == "" {
				require.NoError(t, back.err)
				assert.Equal(t, 1, c.get("undo backup"))

				return
			}

			require.ErrorIs(t, back.err, pipeline.ErrIrreversible)
			assert.Zero(t, c.get("undo backup"))
		})
	}
}

// The point of no return is read from a file journal as from any other.
func TestInspect_PointOfNoReturn(t *testing.T) {
	t.Parallel()

	var c calls

	fails := true
	plan := upgradePlan(t, &c, &fails)
	path := t.TempDir() + "/upgrade.jsonl"

	j, err := pipeline.OpenJournal(path)
	require.NoError(t, err)

	_, err = pipeline.Run(t.Context(), plan, j)
	require.Error(t, err)
	require.NoError(t, j.Close())

	state, err := pipeline.Inspect(path)
	require.NoError(t, err)
	assert.Equal(t, "p/alter-table", state.PointOfNoReturn())
	require.NotNil(t, state.Plan)
	assert.True(t, state.Plan.Children[1].Irreversible)
}

// WithStrictUndo refuses a rollback that would leave a step's work in place.
func TestRollback_StrictUndo(t *testing.T) {
	t.Parallel()

	var c calls

	plan := mustPlan(t,
		pipeline.Effect("a", undoable{c: &c, name: "a"}),
		pipeline.Effect("b", ok()),
		pipeline.Ensure("c", ok()),
		pipeline.Observe("look", ok()),
		pipeline.Effect("found-done", &checkStep{done: func() bool { return true }}),
	)
	j := pipeline.MemoryJournal()

	require.NoError(t, resumeOn(t, j, plan).err)

	back := resumeRollback(t, j, plan, pipeline.WithStrictUndo())
	require.ErrorIs(t, back.err, pipeline.ErrNoUndo)
	assert.Nil(t, back.res)

	var refused *pipeline.NoUndoError

	require.ErrorAs(t, back.err, &refused)
	assert.Equal(t, []string{"p/c", "p/b"}, refused.Paths, "in the order a rollback reaches them")
	require.EqualError(t, refused, "pipeline: a step that ran has no Undo: p/c, p/b")
	assert.Zero(t, c.get("undo a"), "nothing is undone")

	// Without the option the rollback passes, as it always did.
	require.NoError(t, resumeRollback(t, j, plan).err)
	assert.Equal(t, 1, c.get("undo a"))
}

// Under WithStrictUndo a plan whose steps that ran all have an Undo is
// rolled back as usual, and an irreversible step is reported as that.
func TestRollback_StrictUndoWithNothingToRefuse(t *testing.T) {
	t.Parallel()

	var c calls

	plan := mustPlan(t, pipeline.Effect("a", undoable{c: &c, name: "a"}), pipeline.Observe("look", ok()))
	j := pipeline.MemoryJournal()

	require.NoError(t, resumeOn(t, j, plan).err)
	require.NoError(t, resumeRollback(t, j, plan, pipeline.WithStrictUndo()).err)
	assert.Equal(t, 1, c.get("undo a"))

	fails := false
	irreversible := upgradePlan(t, &c, &fails)
	j = pipeline.MemoryJournal()

	require.NoError(t, resumeOn(t, j, irreversible).err)
	require.ErrorIs(t, resumeRollback(t, j, irreversible, pipeline.WithStrictUndo()).err, pipeline.ErrIrreversible)
}

func TestNewPlan_IrreversibleSteps(t *testing.T) {
	t.Parallel()

	var c calls

	_, err := pipeline.NewPlan("p",
		pipeline.Effect("undoable", undoable{c: &c, name: "a"}).Irreversible(),
		pipeline.Observe("look", ok()).Irreversible(),
	)
	require.ErrorContains(t, err, "p/undoable: an irreversible step may not have Undo")
	require.ErrorContains(t, err, "p/look: an observe step changes nothing, so it may not be irreversible")

	plan := mustPlan(t,
		pipeline.Effect("alter-table", ok()).Irreversible(),
		pipeline.Ensure("setting", ok()).Irreversible(),
	)
	assert.Equal(t, "p\n├── alter-table (effect, irreversible)\n└── setting (ensure, irreversible)\n", pipeline.Describe(plan))

	// The marker is a setting, not part of the plan's shape: marking a step
	// in a fixed build doesn't strand a journal.
	unmarked := mustPlan(t, pipeline.Effect("alter-table", ok()), pipeline.Ensure("setting", ok()))
	assert.Equal(t, unmarked.Fingerprint(), plan.Fingerprint())
}

// laterPlan is upgradePlan as a later build has it: the same shape, with
// alter-table's marker gone. With undo, the later build also gave it an Undo.
func laterPlan(t *testing.T, c *calls, fails *bool, undo bool) *pipeline.Plan {
	t.Helper()

	alter := pipeline.Effect("alter-table", counted(c, "alter-table", nil))
	if undo {
		alter = pipeline.Effect("alter-table", undoable{c: c, name: "alter-table"})
	}

	return mustPlan(t,
		pipeline.Effect("backup", undoable{c: c, name: "backup"}),
		alter,
		pipeline.Effect("node1", undoable{c: c, name: "node1", runFails: fails}),
	)
}

// refusesAt checks that a rollback of plan is refused for alter-table, which
// attempt 1 ran, that it writes nothing and that no Undo is called.
func refusesAt(t *testing.T, c *calls, j *pipeline.Journal, plan *pipeline.Plan, opts ...pipeline.Option) {
	t.Helper()

	back := resumeRollback(t, j, plan, opts...)
	require.ErrorIs(t, back.err, pipeline.ErrIrreversible)
	assert.Nil(t, back.res)
	assert.Empty(t, back.events, "the refusal writes nothing")

	var refused *pipeline.IrreversibleError

	require.ErrorAs(t, back.err, &refused)
	assert.Equal(t, "p/alter-table", refused.Path)
	assert.Equal(t, 1, refused.Attempt, "the attempt that ran it while it was irreversible")

	for _, name := range []string{"backup", "alter-table", "node1"} {
		assert.Zero(t, c.get("undo "+name), "nothing is undone")
	}

	state, err := j.State()
	require.NoError(t, err)
	assert.Equal(t, "p/alter-table", state.PointOfNoReturn())
}

// What a step was when it ran is the journal's to say, not the latest
// plan's. A later build that drops the marker, and even gives the step an
// Undo, has the same shape, so nothing else would notice the change.
func TestRollback_DroppingTheMarkerDoesNotForgetAnIrreversibleStep(t *testing.T) {
	t.Parallel()

	for name, undo := range map[string]bool{"with an Undo": true, "with none": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var c calls

			fails := true
			marked, unmarked := upgradePlan(t, &c, &fails), laterPlan(t, &c, &fails, undo)
			require.Equal(t, marked.Fingerprint(), unmarked.Fingerprint())

			j := pipeline.MemoryJournal()
			require.Error(t, resumeOn(t, j, marked).err)

			// The later plan is refused when it is first given, and once an
			// attempt has made it the journal's latest.
			refusesAt(t, &c, j, unmarked)
			require.Error(t, resumeOn(t, j, unmarked).err)
			refusesAt(t, &c, j, unmarked)
			refusesAt(t, &c, j, unmarked, pipeline.WithStrictUndo())

			fails = false

			require.NoError(t, resumeOn(t, j, unmarked).err)
			refusesAt(t, &c, j, unmarked)
			refusesAt(t, &c, j, marked)
		})
	}
}

// A path that is taken out of the plan and put back without its marker is
// the step that ran.
func TestRollback_ARemovedPathComingBackDoesNotForgetAnIrreversibleStep(t *testing.T) {
	t.Parallel()

	var c calls

	fails := true
	marked, unmarked := upgradePlan(t, &c, &fails), laterPlan(t, &c, &fails, true)
	without := mustPlan(t,
		pipeline.Effect("backup", undoable{c: &c, name: "backup"}),
		pipeline.Effect("node1", undoable{c: &c, name: "node1", runFails: &fails}),
	)

	j := pipeline.MemoryJournal()
	require.Error(t, resumeOn(t, j, marked).err)
	require.Error(t, resumeOn(t, j, without, matchByPath()).err)
	refusesAt(t, &c, j, without)

	refusesAt(t, &c, j, unmarked, matchByPath())
	require.Error(t, resumeOn(t, j, unmarked, matchByPath()).err)
	refusesAt(t, &c, j, unmarked)

	state, err := j.State()
	require.NoError(t, err)
	assert.Empty(t, state.Removed)
}

// A marker dropped by one build and a path removed by the next: the removed
// step is listed as it was last planned, which is not as it ran.
func TestRollback_ARemovedStepThatLostItsMarkerFirstIsNotForgotten(t *testing.T) {
	t.Parallel()

	var c calls

	fails := true
	marked, unmarked := upgradePlan(t, &c, &fails), laterPlan(t, &c, &fails, false)
	without := mustPlan(t,
		pipeline.Effect("backup", undoable{c: &c, name: "backup"}),
		pipeline.Effect("node1", undoable{c: &c, name: "node1", runFails: &fails}),
	)

	j := pipeline.MemoryJournal()
	require.Error(t, resumeOn(t, j, marked).err)
	require.Error(t, resumeOn(t, j, unmarked).err)
	require.Error(t, resumeOn(t, j, without, matchByPath()).err)

	state, err := j.State()
	require.NoError(t, err)
	require.Len(t, state.Removed, 1)
	assert.False(t, state.Removed[0].Irreversible, "as it was last planned")

	refusesAt(t, &c, j, without)
	refusesAt(t, &c, j, without, pipeline.WithStrictUndo())
}

// Once step.running is stored the step may have run, whatever a later plan
// calls it. Before that, nothing has.
func TestRollback_ACrashAroundAnIrreversibleStepOutlastsItsMarker(t *testing.T) {
	t.Parallel()

	for point, ran := range map[pipelinetest.Point]bool{
		pipelinetest.Started:  false,
		pipelinetest.Running:  true,
		pipelinetest.Finished: true,
	} {
		t.Run(string(point), func(t *testing.T) {
			t.Parallel()

			var c calls

			fails := false
			marked, unmarked := upgradePlan(t, &c, &fails), laterPlan(t, &c, &fails, true)
			crash := pipelinetest.CrashAfter("p/alter-table", point)

			_, err := pipeline.Run(t.Context(), marked, crash.Journal)
			require.ErrorIs(t, err, pipelinetest.ErrCrashed)

			crash.Restart()

			if ran {
				refusesAt(t, &c, crash.Journal, unmarked)

				return
			}

			// step.running could not be stored, so Run was not called.
			assert.Zero(t, c.get("alter-table"))
			require.NoError(t, resumeRollback(t, crash.Journal, unmarked).err)
			assert.Equal(t, 1, c.get("undo backup"))
			assert.Zero(t, c.get("undo alter-table"), "its Run was never called")
		})
	}
}

// The attempt named is the one that ran the step while it was irreversible,
// whichever plan the rollback is given: not an earlier one that ran it
// before it was marked.
func TestRollback_NamesTheAttemptThatRanTheStepWhileItWasIrreversible(t *testing.T) {
	t.Parallel()

	var c calls

	fails := true
	unmarked, marked := laterPlan(t, &c, &fails, false), upgradePlan(t, &c, &fails)
	j := pipeline.MemoryJournal()

	require.Error(t, resumeOn(t, j, unmarked).err)
	require.Error(t, resumeOn(t, j, marked, pipeline.Force()).err)
	require.Equal(t, 2, c.get("alter-table"), "the forced attempt ran it again, marked")

	for name, plan := range map[string]*pipeline.Plan{"the plan that marks it": marked, "one that does not": unmarked} {
		var refused *pipeline.IrreversibleError

		require.ErrorAs(t, resumeRollback(t, j, plan).err, &refused, name)
		assert.Equal(t, "p/alter-table", refused.Path, name)
		assert.Equal(t, 2, refused.Attempt, name)
	}
}

// A recovery step's reset and a forced attempt each forget what ran. Neither
// forgets an irreversible step, and a later plan can't make them.
func TestRollback_AResetOrAForcedAttemptDoesNotForgetWithTheMarkerGone(t *testing.T) {
	t.Parallel()

	var c calls

	fails := true

	plan := func(format pipeline.StepNode) *pipeline.Plan {
		return mustPlan(t,
			pipeline.Effect("backup", undoable{c: &c, name: "backup"}),
			pipeline.Group("node",
				format,
				pipeline.Effect("apply", undoable{c: &c, name: "apply", runFails: &fails}),
			).RecoverOnFailure(pipeline.Effect("recover", undoable{c: &c, name: "recover"})),
		)
	}

	marked := plan(pipeline.Effect("format", counted(&c, "format", nil)).Irreversible())
	unmarked := plan(pipeline.Effect("format", undoable{c: &c, name: "format"}))
	j := pipeline.MemoryJournal()

	require.Error(t, resumeOn(t, j, marked).err)

	refuses := func() {
		t.Helper()

		var refused *pipeline.IrreversibleError

		require.ErrorAs(t, resumeRollback(t, j, unmarked).err, &refused)
		assert.Equal(t, "p/node/format", refused.Path)
		assert.Equal(t, 1, refused.Attempt)

		for _, name := range []string{"backup", "format", "apply", "recover"} {
			assert.Zero(t, c.get("undo "+name), "nothing is undone")
		}
	}

	refuses()

	fails = false

	require.NoError(t, resumeOn(t, j, unmarked).err)
	require.NoError(t, resumeOn(t, j, unmarked, pipeline.Force()).err)
	refuses()

	state, err := j.State()
	require.NoError(t, err)
	assert.Equal(t, "p/node/format", state.PointOfNoReturn())
}

// A step that ran before a later build marked it is no easier to undo for
// having run first.
func TestRollback_AStepMarkedAfterItRanCounts(t *testing.T) {
	t.Parallel()

	var c calls

	fails := true
	unmarked, marked := laterPlan(t, &c, &fails, false), upgradePlan(t, &c, &fails)
	j := pipeline.MemoryJournal()

	require.Error(t, resumeOn(t, j, unmarked).err)

	state, err := j.State()
	require.NoError(t, err)
	assert.Empty(t, state.PointOfNoReturn(), "the journal has not seen the marker yet")

	var refused *pipeline.IrreversibleError

	back := resumeRollback(t, j, marked)
	require.ErrorAs(t, back.err, &refused)
	assert.Equal(t, "p/alter-table", refused.Path)
	assert.Equal(t, 1, refused.Attempt)
	assert.Empty(t, back.events)
	assert.Zero(t, c.get("undo backup"), "nothing is undone")

	// Once an attempt has recorded the plan that marks it, the journal says so.
	require.Error(t, resumeOn(t, j, marked).err)
	refusesAt(t, &c, j, marked)
}
