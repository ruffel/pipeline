package runbook_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/ruffel/pipeline/runbook"
	"github.com/ruffel/pipeline/runbook/runbooktest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recording is a step that counts its Run calls, records a value, and fails
// while failing is set.
func recording(c *calls, name string, failing *bool) runbook.Step {
	return runbook.StepFunc(func(_ context.Context, sc *runbook.StepContext) error {
		c.add(name)

		if err := sc.Record("ran", true); err != nil {
			return err
		}

		if err := sc.Item("node1", runbook.ItemPassed, ""); err != nil {
			return err
		}

		if failing != nil && *failing {
			return errors.New(name + " failed")
		}

		return nil
	})
}

func state(t *testing.T, j *runbook.Journal) *runbook.State {
	t.Helper()

	s, err := j.State()
	require.NoError(t, err)

	return s
}

// A cleanup handler runs when its group fails, inside the group, and leaves
// the group's record alone: a resume carries on from the failure.
func TestOnFailure_CleansUpAndResumeCarriesOn(t *testing.T) {
	t.Parallel()

	var c calls

	failing := true
	plan := mustPlan(t,
		runbook.Group("g",
			runbook.Effect("a", counted(&c, "a", nil)),
			runbook.Effect("b", counted(&c, "b", &failing)),
		).OnFailure(runbook.Effect("cleanup", counted(&c, "cleanup", nil))),
		runbook.Effect("after", counted(&c, "after", nil)),
	)
	j := runbook.MemoryJournal()

	first := resumeOn(t, j, plan)
	require.ErrorContains(t, first.err, "p/g/b: b failed")
	assert.Equal(t, []string{
		"group.started p/g",
		"step.started p/g/a",
		"step.running p/g/a",
		"step.finished p/g/a passed",
		"step.started p/g/b",
		"step.running p/g/b",
		"step.finished p/g/b failed",
		"step.started p/g/cleanup",
		"step.running p/g/cleanup",
		"step.finished p/g/cleanup passed",
		"group.finished p/g failed",
	}, trace(first.events))
	assert.Zero(t, c.get("after"), "the group still fails")

	failing = false
	second := resumeOn(t, j, plan)

	require.NoError(t, second.err)
	assert.Equal(t, 1, c.get("a"), "a is not redone")
	assert.Equal(t, 2, c.get("b"))
	assert.Equal(t, 1, c.get("cleanup"), "a group that passes is not cleaned up")
	assert.Equal(t, 1, c.get("after"))
}

// A cleanup handler runs every time its group fails, whatever it did before.
func TestOnFailure_RunsOnEveryFailure(t *testing.T) {
	t.Parallel()

	var c calls

	plan := mustPlan(t,
		runbook.Group("g", runbook.Effect("b", fail("boom"))).
			OnFailure(runbook.Effect("cleanup", counted(&c, "cleanup", nil))),
	)
	j := runbook.MemoryJournal()

	require.Error(t, resumeOn(t, j, plan).err)
	require.Error(t, resumeOn(t, j, plan).err)
	assert.Equal(t, 2, c.get("cleanup"))
}

// A recovery handler resets its group as it starts: everything in it goes
// back to never started, recorded values and items too, and a resume runs
// the whole group again.
func TestRecoverOnFailure_ResetsTheGroupAndResumeRedoesIt(t *testing.T) {
	t.Parallel()

	var c calls

	failing := true
	plan := mustPlan(t,
		runbook.Group("g",
			runbook.Effect("a", recording(&c, "a", nil)),
			runbook.Effect("b", counted(&c, "b", &failing)),
		).RecoverOnFailure(runbook.Effect("recover", counted(&c, "recover", nil))),
	)
	j := runbook.MemoryJournal()

	first := resumeOn(t, j, plan)
	require.Error(t, first.err)
	assert.Equal(t, []string{
		"step.finished p/g/b failed",
		"group.reset p/g",
		"step.started p/g/recover",
		"step.running p/g/recover",
		"step.finished p/g/recover passed",
		"group.finished p/g failed",
	}, trace(first.events)[6:])

	s := state(t, j)
	a := s.Element("p/g/a")
	assert.Equal(t, runbook.StatusNotReached, a.Status)
	assert.False(t, a.Done)
	assert.False(t, a.RunCalled, "a later rollback leaves the group to its handler")

	_, recorded := s.Value("p/g/a", "ran")
	assert.False(t, recorded, "values are forgotten")
	assert.Empty(t, s.Items("p/g/a"), "items are forgotten")
	assert.Equal(t, runbook.StatusPassed, s.Element("p/g/recover").Status)
	assert.Equal(t, runbook.StatusFailed, s.Element("p/g").Status)

	failing = false
	second := resumeOn(t, j, plan)

	require.NoError(t, second.err)
	assert.Equal(t, 2, c.get("a"), "the group is redone from the start")
	assert.Equal(t, 1, c.get("recover"))
}

