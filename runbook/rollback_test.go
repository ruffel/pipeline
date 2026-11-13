package runbook_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ruffel/pipeline/runbook"
	"github.com/ruffel/pipeline/runbook/runbooktest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// undoable is a step whose Run records the version it replaced and whose
// Undo puts it back. Run fails while runFails is set, Undo while undoFails
// is set.
type undoable struct {
	c         *calls
	name      string
	runFails  *bool
	undoFails *bool
	undo      func(ctx context.Context, sc *runbook.StepContext) error
	seen      *sync.Map // what each Undo found recorded, by name
}

func (u undoable) Run(_ context.Context, sc *runbook.StepContext) error {
	u.c.add(u.name)

	if err := sc.Record("previous", u.name+"-old"); err != nil {
		return err
	}

	if u.runFails != nil && *u.runFails {
		return errors.New(u.name + " failed")
	}

	return nil
}

func (u undoable) Undo(ctx context.Context, sc *runbook.StepContext) error {
	u.c.add("undo " + u.name)

	if u.seen != nil {
		var prev string

		if ok, err := sc.Recorded(sc.Path(), "previous", &prev); err == nil && ok {
			u.seen.Store(u.name, prev)
		}
	}

	if u.undo != nil {
		return u.undo(ctx, sc)
	}

	if u.undoFails != nil && *u.undoFails {
		return errors.New("undo " + u.name + " failed")
	}

	return nil
}

// checkedUndoable is an undoable step with a Check that finds it done.
type checkedUndoable struct{ undoable }

func (checkedUndoable) Check(context.Context, *runbook.StepContext) (bool, error) { return true, nil }

// Rollback undoes, in reverse plan order, every Effect and Ensure step whose
// Run was called, failed ones included. A step without Undo is reported and
// keeps its outcome; Observe steps are left alone.
func TestRollback_UndoesWhatRanInReversePlanOrder(t *testing.T) {
	t.Parallel()

	var c calls

	fails := true
	plan := mustPlan(t,
		runbook.Effect("a", undoable{c: &c, name: "a"}),
		runbook.Group("g",
			runbook.Ensure("b", undoable{c: &c, name: "b"}),
			runbook.Effect("c", ok()),
		),
		runbook.Observe("d", ok()),
		runbook.Effect("e", undoable{c: &c, name: "e", runFails: &fails}),
	)
	j := runbook.MemoryJournal()

	require.Error(t, resumeOn(t, j, plan).err)

	back, err := runbook.Rollback(t.Context(), plan, j)
	require.NoError(t, err)
	assert.Equal(t, runbook.OutcomePassed, back.Outcome)

	events, err := j.Events()
	require.NoError(t, err)

	rollback := kinds(events, runbook.EventRunStarted)[1]
	assert.True(t, rollback.Rollback)
	assert.Equal(t, []string{
		"undo.started p/e",
		"undo.finished p/e undone",
		"undo.finished p/g/c skipped/no-undo",
		"undo.started p/g/b",
		"undo.finished p/g/b undone",
		"undo.started p/a",
		"undo.finished p/a undone",
	}, trace(eventsOf(events, rollback.Run)))

	s := state(t, j)
	assert.True(t, s.Attempts[1].Rollback)
	assert.False(t, s.Unfinished())

	for _, path := range []string{"p/a", "p/g/b", "p/e"} {
		el := s.Element(path)
		assert.Equal(t, runbook.StatusNotReached, el.Status, path)
		assert.False(t, el.RunCalled, path)

		_, recorded := s.Value(path, "previous")
		assert.False(t, recorded, "%s's values are forgotten", path)
	}

	assert.Equal(t, runbook.StatusPassed, s.Element("p/g/c").Status, "a step without Undo keeps its outcome")
	assert.False(t, s.Element("p/g").Done, "a group with an undone step is no longer settled")
	assert.Equal(t, runbook.StatusPassed, s.Element("p/d").Status)
}

// eventsOf returns the events the attempt run wrote.
func eventsOf(events []runbook.Event, run string) []runbook.Event {
	var out []runbook.Event

	for _, e := range events {
		if e.Run == run {
			out = append(out, e)
		}
	}

	return out
}

