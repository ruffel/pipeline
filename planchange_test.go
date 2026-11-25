package pipeline_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ruffel/pipeline"
	"github.com/ruffel/pipeline/internal/journalhook"
	"github.com/ruffel/pipeline/pipelinetest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// effect is an Effect step that counts its runs, can be undone, and fails
// while fails is set.
func effect(c *calls, name string, fails *bool) pipeline.StepNode {
	return pipeline.Effect(name, undoable{c: c, name: name, runFails: fails})
}

func matchByPath() pipeline.Option { return pipeline.WithPlanChange(pipeline.MatchByPath) }

// A fixed build adds a step and resumes what the broken build left.
func TestPlanChange_ResumesWithAnAddedStep(t *testing.T) {
	t.Parallel()

	var c calls

	fails := true
	before := mustPlan(t, effect(&c, "a", nil), effect(&c, "b", &fails))
	after := mustPlan(t, effect(&c, "a", nil), effect(&c, "hotfix", nil), effect(&c, "b", nil))
	j := pipeline.MemoryJournal()

	require.Error(t, resumeOn(t, j, before).err)

	// Without the option the journal is refused, and nothing is written.
	refused := resumeOn(t, j, after)
	require.ErrorIs(t, refused.err, pipeline.ErrPlanChanged)
	assert.Empty(t, refused.events)

	out := resumeOn(t, j, after, matchByPath())
	require.NoError(t, out.err)
	assert.Equal(t, 1, c.get("a"), "a is not run again")
	assert.Equal(t, 1, c.get("hotfix"))
	assert.Equal(t, 2, c.get("b"))

	changed := kinds(out.events, pipeline.EventPlanChanged)
	require.Len(t, changed, 1)
	assert.Equal(t, []string{"p/hotfix"}, changed[0].Added)
	assert.Empty(t, changed[0].Removed)
	assert.Equal(t, pipeline.EventRunStarted, out.events[0].Kind)
	assert.Equal(t, pipeline.EventPlanChanged, out.events[1].Kind, "recorded before anything runs")

	state, err := j.State()
	require.NoError(t, err)
	assert.Equal(t, after.Fingerprint(), state.Fingerprint)
	assert.Empty(t, state.Removed)

	// The journal now belongs to the new plan: it needs no option, and the
	// old plan is the changed one.
	require.ErrorIs(t, resumeOn(t, j, after).err, pipeline.ErrAlreadyComplete)
	require.ErrorIs(t, resumeOn(t, j, before).err, pipeline.ErrPlanChanged)
}

// A group that had settled is entered again for a step added to it. Its
// other steps are treated as on any resume.
func TestPlanChange_ANewStepUnsettlesItsGroup(t *testing.T) {
	t.Parallel()

	var c calls

	fails := true

	plan := func(extra ...pipeline.Element) *pipeline.Plan {
		node := append([]pipeline.Element{
			effect(&c, "drain", nil),
			pipeline.Ensure("setting", undoable{c: &c, name: "setting"}),
		}, extra...)

		return mustPlan(t,
			pipeline.Group("outer", pipeline.Group("node1", node...)),
			pipeline.Group("node2", effect(&c, "drain2", nil)),
			effect(&c, "last", &fails),
		)
	}

	before, after := plan(), plan(effect(&c, "repair", nil))
	j := pipeline.MemoryJournal()

	require.Error(t, resumeOn(t, j, before).err)

	fails = false
	out := resumeOn(t, j, after, matchByPath())
	require.NoError(t, out.err)

	assert.Equal(t, 1, c.get("repair"), "the new step runs")
	assert.Equal(t, 1, c.get("drain"), "a passed Effect step is skipped")
	assert.Equal(t, 2, c.get("setting"), "an Ensure step runs when its group is entered")
	assert.Equal(t, 1, c.get("drain2"), "a group with nothing new stays settled")

	lines := strings.Join(trace(out.events), "\n")
	assert.Contains(t, lines, "group.finished p/node2 skipped/settled")
	assert.Contains(t, lines, "group.finished p/outer/node1 passed")
}

// What a passed outcome means depends on the kind, so a changed kind is
// refused whatever the option says.
func TestPlanChange_AChangedKindIsRefused(t *testing.T) {
	t.Parallel()

	fails := true

	var c calls

	before := mustPlan(t, pipeline.Effect("a", ok()), effect(&c, "b", &fails))
	after := mustPlan(t, pipeline.Ensure("a", ok()), effect(&c, "b", nil))
	j := pipeline.MemoryJournal()

	require.Error(t, resumeOn(t, j, before).err)

	out := resumeOn(t, j, after, matchByPath())
	require.ErrorIs(t, out.err, pipeline.ErrPlanChanged)
	require.ErrorContains(t, out.err, "the kind of p/a differs")
	assert.Empty(t, out.events)
	assert.Nil(t, out.res)
}