// A handler that fails is reported with its group's failure, and is not run
// again on resume: the next attempt carries on, after a recovery from the
// group's start.
func TestFailureHandlers_AFailedHandlerIsNotOwed(t *testing.T) {
	t.Parallel()

	for _, recovers := range []bool{false, true} {
		var c calls

		failing := true
		group := runbook.Group("g",
			runbook.Effect("a", counted(&c, "a", nil)),
			runbook.Effect("b", counted(&c, "b", &failing)),
		)
		handler := runbook.Effect("handler", fail("handler broke"))

		if recovers {
			group = group.RecoverOnFailure(handler)
		} else {
			group = group.OnFailure(handler)
		}

		plan := mustPlan(t, group)
		j := runbook.MemoryJournal()

		first := resumeOn(t, j, plan)
		require.ErrorContains(t, first.err, "p/g/b: b failed")
		require.ErrorContains(t, first.err, "p/g/handler: handler broke")

		failing = false
		second := resumeOn(t, j, plan)

		require.NoError(t, second.err)
		assert.NotContains(t, trace(second.events), "step.started p/g/handler")

		if recovers {
			assert.Equal(t, 2, c.get("a"), "a failed recovery still redoes the group")
		} else {
			assert.Equal(t, 1, c.get("a"))
		}
	}
}

// A crash inside a handler leaves it unfinished, and it is not finished on
// resume: after a recovery the group is redone, after a cleanup the run
// carries on.
func TestFailureHandlers_ACrashInsideOneIsNotOwed(t *testing.T) {
	t.Parallel()

	for _, recovers := range []bool{false, true} {
		var c calls

		failing := true
		group := runbook.Group("g",
			runbook.Effect("a", counted(&c, "a", nil)),
			runbook.Effect("b", counted(&c, "b", &failing)),
		)
		handler := runbook.Effect("handler", counted(&c, "handler", nil))

		if recovers {
			group = group.RecoverOnFailure(handler)
		} else {
			group = group.OnFailure(handler)
		}

		plan := mustPlan(t, group)
		crash := runbooktest.CrashAfter("p/g/handler", runbooktest.Running)

		_, err := runbook.Run(t.Context(), plan, crash.Journal)
		require.ErrorIs(t, err, runbooktest.ErrCrashed)
		assert.Equal(t, runbook.StatusStarted, state(t, crash.Journal).Element("p/g/handler").Status)

		crash.Restart()

		failing = false
		second := resumeOn(t, crash.Journal, plan)

		require.NoError(t, second.err)
		assert.Equal(t, 1, c.get("handler"), "recovers %v", recovers)

		if recovers {
			assert.Equal(t, 2, c.get("a"))
		} else {
			assert.Equal(t, 1, c.get("a"))
		}
	}
}