// A step whose Check found the work done was never Run, so it isn't undone.
func TestRollback_LeavesWhatCheckFoundDone(t *testing.T) {
	t.Parallel()

	var c calls

	plan := mustPlan(t,
		runbook.Effect("done", checkedUndoable{undoable{c: &c, name: "done"}}),
		runbook.Effect("ran", undoable{c: &c, name: "ran"}),
	)
	j := runbook.MemoryJournal()

	require.NoError(t, resumeOn(t, j, plan).err)

	back := resumeRollback(t, j, plan)
	require.NoError(t, back.err)
	assert.Zero(t, c.get("undo done"))
	assert.Equal(t, 1, c.get("undo ran"))
	assert.NotContains(t, strings.Join(trace(back.events), "\n"), "p/done")
}

// resumeRollback makes one rollback of plan against j.
func resumeRollback(t *testing.T, j *runbook.Journal, plan *runbook.Plan, opts ...runbook.Option) attemptResult {
	t.Helper()

	before, err := j.Events()
	require.NoError(t, err)

	res, runErr := runbook.Rollback(t.Context(), plan, j, opts...)

	after, err := j.Events()
	require.NoError(t, err)

	return attemptResult{res: res, err: runErr, events: after[len(before):]}
}

// Undo can read what an interrupted Run recorded before it was cut off.
func TestRollback_UndoReadsWhatAnInterruptedRunRecorded(t *testing.T) {
	t.Parallel()

	var (
		c    calls
		seen sync.Map
	)

	plan := mustPlan(t, runbook.Effect("packages", undoable{c: &c, name: "packages", seen: &seen}))
	crash := runbooktest.CrashAfter("p/packages", runbooktest.Value("previous"))

	_, err := runbook.Run(t.Context(), plan, crash.Journal)
	require.ErrorIs(t, err, runbooktest.ErrCrashed)

	crash.Restart()

	require.NoError(t, resumeRollback(t, crash.Journal, plan).err)

	prev, ok := seen.Load("packages")
	require.True(t, ok)
	assert.Equal(t, "packages-old", prev)
}

// A rollback stops at the first Undo that fails. Run is refused until a
// rollback passes, and the next rollback carries on, undoing nothing twice.
func TestRollback_StopsAtAFailedUndoAndCarriesOn(t *testing.T) {
	t.Parallel()

	var c calls

	fails := true
	plan := mustPlan(t,
		runbook.Effect("a", undoable{c: &c, name: "a"}),
		runbook.Effect("b", undoable{c: &c, name: "b", undoFails: &fails}),
		runbook.Effect("c", undoable{c: &c, name: "c"}),
	)
	j := runbook.MemoryJournal()

	require.NoError(t, resumeOn(t, j, plan).err)

	first := resumeRollback(t, j, plan)

	var stepErr *runbook.StepError

	require.ErrorAs(t, first.err, &stepErr)
	assert.Equal(t, "p/b", stepErr.Path)
	assert.Equal(t, runbook.OutcomeFailed, first.res.Outcome)
	assert.Zero(t, c.get("undo a"), "nothing before the failure in plan order is undone")
	assert.True(t, state(t, j).Unfinished())

	_, err := runbook.Run(t.Context(), plan, j)
	require.ErrorIs(t, err, runbook.ErrRollbackUnfinished)

	fails = false
	second := resumeRollback(t, j, plan)

	require.NoError(t, second.err)
	assert.Equal(t, []string{
		"undo.started p/b",
		"undo.finished p/b undone",
		"undo.started p/a",
		"undo.finished p/a undone",
	}, trace(second.events))
	assert.Equal(t, 1, c.get("undo c"))
	assert.Equal(t, 2, c.get("undo b"))
	assert.Equal(t, 1, c.get("undo a"))
}

// A rollback that crashes carries on from the last undo it recorded.
func TestRollback_ResumesAfterACrash(t *testing.T) {
	t.Parallel()

	var c calls

	plan := mustPlan(t,
		runbook.Effect("a", undoable{c: &c, name: "a"}),
		runbook.Effect("b", undoable{c: &c, name: "b"}),
	)
	crash := runbooktest.CrashAfter("p/b", runbooktest.UndoFinished)

	require.NoError(t, resumeOn(t, crash.Journal, plan).err)

	_, err := runbook.Rollback(t.Context(), plan, crash.Journal)
	require.ErrorIs(t, err, runbooktest.ErrCrashed)

	crash.Restart()

	second := resumeRollback(t, crash.Journal, plan)
	require.NoError(t, second.err)
	assert.Equal(t, []string{"undo.started p/a", "undo.finished p/a undone"}, trace(second.events))
	assert.Equal(t, 1, c.get("undo b"))
}

