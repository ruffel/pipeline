package runbook_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ruffel/pipeline/runbook"
	"github.com/ruffel/pipeline/runbook/runbooktest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// commitStep is an irreversible step: it has no Undo. Run fails while
// runFails is set; Check reports done while done is set, if checked.
type commitStep struct {
	c        *calls
	runFails *bool
}

func (s commitStep) Run(context.Context, *runbook.StepContext) error {
	s.c.add("commit")

	if s.runFails != nil && *s.runFails {
		return errors.New("commit failed")
	}

	return nil
}

type checkedCommit struct {
	commitStep

	done *bool
	err  *error
}

func (s checkedCommit) Check(context.Context, *runbook.StepContext) (bool, error) {
	s.c.add("check commit")

	if s.err != nil && *s.err != nil {
		return false, *s.err
	}

	return *s.done, nil
}

func irreversibleOf(t *testing.T, j *runbook.Journal, path string) runbook.IrreversibleState {
	t.Helper()

	return state(t, j).Irreversible()[path]
}

func TestIrreversible_PlanValidation(t *testing.T) {
	t.Parallel()

	var c calls

	_, err := runbook.NewPlan("p",
		runbook.Ensure("ensure", ok()).Irreversible(),
		runbook.Observe("observe", ok()).Irreversible(),
		runbook.Effect("undoable", undoable{c: &c, name: "u"}).Irreversible(),
	)

	require.Error(t, err)
	require.ErrorContains(t, err, "p/ensure: only an effect step can be irreversible, not ensure")
	require.ErrorContains(t, err, "p/observe: only an effect step can be irreversible, not observe")
	require.ErrorContains(t, err, "p/undoable: an irreversible step may not have Undo")
}

func TestIrreversible_SpecDescribeAndFingerprint(t *testing.T) {
	t.Parallel()

	var c calls

	marked := mustPlan(t, runbook.Effect("commit", commitStep{c: &c}).Irreversible())
	plain := mustPlan(t, runbook.Effect("commit", commitStep{c: &c}))

	assert.True(t, marked.Spec().Children[0].Irreversible)
	assert.Contains(t, runbook.Describe(marked), "commit (effect, irreversible)")
	assert.NotEqual(t, plain.Fingerprint(), marked.Fingerprint())
}

// Once an Irreversible step has passed, Rollback refuses and undoes nothing.
// The refusal is recorded, but Run may go on after it.
func TestIrreversible_PassedRefusesRollback(t *testing.T) {
	t.Parallel()

	var c calls

	fails := true
	plan := mustPlan(t,
		runbook.Effect("backup", undoable{c: &c, name: "backup"}),
		runbook.Effect("commit", commitStep{c: &c}).Irreversible(),
		runbook.Effect("after", undoable{c: &c, name: "after", runFails: &fails}),
	)
	j := runbook.MemoryJournal()

	require.Error(t, resumeOn(t, j, plan).err)
	assert.Equal(t, runbook.IrreversiblePassed, irreversibleOf(t, j, "p/commit"))

	back := resumeRollback(t, j, plan)
	require.ErrorIs(t, back.err, runbook.ErrIrreversible)
	require.ErrorContains(t, back.err, "p/commit")
	assert.Equal(t, runbook.OutcomeFailed, back.res.Outcome)
	assert.Empty(t, kinds(back.events, runbook.EventUndoStarted), "nothing is undone")
	assert.Zero(t, c.get("undo after"))

	fails = false

	require.NoError(t, resumeOn(t, j, plan).err, "a refused rollback doesn't hold up Run")
}

// A step whose Check found its work done is past the point of no return as
// much as one whose Run passed.
func TestIrreversible_DoneByCheckCountsAsPassed(t *testing.T) {
	t.Parallel()

	var c calls

	done := true
	plan := mustPlan(t, runbook.Effect("commit", checkedCommit{commitStep: commitStep{c: &c}, done: &done}).Irreversible())
	j := runbook.MemoryJournal()

	require.NoError(t, resumeOn(t, j, plan).err)
	assert.Zero(t, c.get("commit"))

	_, err := runbook.Rollback(t.Context(), plan, j)
	require.ErrorIs(t, err, runbook.ErrIrreversible)
}