// A handler doesn't start once a stop or cancel has been requested, and
// not for a group that was only stopped.
func TestFailureHandlers_NotAfterAStopOrCancel(t *testing.T) {
	t.Parallel()

	handled := func(c *calls) runbook.StepNode {
		return runbook.Effect("handler", counted(c, "handler", nil))
	}

	t.Run("stop requested as the group fails", func(t *testing.T) {
		t.Parallel()

		var c calls

		stopper := runbook.NewStopper()
		b := runbook.StepFunc(func(context.Context, *runbook.StepContext) error {
			stopper.Stop("alice")

			return errors.New("boom")
		})

		out := runPlan(t, t.Context(), mustPlan(t,
			runbook.Group("g", runbook.Effect("b", b)).RecoverOnFailure(handled(&c)),
		), runbook.WithStopper(stopper))

		require.ErrorIs(t, out.err, runbook.ErrStopped)
		require.ErrorContains(t, out.err, "boom")
		assert.Zero(t, c.get("handler"))
		assert.NotContains(t, trace(out.events), "step.started p/g/handler", "the handler doesn't start")
		assert.Empty(t, kinds(out.events, runbook.EventGroupReset))
		assert.Equal(t, runbook.StatusFailed, out.state.Element("p/g").Status)
	})

	t.Run("cancel requested as the group fails", func(t *testing.T) {
		t.Parallel()

		var c calls

		ctx, cancel := context.WithCancel(t.Context())
		b := runbook.StepFunc(func(context.Context, *runbook.StepContext) error {
			cancel()

			return errors.New("boom")
		})

		out := runPlan(t, ctx, mustPlan(t,
			runbook.Group("g", runbook.Effect("b", b)).OnFailure(handled(&c)),
		))

		require.ErrorContains(t, out.err, "boom")
		assert.Equal(t, runbook.OutcomeCancelled, out.res.Outcome)
		assert.Zero(t, c.get("handler"))
		assert.NotContains(t, trace(out.events), "step.started p/g/handler", "the handler doesn't start")
	})

	t.Run("group only stopped", func(t *testing.T) {
		t.Parallel()

		var c calls

		stopper := runbook.NewStopper()
		a := runbook.StepFunc(func(context.Context, *runbook.StepContext) error {
			stopper.Stop("alice")

			return nil
		})

		out := runPlan(t, t.Context(), mustPlan(t,
			runbook.Group("g", runbook.Effect("a", a), runbook.Effect("b", ok())).OnFailure(handled(&c)),
		), runbook.WithStopper(stopper))

		assert.Equal(t, runbook.ErrStopped, out.err)
		assert.Zero(t, c.get("handler"))
		assert.Equal(t, runbook.StatusStopped, out.state.Element("p/g").Status)
	})
}

// A parallel group that cancels its running children on a failure still
// gives its handler a live context.
func TestFailureHandlers_RunAfterAGroupCancelledItsChildren(t *testing.T) {
	t.Parallel()

	var handlerErr error

	handler := runbook.StepFunc(func(ctx context.Context, _ *runbook.StepContext) error {
		handlerErr = ctx.Err()

		return nil
	})

	out := runPlan(t, t.Context(), mustPlan(t,
		runbook.Group("g",
			runbook.Effect("boom", fail("boom")),
			runbook.Effect("wait", waitForCancel()),
		).Parallel(0).CancelOnError().OnFailure(runbook.Effect("handler", handler)),
	))

	require.ErrorContains(t, out.err, "boom")
	assert.Equal(t, runbook.StatusPassed, out.state.Element("p/g/handler").Status)
	assert.NoError(t, handlerErr)
}

// Nested groups each handle their own failure, innermost first.
func TestFailureHandlers_Nested(t *testing.T) {
	t.Parallel()

	out := runPlan(t, t.Context(), mustPlan(t,
		runbook.Group("outer",
			runbook.Group("inner", runbook.Effect("b", fail("boom"))).
				RecoverOnFailure(runbook.Effect("recover", ok())),
		).OnFailure(runbook.Effect("cleanup", ok())),
	))

	require.Error(t, out.err)
	assert.Equal(t, []string{
		"group.reset p/outer/inner",
		"step.finished p/outer/inner/recover passed",
		"group.finished p/outer/inner failed",
		"step.finished p/outer/cleanup passed",
		"group.finished p/outer failed",
	}, slicesFilter(trace(out.events), func(l string) bool {
		return strings.HasPrefix(l, "group.reset") || strings.HasPrefix(l, "group.finished") ||
			strings.HasPrefix(l, "step.finished p/outer/inner/recover") || strings.HasPrefix(l, "step.finished p/outer/cleanup")
	}))
}

func slicesFilter(lines []string, keep func(string) bool) []string {
	var out []string

	for _, l := range lines {
		if keep(l) {
			out = append(out, l)
		}
	}

	return out
}