// After a rollback passes, Run does the work again: groups are entered
// again and gates wait again, though an earlier run passed.
func TestRollback_ThenRunDoesItAgain(t *testing.T) {
	t.Parallel()

	var c calls

	plan := mustPlan(t,
		runbook.Group("g", runbook.Effect("a", undoable{c: &c, name: "a"})),
		runbook.Gate("go", runbook.GateOptions{Approvers: []runbook.Approver{approver{
			name: "test", wait: func(context.Context, runbook.GateInfo) (string, error) {
				c.add("asked")

				return "alice", nil
			},
		}}}),
	)
	j := runbook.MemoryJournal()

	require.NoError(t, resumeOn(t, j, plan).err)
	require.NoError(t, resumeRollback(t, j, plan).err)

	again := resumeOn(t, j, plan)
	require.NoError(t, again.err)
	assert.Equal(t, 2, c.get("a"))
	assert.Equal(t, 2, c.get("asked"))
}

// A stop or cancel keeps the next Undo from starting. A stopped rollback is
// unfinished, and the next one carries on.
func TestRollback_StopAndCancel(t *testing.T) {
	t.Parallel()

	t.Run("stop", func(t *testing.T) {
		t.Parallel()

		var c calls

		stopper := runbook.NewStopper()
		plan := mustPlan(t,
			runbook.Effect("a", undoable{c: &c, name: "a"}),
			runbook.Effect("b", undoable{c: &c, name: "b", undo: func(context.Context, *runbook.StepContext) error {
				stopper.Stop("alice")

				return nil
			}}),
		)
		j := runbook.MemoryJournal()

		require.NoError(t, resumeOn(t, j, plan).err)

		first := resumeRollback(t, j, plan, runbook.WithStopper(stopper))
		assert.Equal(t, runbook.ErrStopped, first.err)
		assert.Equal(t, runbook.OutcomeStopped, first.res.Outcome)
		assert.Zero(t, c.get("undo a"))

		_, err := runbook.Run(t.Context(), plan, j)
		require.ErrorIs(t, err, runbook.ErrRollbackUnfinished)

		require.NoError(t, resumeRollback(t, j, plan).err)
		assert.Equal(t, 1, c.get("undo a"))
		assert.Equal(t, 1, c.get("undo b"))
	})

	t.Run("cancel", func(t *testing.T) {
		t.Parallel()

		var c calls

		ctx, cancel := context.WithCancel(t.Context())
		plan := mustPlan(t,
			runbook.Effect("a", undoable{c: &c, name: "a"}),
			runbook.Effect("b", undoable{c: &c, name: "b", undo: func(context.Context, *runbook.StepContext) error {
				cancel()

				return nil
			}}),
		)
		j := runbook.MemoryJournal()

		require.NoError(t, resumeOn(t, j, plan).err)

		res, err := runbook.Rollback(ctx, plan, j)
		require.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, runbook.OutcomeCancelled, res.Outcome)
		assert.Zero(t, c.get("undo a"))
	})
}