// A step in doubt with no Check refuses a rollback, unless the caller says
// it did not commit; then the rollback goes past it, recording it as not
// committed, and undoes what came before.
func TestIrreversible_InDoubtWithoutCheck(t *testing.T) {
	t.Parallel()

	var c calls

	fails := true
	plan := mustPlan(t,
		runbook.Effect("backup", undoable{c: &c, name: "backup"}),
		runbook.Effect("commit", commitStep{c: &c, runFails: &fails}).Irreversible(),
	)
	j := runbook.MemoryJournal()

	require.Error(t, resumeOn(t, j, plan).err)
	assert.Equal(t, runbook.IrreversibleInDoubt, irreversibleOf(t, j, "p/commit"))

	_, err := runbook.Rollback(t.Context(), plan, j)
	require.ErrorIs(t, err, runbook.ErrIrreversibleInDoubt)
	require.ErrorContains(t, err, "p/commit")
	assert.Zero(t, c.get("undo backup"))

	back := resumeRollback(t, j, plan, runbook.AssumeNotCommitted("p/commit"))
	require.NoError(t, back.err)
	assert.Equal(t, []string{
		"undo.finished p/commit undone/not-committed",
		"undo.started p/backup",
		"undo.finished p/backup undone",
	}, trace(back.events))
	assert.Equal(t, runbook.IrreversibleNotReached, irreversibleOf(t, j, "p/commit"))
}

// A step in doubt with a Check is settled by it: done refuses the rollback
// and records the step as passed; not done lets the rollback go past it.
func TestIrreversible_InDoubtSettledByCheck(t *testing.T) {
	t.Parallel()

	inDoubt := func(t *testing.T, done *bool, checkErr *error) (*runbook.Plan, *runbook.Journal, *calls) {
		t.Helper()

		c := &calls{}
		fails := true
		plan := mustPlan(t,
			runbook.Effect("backup", undoable{c: c, name: "backup"}),
			runbook.Effect("commit", checkedCommit{
				commitStep: commitStep{c: c, runFails: &fails}, done: done, err: checkErr,
			}).Irreversible(),
		)
		j := runbook.MemoryJournal()

		*done = false

		require.Error(t, resumeOn(t, j, plan).err)
		require.Equal(t, runbook.IrreversibleInDoubt, irreversibleOf(t, j, "p/commit"))

		return plan, j, c
	}

	t.Run("committed", func(t *testing.T) {
		t.Parallel()

		done := false
		plan, j, c := inDoubt(t, &done, nil)

		done = true
		back := resumeRollback(t, j, plan)

		require.ErrorIs(t, back.err, runbook.ErrIrreversible)
		assert.Contains(t, trace(back.events), "step.finished p/commit passed/recovered")
		assert.Equal(t, runbook.IrreversiblePassed, irreversibleOf(t, j, "p/commit"))
		assert.Zero(t, c.get("undo backup"))
	})

	t.Run("not committed", func(t *testing.T) {
		t.Parallel()

		done := false
		plan, j, c := inDoubt(t, &done, nil)

		back := resumeRollback(t, j, plan)

		require.NoError(t, back.err)
		assert.Contains(t, trace(back.events), "undo.finished p/commit undone/not-committed")
		assert.Equal(t, 1, c.get("undo backup"))
		assert.Equal(t, runbook.IrreversibleNotReached, irreversibleOf(t, j, "p/commit"))
	})

	t.Run("check fails", func(t *testing.T) {
		t.Parallel()

		done := false

		var checkErr error

		plan, j, c := inDoubt(t, &done, &checkErr)

		checkErr = errors.New("can't tell")
		back := resumeRollback(t, j, plan)

		var stepErr *runbook.StepError

		require.ErrorAs(t, back.err, &stepErr)
		assert.Equal(t, "p/commit", stepErr.Path)
		assert.Zero(t, c.get("undo backup"))
		assert.Equal(t, runbook.IrreversibleInDoubt, irreversibleOf(t, j, "p/commit"))
	})

	t.Run("assumed, without asking the check", func(t *testing.T) {
		t.Parallel()

		done := false
		plan, j, c := inDoubt(t, &done, nil)
		before := c.get("check commit")

		require.NoError(t, resumeRollback(t, j, plan, runbook.AssumeNotCommitted("p/commit")).err)
		assert.Equal(t, before, c.get("check commit"))
	})
}