// A removed step keeps its record, so a plan that puts its path back with
// another kind is refused: a passed Ensure step coming back as an Effect
// step would be skipped as done, and its Run never called.
func TestPlanChange_ARemovedPathMayNotComeBackAsAnotherKind(t *testing.T) {
	t.Parallel()

	var c calls

	fails := true
	before := mustPlan(t, pipeline.Ensure("setting", ok()), effect(&c, "b", &fails))
	without := mustPlan(t, effect(&c, "b", &fails))
	j := pipeline.MemoryJournal()

	require.Error(t, resumeOn(t, j, before).err)
	require.Error(t, resumeOn(t, j, without, matchByPath()).err)

	rekinded := mustPlan(t, effect(&c, "setting", nil), effect(&c, "b", nil))
	out := resumeOn(t, j, rekinded, matchByPath())
	require.ErrorIs(t, out.err, pipeline.ErrPlanChanged)
	require.ErrorContains(t, out.err, "the kind of p/setting differs")
	assert.Empty(t, out.events)
	assert.Zero(t, c.get("setting"))

	// With the kind it had, it carries on from its record.
	fails = false

	require.NoError(t, resumeOn(t, j, before, matchByPath()).err)

	state, err := j.State()
	require.NoError(t, err)
	assert.Empty(t, state.Removed)
}

// A removed step keeps its record. If it ran and had an Undo, the code to
// undo it has gone, so a rollback is refused.
func TestPlanChange_RemovedSteps(t *testing.T) {
	t.Parallel()

	var c calls

	fails := true
	before := mustPlan(t,
		effect(&c, "a", nil),
		effect(&c, "old", nil),
		pipeline.Effect("plain", ok()),
		effect(&c, "b", &fails),
		effect(&c, "never", nil),
	)
	after := mustPlan(t, effect(&c, "a", nil), effect(&c, "b", &fails))
	j := pipeline.MemoryJournal()

	require.Error(t, resumeOn(t, j, before).err)

	out := resumeOn(t, j, after, matchByPath())
	require.Error(t, out.err)

	changed := kinds(out.events, pipeline.EventPlanChanged)
	require.Len(t, changed, 1)
	assert.Equal(t, []string{"p/old", "p/plain", "p/never"}, changed[0].Removed)

	state, err := j.State()
	require.NoError(t, err)
	require.Len(t, state.Removed, 3)
	assert.Equal(t, "p/old", state.Removed[0].Path)
	assert.True(t, state.Removed[0].Undo)
	assert.Equal(t, pipeline.StatusPassed, state.Element("p/old").Status, "its record is kept")

	_, recorded := state.Value("p/old", "previous")
	assert.True(t, recorded, "and so is what it recorded")

	back := resumeRollback(t, j, after)
	require.ErrorIs(t, back.err, pipeline.ErrPlanChanged)
	require.ErrorContains(t, back.err, "p/old ran and was then removed")
	assert.NotContains(t, back.err.Error(), "p/never", "a removed step that never ran is only reported")
	assert.NotContains(t, back.err.Error(), "p/plain", "and so is one that had no Undo")
	assert.Empty(t, back.events)
	assert.Zero(t, c.get("undo a"), "nothing is undone")

	// Putting the step back makes the rollback possible again.
	back = resumeRollback(t, j, before, matchByPath())
	require.NoError(t, back.err)
	assert.Equal(t, 1, c.get("undo old"))
	assert.Equal(t, 1, c.get("undo a"))

	state, err = j.State()
	require.NoError(t, err)
	assert.Empty(t, state.Removed)
}

// A rollback given the changed plan itself decides in the same way.
func TestPlanChange_RollbackWithAChangedPlan(t *testing.T) {
	t.Parallel()

	var c calls

	before := mustPlan(t, effect(&c, "a", nil), pipeline.Effect("plain", ok()))
	after := mustPlan(t, effect(&c, "a", nil), effect(&c, "new", nil))
	gone := mustPlan(t, pipeline.Effect("plain", ok()))

	j := pipeline.MemoryJournal()
	require.NoError(t, resumeOn(t, j, before).err)

	require.ErrorIs(t, resumeRollback(t, j, gone, matchByPath()).err, pipeline.ErrPlanChanged)
	require.ErrorIs(t, resumeRollback(t, j, after, matchByPath(), pipeline.WithStrictUndo()).err, pipeline.ErrNoUndo)

	back := resumeRollback(t, j, after, matchByPath())
	require.NoError(t, back.err)
	assert.Equal(t, 1, c.get("undo a"))
	assert.Zero(t, c.get("undo new"))
	assert.Len(t, kinds(back.events, pipeline.EventPlanChanged), 1)
}