// A panicking Undo fails as a panic, and a step's Timeout bounds its Undo.
func TestRollback_PanicAndTimeout(t *testing.T) {
	t.Parallel()

	var c calls

	plan := mustPlan(t,
		runbook.Effect("slow", undoable{c: &c, name: "slow", undo: func(ctx context.Context, _ *runbook.StepContext) error {
			<-ctx.Done()

			return ctx.Err()
		}}).Timeout(20*time.Millisecond),
		runbook.Effect("panics", undoable{c: &c, name: "panics", undo: func(context.Context, *runbook.StepContext) error {
			panic("boom")
		}}),
	)
	j := runbook.MemoryJournal()

	require.NoError(t, resumeOn(t, j, plan).err)

	first := resumeRollback(t, j, plan)
	require.Error(t, first.err)
	assert.Equal(t, runbook.ErrorPanic, kinds(first.events, runbook.EventUndoFinished)[0].Error.Kind)

	// Let the panicking step's Undo pass, to reach the slow one.
	j2 := runbook.MemoryJournal()
	slowOnly := mustPlan(t, runbook.Effect("slow", undoable{c: &c, name: "slow", undo: func(ctx context.Context, _ *runbook.StepContext) error {
		<-ctx.Done()

		return ctx.Err()
	}}).Timeout(20*time.Millisecond))

	require.NoError(t, resumeOn(t, j2, slowOnly).err)

	second := resumeRollback(t, j2, slowOnly)
	require.ErrorIs(t, second.err, context.DeadlineExceeded)
	assert.Equal(t, runbook.ErrorTimeout, kinds(second.events, runbook.EventUndoFinished)[0].Error.Kind)
	assert.Equal(t, runbook.OutcomeFailed, second.res.Outcome)
}

// A rollback refuses a changed plan, and of nothing simply passes.
func TestRollback_PlanAndEmptyJournal(t *testing.T) {
	t.Parallel()

	var c calls

	j := runbook.MemoryJournal()
	require.NoError(t, resumeOn(t, j, mustPlan(t, runbook.Effect("a", undoable{c: &c, name: "a"}))).err)

	_, err := runbook.Rollback(t.Context(), mustPlan(t, runbook.Effect("other", ok())), j)
	require.ErrorIs(t, err, runbook.ErrPlanChanged)

	res, err := runbook.Rollback(t.Context(), mustPlan(t, runbook.Effect("a", ok())), runbook.MemoryJournal())
	require.NoError(t, err)
	assert.Equal(t, runbook.OutcomePassed, res.Outcome)
}

// A recovery handler's reset takes its group's steps out of what a rollback
// undoes; the handler itself ran, so it is undone.
func TestRollback_LeavesARecoveredGroupToItsHandler(t *testing.T) {
	t.Parallel()

	var c calls

	fails := true
	plan := mustPlan(t,
		runbook.Group("node",
			runbook.Effect("apply", undoable{c: &c, name: "apply", runFails: &fails}),
		).RecoverOnFailure(runbook.Effect("recover", undoable{c: &c, name: "recover"})),
	)
	j := runbook.MemoryJournal()

	require.Error(t, resumeOn(t, j, plan).err)
	require.NoError(t, resumeRollback(t, j, plan).err)

	assert.Zero(t, c.get("undo apply"))
	assert.Equal(t, 1, c.get("undo recover"))
}

// A rollback is given the run's inputs, and Run is happy with them after.
func TestRollback_KeepsTheInputs(t *testing.T) {
	t.Parallel()

	var c calls

	type inputs struct{ Version string }

	plan := mustPlan(t, runbook.Effect("a", undoable{c: &c, name: "a"}))
	j := runbook.MemoryJournal()
	in := runbook.WithInputs(inputs{Version: "8.1"})

	require.NoError(t, resumeOn(t, j, plan, in).err)

	_, err := runbook.Rollback(t.Context(), plan, j)
	require.ErrorIs(t, err, runbook.ErrInputsChanged)

	require.NoError(t, resumeRollback(t, j, plan, in).err)
	require.NoError(t, resumeOn(t, j, plan, in).err)

	var got inputs

	ok, err := state(t, j).DecodeInputs(&got)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "8.1", got.Version)
}

func TestPlainObserver_ARollback(t *testing.T) {
	t.Parallel()

	var (
		c calls
		b strings.Builder
	)

	plan := mustPlan(t,
		runbook.Effect("backup", undoable{c: &c, name: "backup"}),
		runbook.Effect("apply", ok()),
	)
	j := runbook.MemoryJournal()

	require.NoError(t, resumeOn(t, j, plan).err)
	require.NoError(t, resumeRollback(t, j, plan, runbook.WithObservers(runbook.NewPlainObserver(&b))).err)

	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	assert.Equal(t, "▶ p (rollback)", lines[0])
	assert.Equal(t, "  ⊘ apply not undone — no-undo", lines[1])
	assert.True(t, strings.HasPrefix(lines[2], "  ↶ backup undone"), lines[2])
	assert.True(t, strings.HasPrefix(lines[3], "✓ p passed"), lines[3])
}