// An interrupted Irreversible step is in doubt.
func TestIrreversible_InterruptedIsInDoubt(t *testing.T) {
	t.Parallel()

	var c calls

	plan := mustPlan(t, runbook.Effect("commit", commitStep{c: &c}).Irreversible())
	crash := runbooktest.CrashAfter("p/commit", runbooktest.Running)

	_, err := runbook.Run(t.Context(), plan, crash.Journal)
	require.ErrorIs(t, err, runbooktest.ErrCrashed)

	assert.Equal(t, runbook.IrreversibleInDoubt, irreversibleOf(t, crash.Journal, "p/commit"))
}

// A point of no return passed stays passed, even after a recovery handler
// resets the group it is in.
func TestIrreversible_PassedSurvivesAReset(t *testing.T) {
	t.Parallel()

	var c calls

	plan := mustPlan(t,
		runbook.Group("node",
			runbook.Effect("commit", commitStep{c: &c}).Irreversible(),
			runbook.Effect("apply", fail("boom")),
		).RecoverOnFailure(runbook.Effect("recover", ok())),
	)
	j := runbook.MemoryJournal()

	require.Error(t, resumeOn(t, j, plan).err)
	assert.Equal(t, runbook.StatusNotReached, state(t, j).Element("p/node/commit").Status, "the reset forgot the step")
	assert.Equal(t, runbook.IrreversiblePassed, irreversibleOf(t, j, "p/node/commit"))

	_, err := runbook.Rollback(t.Context(), plan, j)
	require.ErrorIs(t, err, runbook.ErrIrreversible)
}

// A step never reached is not in the way, and its state says so.
func TestIrreversible_NotReached(t *testing.T) {
	t.Parallel()

	var c calls

	plan := mustPlan(t,
		runbook.Effect("backup", undoable{c: &c, name: "backup"}),
		runbook.Effect("boom", fail("boom")),
		runbook.Effect("commit", commitStep{c: &c}).Irreversible(),
	)
	j := runbook.MemoryJournal()

	require.Error(t, resumeOn(t, j, plan).err)
	assert.Equal(t, map[string]runbook.IrreversibleState{"p/commit": runbook.IrreversibleNotReached},
		state(t, j).Irreversible())

	require.NoError(t, resumeRollback(t, j, plan).err)
	assert.Equal(t, 1, c.get("undo backup"))
}

// A forced run that calls a passed step's Run again, and fails it, doesn't
// put the step back in doubt: the earlier pass committed it.
func TestIrreversible_PassedSurvivesALaterRun(t *testing.T) {
	t.Parallel()

	var c calls

	fails := false
	plan := mustPlan(t, runbook.Effect("commit", commitStep{c: &c, runFails: &fails}).Irreversible())
	j := runbook.MemoryJournal()

	require.NoError(t, resumeOn(t, j, plan).err)

	fails = true

	require.Error(t, resumeOn(t, j, plan, runbook.Force()).err)
	assert.Equal(t, 2, c.get("commit"))
	assert.Equal(t, runbook.IrreversiblePassed, irreversibleOf(t, j, "p/commit"))

	_, err := runbook.Rollback(t.Context(), plan, j, runbook.AssumeNotCommitted("p/commit"))
	require.ErrorIs(t, err, runbook.ErrIrreversible)
}