func TestFailureHandlers_PlanValidation(t *testing.T) {
	t.Parallel()

	_, err := runbook.NewPlan("p",
		runbook.Group("both", runbook.Effect("a", ok())).
			OnFailure(runbook.Effect("x", ok())).RecoverOnFailure(runbook.Effect("y", ok())),
		runbook.Group("kind", runbook.Effect("a", ok())).OnFailure(runbook.Ensure("x", ok())),
		runbook.Group("clash", runbook.Effect("a", ok())).OnFailure(runbook.Effect("a", ok())),
		runbook.Group("nil", runbook.Effect("a", ok())).RecoverOnFailure(runbook.Effect("x", nil)),
		runbook.Group("name", runbook.Effect("a", ok())).OnFailure(runbook.Effect("bad name", ok())),
	)

	require.Error(t, err)
	require.ErrorContains(t, err, "p/both: a group has one failure handler")
	require.ErrorContains(t, err, "p/kind/x: a failure handler must be an effect step, not ensure")
	require.ErrorContains(t, err, "p/clash/a: a failure handler may not share its name")
	require.ErrorContains(t, err, "p/nil/x: step is nil")
	require.ErrorContains(t, err, "p/name/bad name:")
}

func TestFailureHandlers_SpecDescribeAndFingerprint(t *testing.T) {
	t.Parallel()

	plain := mustPlan(t, runbook.Group("node", runbook.Effect("apply", ok())))
	handled := mustPlan(t, runbook.Group("node", runbook.Effect("apply", ok())).
		RecoverOnFailure(runbook.Effect("recover", ok()).Description("Put the old version back")))
	cleaned := mustPlan(t, runbook.Group("node", runbook.Effect("apply", ok())).
		OnFailure(runbook.Effect("cleanup", ok())))

	assert.NotEqual(t, plain.Fingerprint(), handled.Fingerprint())

	spec := handled.Spec().Children[0]
	require.NotNil(t, spec.OnFailure)
	assert.Equal(t, "p/node/recover", spec.OnFailure.Path)
	assert.Equal(t, runbook.KindEffect, spec.OnFailure.Kind)
	assert.True(t, spec.Recovers)
	assert.Len(t, spec.Children, 1, "the handler is not one of the children")

	assert.Equal(t, `p
└── node
    ├── apply (effect)
    └── recover (effect, recovers the group on failure) — Put the old version back
`, runbook.Describe(handled))
	assert.Contains(t, runbook.Describe(cleaned), "└── cleanup (effect, cleans up on failure)")
}

func TestPlainObserver_AGroupReset(t *testing.T) {
	t.Parallel()

	var b strings.Builder

	out := runPlan(t, t.Context(), mustPlan(t,
		runbook.Group("node", runbook.Effect("apply", fail("boom"))).
			RecoverOnFailure(runbook.Effect("recover", ok())),
	), runbook.WithObservers(runbook.NewPlainObserver(&b)))

	require.Error(t, out.err)
	assert.Contains(t, b.String(), "  ↺ node reset: a resume runs it again from the start\n")
	assert.Contains(t, b.String(), "    ✓ recover")
}

// A plan's root can have a failure handler: cleanup when the run fails, or
// a recovery that resets the whole run.
func TestNewPlanFromGroup_ARootHandler(t *testing.T) {
	t.Parallel()

	t.Run("cleanup", func(t *testing.T) {
		t.Parallel()

		var c calls

		plan, err := runbook.NewPlanFromGroup(runbook.Group("upgrade",
			runbook.Effect("a", counted(&c, "a", nil)),
			runbook.Effect("b", fail("boom")),
		).OnFailure(runbook.Effect("repair-on", counted(&c, "repair-on", nil))))
		require.NoError(t, err)

		out := runPlan(t, t.Context(), plan)

		require.ErrorContains(t, out.err, "upgrade/b: boom")
		assert.Equal(t, runbook.OutcomeFailed, out.res.Outcome)
		assert.Equal(t, []string{
			"step.finished upgrade/b failed",
			"step.started upgrade/repair-on",
			"step.running upgrade/repair-on",
			"step.finished upgrade/repair-on passed",
		}, trace(out.events)[5:])
		assert.Equal(t, 1, c.get("repair-on"))
		assert.True(t, out.state.Element("upgrade/a").Done, "cleanup leaves the record alone")
	})

	t.Run("recovery", func(t *testing.T) {
		t.Parallel()

		var c calls

		failing := true
		plan, err := runbook.NewPlanFromGroup(runbook.Group("upgrade",
			runbook.Effect("a", counted(&c, "a", nil)),
			runbook.Effect("b", counted(&c, "b", &failing)),
		).RecoverOnFailure(runbook.Effect("restore", counted(&c, "restore", nil))))
		require.NoError(t, err)

		j := runbook.MemoryJournal()

		first := resumeOn(t, j, plan)
		require.Error(t, first.err)
		assert.Contains(t, trace(first.events), "group.reset upgrade")
		assert.False(t, state(t, j).Element("upgrade/a").Done)

		failing = false
		second := resumeOn(t, j, plan)

		require.NoError(t, second.err)
		assert.Equal(t, 2, c.get("a"), "the whole run is redone")
		assert.Equal(t, 1, c.get("restore"))
	})

	t.Run("passing run", func(t *testing.T) {
		t.Parallel()

		var c calls

		plan, err := runbook.NewPlanFromGroup(runbook.Group("upgrade", runbook.Effect("a", ok())).
			OnFailure(runbook.Effect("cleanup", counted(&c, "cleanup", nil))))
		require.NoError(t, err)

		require.NoError(t, runPlan(t, t.Context(), plan).err)
		assert.Zero(t, c.get("cleanup"))
	})
}