// A removed irreversible step that ran is still a point of no return.
func TestPlanChange_ARemovedIrreversibleStepStillRefusesRollback(t *testing.T) {
	t.Parallel()

	var c calls

	fails := true
	before := mustPlan(t,
		effect(&c, "a", nil), pipeline.Effect("alter-table", ok()).Irreversible(), effect(&c, "b", &fails),
	)
	after := mustPlan(t, effect(&c, "a", nil), effect(&c, "b", nil))
	j := pipeline.MemoryJournal()

	require.Error(t, resumeOn(t, j, before).err)
	require.NoError(t, resumeOn(t, j, after, matchByPath()).err)

	state, err := j.State()
	require.NoError(t, err)
	assert.Equal(t, "p/alter-table", state.PointOfNoReturn())
	require.ErrorIs(t, resumeRollback(t, j, after).err, pipeline.ErrIrreversible)
}

// The state comes from the plans in run.started, so a crash before
// plan.changed is written loses nothing: wherever the changed attempt
// crashes, the next one finishes the same work.
func TestPlanChange_ACrashAtAnyPointEndsTheSame(t *testing.T) {
	t.Parallel()

	final := func(t *testing.T, crashAt int) (string, int) {
		t.Helper()

		var c calls

		fails := true
		group := func(extra ...pipeline.Element) pipeline.GroupNode {
			return pipeline.Group("g", append([]pipeline.Element{effect(&c, "a", nil)}, extra...)...)
		}
		before := mustPlan(t, group(), effect(&c, "old", nil), effect(&c, "b", &fails))
		after := mustPlan(t, group(effect(&c, "new", nil)), effect(&c, "b", nil))

		// The journal stores crashAt events of the changed attempt, then
		// fails every write, until the process starts again.
		var (
			armed   bool
			written int
		)

		j := pipeline.MemoryJournal()
		journalhook.Set(j, func(pipeline.Event) error {
			if armed && crashAt > 0 && written >= crashAt {
				return pipelinetest.ErrCrashed
			}

			written++

			return nil
		})

		_, err := pipeline.Run(t.Context(), before, j)
		require.Error(t, err)

		armed, written = true, 0

		_, err = pipeline.Run(t.Context(), after, j, matchByPath())
		if crashAt > 0 && written >= crashAt {
			require.ErrorIs(t, err, pipelinetest.ErrCrashed)

			armed = false

			_, err = pipeline.Run(t.Context(), after, j, matchByPath())
		}

		require.NoError(t, err)

		state, err := j.State()
		require.NoError(t, err)

		lines := make([]string, 0, 6+len(state.Removed))

		for _, path := range []string{"p", "p/g", "p/g/a", "p/g/new", "p/old", "p/b"} {
			el := state.Element(path)
			lines = append(lines, fmt.Sprintf("%s done=%t", path, el.Done))
		}

		for _, n := range state.Removed {
			lines = append(lines, "removed "+n.Path)
		}

		// A step whose Run a crash interrupted is run again, as on any
		// resume; one that had passed is not.
		assert.GreaterOrEqual(t, c.get("new"), 1, "crash at event %d", crashAt)
		assert.Equal(t, 1, c.get("a"), "crash at event %d", crashAt)

		return strings.Join(lines, "\n"), written
	}

	want, events := final(t, 0)
	require.Contains(t, want, "removed p/old")
	require.Greater(t, events, 5)

	for at := 1; at < events; at++ {
		got, _ := final(t, at)
		assert.Equal(t, want, got, "crash at event %d", at)
	}
}

// A viewer reads a changed plan from a file journal, and follows it.
func TestPlanChange_InspectAndFollow(t *testing.T) {
	t.Parallel()

	var (
		c calls
		b strings.Builder
	)

	fails := true
	before := mustPlan(t, effect(&c, "a", nil), effect(&c, "old", nil), effect(&c, "b", &fails))
	after := mustPlan(t, effect(&c, "a", nil), effect(&c, "new", nil), effect(&c, "b", nil))
	path := t.TempDir() + "/upgrade.jsonl"

	j, err := pipeline.OpenJournal(path)
	require.NoError(t, err)

	_, err = pipeline.Run(t.Context(), before, j)
	require.Error(t, err)

	_, err = pipeline.Run(t.Context(), after, j, matchByPath())
	require.NoError(t, err)
	require.NoError(t, j.Close())

	state, err := pipeline.Inspect(path)
	require.NoError(t, err)
	require.Len(t, state.Removed, 1)
	assert.Equal(t, "p/old", state.Removed[0].Path)
	assert.Equal(t, pipeline.StatusPassed, state.Element("p/new").Status)
	assert.Len(t, state.Plan.Children, 3)

	require.NoError(t, followErr(pipeline.Follow(t.Context(), path, pipeline.FollowOptions{}, pipeline.NewPlainObserver(&b))))
	assert.Contains(t, b.String(), "± the plan has changed; added p/new; removed p/old\n")
}
