package pipeline_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ruffel/pipeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// warns is a step that records the warnings it is given each time it runs,
// and an item that warned if item is set, and fails while fails is set. Its
// Check reports done once done is set, and it can be undone.
type warns struct {
	warnings *[]string
	fails    *bool
	done     *bool
	item     bool
	// checked, if set, is a warning that its Check records.
	checked string
}

func (w warns) Check(_ context.Context, s *pipeline.StepContext) (bool, error) {
	if w.checked != "" {
		if err := s.RecordWarning(w.checked); err != nil {
			return false, err
		}
	}

	return w.done != nil && *w.done, nil
}

func (w warns) Run(_ context.Context, s *pipeline.StepContext) error {
	if w.item {
		if err := s.Item("node7", pipeline.ItemWarned, "clock is 2s out"); err != nil {
			return err
		}
	}

	for _, msg := range *w.warnings {
		if err := s.RecordWarning(msg); err != nil {
			return err
		}
	}

	if w.fails != nil && *w.fails {
		return errors.New("boom")
	}

	return nil
}

func (warns) Undo(context.Context, *pipeline.StepContext) error { return nil }

// said is a state's warnings as "path: message", an item's as
// "path[item]: message".
func warned(state *pipeline.State) []string {
	warnings := state.Warnings()
	out := make([]string, 0, len(warnings))

	for _, w := range warnings {
		at := w.Path
		if w.Item != "" {
			at += "[" + w.Item + "]"
		}

		out = append(out, at+": "+w.Message)
	}

	return out
}

// A recorded warning is durable, does not fail its step or change the run's
// outcome, and is listed with the items that warned, in plan order.
func TestRecordWarning_IsKeptAndListedInPlanOrder(t *testing.T) {
	t.Parallel()

	var b strings.Builder

	plan := mustPlan(t,
		pipeline.Effect("upgrade", pipeline.StepFunc(func(_ context.Context, s *pipeline.StepContext) error {
			s.Warn("a message, for whoever is watching")

			return errors.Join(
				s.Item("node9", pipeline.ItemWarned, "clock is 3s out"),
				s.Item("node8", pipeline.ItemPassed, ""),
				s.Item("node7", pipeline.ItemWarned, "clock is 2s out"),
				s.RecordWarning("the old packages could not be removed"),
				s.RecordWarning("the cache was not warmed"),
			)
		})),
		pipeline.Effect("tidy", pipeline.StepFunc(func(_ context.Context, s *pipeline.StepContext) error {
			return s.RecordWarning("monitoring is still off")
		})),
	)
	j := pipeline.MemoryJournal()

	out := resumeOn(t, j, plan, pipeline.WithObservers(pipeline.NewPlainObserver(&b)))

	require.NoError(t, out.err, "a warning fails nothing")
	assert.Equal(t, pipeline.OutcomePassed, out.res.Outcome)

	recorded := kinds(out.events, pipeline.EventWarning)
	require.Len(t, recorded, 3)
	assert.True(t, recorded[0].Kind.Durable())
	assert.Equal(t, "p/upgrade", recorded[0].Path)

	state := stateOf(t, j)
	assert.Equal(t, []string{
		"p/upgrade: the old packages could not be removed",
		"p/upgrade: the cache was not warmed",
		"p/upgrade[node7]: clock is 2s out",
		"p/upgrade[node9]: clock is 3s out",
		"p/tidy: monitoring is still off",
	}, warned(state), "a step's own first, then its items by target; Warn's message is not among them")

	assert.Equal(t, out.res.Run, state.Warnings()[0].Run)
	assert.Equal(t, pipeline.ItemWarned, state.Items("p/upgrade")["node7"].Status)
	assert.False(t, state.Unfinished(), "the run passed")

	assert.Contains(t, b.String(), "upgrade: [WARN] the cache was not warmed\n")
	assert.Contains(t, b.String(), "upgrade: node7 warned — clock is 2s out\n")
}