// The root group's other settings apply to the whole run.
func TestNewPlanFromGroup_RootSettings(t *testing.T) {
	t.Parallel()

	labels := make(chan runbook.L, 2)
	step := runbook.StepFunc(func(_ context.Context, sc *runbook.StepContext) error {
		labels <- sc.Labels()

		return nil
	})

	plan, err := runbook.NewPlanFromGroup(runbook.Group("upgrade",
		runbook.Effect("a", step),
		runbook.Effect("b", step),
	).Parallel(2).ContinueOnError().Labels(runbook.L{"cluster": "c1"}).Description("Upgrade the cluster"))
	require.NoError(t, err)

	assert.Equal(t, `upgrade (parallel, limit 2, continue on error) — Upgrade the cluster
├── a (effect)
└── b (effect)
`, runbook.Describe(plan))

	require.NoError(t, runPlan(t, t.Context(), plan).err)
	assert.Equal(t, runbook.L{"cluster": "c1"}, <-labels)

	_, err = runbook.NewPlanFromGroup(runbook.Group("bad name", runbook.Effect("a", nil)))
	require.ErrorContains(t, err, "bad name:")
	require.ErrorContains(t, err, "bad name/a: step is nil")
}

// A step moved between a group's children and its failure handler, or a
// handler changed between cleanup and recovery, changes the plan's shape.
func TestFailureHandlers_ChangeTheFingerprint(t *testing.T) {
	t.Parallel()

	child := mustPlan(t, runbook.Group("g", runbook.Effect("a", ok()), runbook.Effect("h", ok())))
	cleanup := mustPlan(t, runbook.Group("g", runbook.Effect("a", ok())).OnFailure(runbook.Effect("h", ok())))
	recovery := mustPlan(t, runbook.Group("g", runbook.Effect("a", ok())).RecoverOnFailure(runbook.Effect("h", ok())))

	assert.NotEqual(t, child.Fingerprint(), cleanup.Fingerprint())
	assert.NotEqual(t, child.Fingerprint(), recovery.Fingerprint())
	assert.NotEqual(t, cleanup.Fingerprint(), recovery.Fingerprint())
}

// A handler's failure is recorded on its group, as the group returns it.
func TestFailureHandlers_AFailedHandlerIsRecordedOnItsGroup(t *testing.T) {
	t.Parallel()

	out := runPlan(t, t.Context(), mustPlan(t,
		runbook.Group("g", runbook.Effect("b", fail("boom"))).
			OnFailure(runbook.Effect("cleanup", fail("cleanup broke"))),
	))

	require.Error(t, out.err)

	g := out.state.Element("p/g")
	require.NotNil(t, g.Error)
	assert.Contains(t, g.Error.Message, "p/g/b: boom")
	assert.Contains(t, g.Error.Message, "p/g/cleanup: cleanup broke")
}

// A plan without failure handlers keeps the fingerprint it always had: the
// kind and path of every element, in plan order.
func TestFingerprint_UnchangedWithoutHandlers(t *testing.T) {
	t.Parallel()

	plan := mustPlan(t,
		runbook.Group("g", runbook.Effect("a", ok()), runbook.Observe("b", ok())),
		runbook.Gate("go", runbook.GateOptions{}),
	)

	sum := sha256.Sum256([]byte("group p\ngroup p/g\neffect p/g/a\nobserve p/g/b\ngate p/go\n"))
	assert.Equal(t, "sha256:"+hex.EncodeToString(sum[:]), plan.Fingerprint())
}