// A step's warnings stand until an attempt calls its Run again. So a step
// that finished with a warning still has it after the attempts that skipped
// it, and one that runs again says afresh what is wrong.
func TestRecordWarning_StandsUntilTheStepRunsAgain(t *testing.T) {
	t.Parallel()

	t.Run("a later attempt skips the step", func(t *testing.T) {
		t.Parallel()

		warnings, fails, done := []string{"a later task failed"}, true, false
		plan := mustPlan(t,
			pipeline.Effect("done", pipeline.StepFunc(warns{warnings: &warnings}.Run)),
			pipeline.Effect("checked", warns{warnings: &warnings, done: &done}),
			pipeline.Effect("after", counted(&calls{}, "after", &fails)),
		)
		j := pipeline.MemoryJournal()

		require.ErrorContains(t, resumeOn(t, j, plan).err, "after failed")

		warnings, fails, done = []string{"not said: neither step runs again"}, false, true
		out := resumeOn(t, j, plan)

		require.NoError(t, out.err)
		assert.Empty(t, kinds(out.events, pipeline.EventWarning))
		assert.Equal(t, []string{"p/done: a later task failed", "p/checked: a later task failed"}, warned(stateOf(t, j)),
			"one was trusted, and the other's Check found its work done")
	})

	// Its Run is called after its Check, in the same attempt: that forgets
	// nothing the Check has just said.
	t.Run("its Check warns, and then it runs", func(t *testing.T) {
		t.Parallel()

		warnings := []string{"the old packages could not be removed"}
		plan := mustPlan(t, pipeline.Effect("a", warns{warnings: &warnings, checked: "the node's clock is out"}))
		j := pipeline.MemoryJournal()

		require.NoError(t, resumeOn(t, j, plan).err)
		assert.Equal(t, []string{"p/a: the node's clock is out", "p/a: the old packages could not be removed"}, warned(stateOf(t, j)))
	})

	t.Run("a later attempt runs it again", func(t *testing.T) {
		t.Parallel()

		warnings, fails := []string{"the mirror is slow", "the mirror timed out once"}, true
		plan := mustPlan(t, pipeline.Effect("a", warns{warnings: &warnings, fails: &fails}).Retry(2, 0))
		j := pipeline.MemoryJournal()

		require.ErrorContains(t, resumeOn(t, j, plan).err, "boom")
		assert.Len(t, warned(stateOf(t, j)), 4, "what both tries of the attempt said")

		warnings, fails = []string{"the mirror is still slow"}, false

		require.NoError(t, resumeOn(t, j, plan).err)
		assert.Equal(t, []string{"p/a: the mirror is still slow"}, warned(stateOf(t, j)))

		warnings = nil

		require.NoError(t, resumeOn(t, j, plan, pipeline.Force()).err)
		assert.Empty(t, warned(stateOf(t, j)), "a clean run leaves none")
	})
}

// A warning goes with the step's other records: when a rollback undoes the
// step, a recovery step resets its group, or a forced attempt starts over,
// even one that fails before it reaches the step.
func TestRecordWarning_IsForgottenWithTheStepsOtherRecords(t *testing.T) {
	t.Parallel()

	for name, forget := range map[string]func(t *testing.T, j *pipeline.Journal, plan *pipeline.Plan, early *bool){
		"a rollback undid the step": func(t *testing.T, j *pipeline.Journal, plan *pipeline.Plan, _ *bool) {
			t.Helper()

			require.NoError(t, resumeRollback(t, j, plan).err)
		},
		"a forced attempt started over": func(t *testing.T, j *pipeline.Journal, plan *pipeline.Plan, early *bool) {
			t.Helper()

			*early = true

			require.ErrorContains(t, resumeOn(t, j, plan, pipeline.Force()).err, "first failed")
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			early := false
			warnings := []string{"the old packages could not be removed"}
			plan := mustPlan(t,
				pipeline.Effect("first", counted(&calls{}, "first", &early)),
				pipeline.Effect("a", warns{warnings: &warnings, item: true}),
			)
			j := pipeline.MemoryJournal()

			require.NoError(t, resumeOn(t, j, plan).err)
			require.Len(t, warned(stateOf(t, j)), 2)

			forget(t, j, plan, &early)

			assert.Empty(t, warned(stateOf(t, j)))
		})
	}

	t.Run("a recovery step reset its group", func(t *testing.T) {
		t.Parallel()

		warnings, fails := []string{"the old packages could not be removed"}, true
		plan := mustPlan(t, pipeline.Group("node",
			pipeline.Effect("a", warns{warnings: &warnings, fails: &fails}),
		).RecoverOnFailure(pipeline.Effect("restore", pipeline.StepFunc(func(_ context.Context, s *pipeline.StepContext) error {
			return s.RecordWarning("the node was put back by hand")
		}))))
		j := pipeline.MemoryJournal()

		require.ErrorContains(t, resumeOn(t, j, plan).err, "boom")
		assert.Equal(t, []string{"p/node/restore: the node was put back by hand"}, warned(stateOf(t, j)),
			"what the group's step said went with the reset, and what the recovery step then said stands")
	})
}

// RecordWarning refuses an empty message, one past the step's allowance, and
// a step that has returned.
func TestRecordWarning_Refusals(t *testing.T) {
	t.Parallel()

	var (
		kept        *pipeline.StepContext
		empty, over error
	)

	out := runPlan(t, t.Context(), mustPlan(t, pipeline.Effect("a", pipeline.StepFunc(
		func(_ context.Context, s *pipeline.StepContext) error {
			kept = s
			empty = s.RecordWarning("")

			if err := s.RecordWarning(strings.Repeat("x", pipeline.MaxWarningBytes)); err != nil {
				return err
			}

			over = s.RecordWarning("x")

			return nil
		}))))

	require.NoError(t, out.err)
	require.ErrorContains(t, empty, "warning is empty")
	require.ErrorContains(t, over, "recorded warnings would exceed")
	assert.Len(t, out.state.Warnings(), 1, "a warning that is refused is not recorded")

	require.ErrorIs(t, kept.RecordWarning("late"), pipeline.ErrStepReturned)

	var zero pipeline.StepContext

	require.ErrorIs(t, zero.RecordWarning("x"), pipeline.ErrStepReturned)
}
