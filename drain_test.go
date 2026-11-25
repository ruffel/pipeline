package pipeline_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ruffel/pipeline"
	"github.com/ruffel/pipeline/pipelinetest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nodeSteps are the steps of one node's upgrade, in order.
var nodeSteps = []string{"stop", "install", "start", "verify"} //nolint:gochecknoglobals // Read-only test data.

// bounded gives a step the timeout that everything in a DrainOnStop group
// needs.
func bounded(s pipeline.StepNode) pipeline.StepNode { return s.Timeout(time.Minute) }

// upgrade is two nodes, each a group of nodeSteps, with a health check
// between them and a step after. Each step counts its runs as "node/step".
// during names steps that do something more when they run.
func upgrade(t *testing.T, c *calls, drain bool, during map[string]func()) *pipeline.Plan {
	t.Helper()

	node := func(name string) pipeline.GroupNode {
		steps := make([]pipeline.Element, 0, len(nodeSteps))

		for _, step := range nodeSteps {
			id := name + "/" + step

			steps = append(steps, bounded(pipeline.Effect(step, pipeline.StepFunc(
				func(context.Context, *pipeline.StepContext) error {
					c.add(id)

					if fn := during[id]; fn != nil {
						fn()
					}

					return nil
				}))))
		}

		g := pipeline.Group(name, steps...)
		if drain {
			g = g.DrainOnStop()
		}

		return g
	}

	return mustPlan(t,
		node("node1"),
		pipeline.Observe("healthy", counted(c, "healthy", nil)),
		node("node2"),
		pipeline.Effect("after", counted(c, "after", nil)),
	)
}

// ran returns how often each of a node's steps ran, in order.
func ran(c *calls, node string) []int {
	out := make([]int, 0, len(nodeSteps))

	for _, step := range nodeSteps {
		out = append(out, c.get(node+"/"+step))
	}

	return out
}

// draining replays each prefix of events, and returns what Draining said
// after each event that changed its answer.
func draining(t *testing.T, events []pipeline.Event) []string {
	t.Helper()

	var (
		out  []string
		last string
	)

	for i := range events {
		state, err := pipeline.Replay(events[:i+1])
		require.NoError(t, err)

		if now := strings.Join(state.Draining(), ","); now != last {
			out = append(out, string(events[i].Kind)+" "+events[i].Path+": "+now)
			last = now
		}
	}

	return out
}

// A stop during any step of a node lets the node finish, and nothing after
// it starts. Without the option the same stop ends the node where it is.
func TestDrain_AStopLetsTheGroupFinishAndStartsNothingAfterIt(t *testing.T) {
	t.Parallel()

	for _, at := range nodeSteps {
		t.Run("during "+at, func(t *testing.T) {
			t.Parallel()

			var c calls

			stopper := pipeline.NewStopper()
			plan := upgrade(t, &c, true, map[string]func(){
				"node1/" + at: func() { stopper.Stop("alice") },
			})

			j := pipeline.MemoryJournal()
			first := resumeOn(t, j, plan, pipeline.WithStopper(stopper))

			require.Equal(t, pipeline.ErrStopped, first.err, "nothing failed, so the stop is the whole error")
			assert.Equal(t, pipeline.OutcomeStopped, first.res.Outcome)
			assert.Equal(t, []int{1, 1, 1, 1}, ran(&c, "node1"), "the node finishes")
			assert.Equal(t, []int{0, 0, 0, 0}, ran(&c, "node2"), "the next node does not start")
			assert.Zero(t, c.get("healthy"))
			assert.Zero(t, c.get("after"))

			lines := trace(first.events)
			assert.Contains(t, lines, "group.finished p/node1 passed")
			assert.NotContains(t, lines, "group.started p/node2")

			state, err := j.State()
			require.NoError(t, err)
			assert.True(t, state.Element("p/node1").Done)
			assert.Equal(t, pipeline.StatusNotReached, state.Element("p/node2").Status)
			assert.Equal(t, pipeline.StatusStopped, state.Element("p").Status)
			assert.Empty(t, state.Draining(), "the attempt has finished")

			// The run says why steps still started after the request, unless
			// the stop came during the last of them.
			said := kinds(first.events, pipeline.EventMessage)

			if at == "verify" {
				assert.Empty(t, said)
			} else {
				require.Len(t, said, 1)
				assert.Equal(t, "p/node1", said[0].Path)
				assert.Equal(t, "a stop was requested: this group finishes first", said[0].Message)
			}

			// The stop was that attempt's. The next one carries on: the
			// finished node is settled, and what is outside it is checked.
			second := resumeOn(t, j, plan)

			require.NoError(t, second.err)
			assert.Equal(t, []int{1, 1, 1, 1}, ran(&c, "node1"), "a settled group is not entered")
			assert.Equal(t, []int{1, 1, 1, 1}, ran(&c, "node2"))
			assert.Equal(t, 1, c.get("healthy"))
			assert.Equal(t, 1, c.get("after"))
			assert.Contains(t, trace(second.events), "group.finished p/node1 skipped/settled")
		})
	}
}

// The option is what makes the difference: an ordinary group is stopped
// before its next step, as it always was.
func TestDrain_AnOrdinaryGroupIsStillStoppedBeforeItsNextStep(t *testing.T) {
	t.Parallel()

	var c calls

	stopper := pipeline.NewStopper()
	plan := upgrade(t, &c, false, map[string]func(){
		"node1/stop": func() { stopper.Stop("alice") },
	})

	first := resumeOn(t, pipeline.MemoryJournal(), plan, pipeline.WithStopper(stopper))

	require.Equal(t, pipeline.ErrStopped, first.err)
	assert.Equal(t, []int{1, 0, 0, 0}, ran(&c, "node1"))
	assert.Contains(t, trace(first.events), "group.finished p/node1 stopped")
	assert.Empty(t, kinds(first.events, pipeline.EventMessage))

	for _, e := range first.events {
		assert.Equal(t, 1, e.V, "nothing in the plan needs a newer version")
	}
}

// A stop that is accepted before the group is entered keeps it out, like any
// element. One that comes once the group is entered, even before its first
// step has started, finds it under way.
func TestDrain_AStopBeforeOrAfterTheGroupIsEntered(t *testing.T) {
	t.Parallel()

	t.Run("before", func(t *testing.T) {
		t.Parallel()

		var c calls

		stopper := pipeline.NewStopper()
		plan := mustPlan(t,
			pipeline.Effect("prepare", stopsWith(stopper)),
			pipeline.Group("node1", bounded(pipeline.Effect("stop", counted(&c, "stop", nil)))).DrainOnStop(),
		)

		out := resumeOn(t, pipeline.MemoryJournal(), plan, pipeline.WithStopper(stopper))

		require.Equal(t, pipeline.ErrStopped, out.err)
		assert.Zero(t, c.get("stop"))
		assert.NotContains(t, trace(out.events), "group.started p/node1")
		assert.Empty(t, draining(t, out.events), "a group that never started is not waited for")
	})

	t.Run("before the run", func(t *testing.T) {
		t.Parallel()

		var c calls

		stopper := pipeline.NewStopper()
		stopper.Stop("alice")

		out := resumeOn(t, pipeline.MemoryJournal(), upgrade(t, &c, true, nil), pipeline.WithStopper(stopper))

		require.Equal(t, pipeline.ErrStopped, out.err)
		assert.Equal(t, []int{0, 0, 0, 0}, ran(&c, "node1"))
	})

	t.Run("after", func(t *testing.T) {
		t.Parallel()

		var c calls

		// group.started is written once the group is entered, and before its
		// first step starts: a stop made from there has lost the race.
		stopper := pipeline.NewStopper()
		entered := pipeline.ObserverFunc(func(_ context.Context, e pipeline.Event) {
			if e.Kind == pipeline.EventGroupStarted && e.Path == "p/node1" {
				stopper.Stop("alice")
			}
		})

		out := resumeOn(t, pipeline.MemoryJournal(), upgrade(t, &c, true, nil),
			pipeline.WithStopper(stopper), pipeline.WithObservers(entered))

		require.Equal(t, pipeline.ErrStopped, out.err)
		assert.Equal(t, []int{1, 1, 1, 1}, ran(&c, "node1"))
		assert.Equal(t, []int{0, 0, 0, 0}, ran(&c, "node2"))
	})
}

// A status command reads which groups a stop is waiting for: from the
// request until the group ends.
func TestDrain_StateSaysWhichGroupsAStopIsWaitingFor(t *testing.T) {
	t.Parallel()

	var c calls

	stopper := pipeline.NewStopper()
	plan := upgrade(t, &c, true, map[string]func(){
		"node1/install": func() { stopper.Stop("alice") },
	})

	out := resumeOn(t, pipeline.MemoryJournal(), plan, pipeline.WithStopper(stopper))
	require.Equal(t, pipeline.ErrStopped, out.err)

	// The attempt has no outcome until run.finished, so the last word is
	// that event's: the group had already ended by then.
	assert.Equal(t, []string{"stop.requested p: p/node1", "group.finished p/node1: "}, draining(t, out.events))

	state, err := pipeline.Replay(out.events)
	require.NoError(t, err)
	require.NotNil(t, state.Plan)
	assert.True(t, state.Plan.Children[0].DrainOnStop)
	assert.False(t, state.Plan.Children[1].DrainOnStop)
}

// Only a stop is put off. A step that fails, panics or passes its timeout
// ends the group there, with a stop pending or not, and the run has failed:
// it is not reported as stopped at a safe place.
func TestDrain_AFailureEndsTheGroupAtOnce(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		start pipeline.Step
		want  string
	}{
		"an error": {start: fail("boom"), want: "boom"},
		"a panic": {
			start: pipeline.StepFunc(func(context.Context, *pipeline.StepContext) error { panic("bug") }),
			want:  "bug",
		},
		"a timeout": {start: waitForCancel(), want: "deadline exceeded"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var c calls

			stopper := pipeline.NewStopper()
			plan := mustPlan(t,
				pipeline.Group("node1",
					bounded(pipeline.Effect("stop", stopsWith(stopper))),
					pipeline.Effect("start", tc.start).Timeout(20*time.Millisecond),
					bounded(pipeline.Effect("verify", counted(&c, "verify", nil))),
				).DrainOnStop(),
				pipeline.Effect("after", counted(&c, "after", nil)),
			)

			j := pipeline.MemoryJournal()
			out := resumeOn(t, j, plan, pipeline.WithStopper(stopper))

			require.ErrorContains(t, out.err, tc.want)
			require.NotErrorIs(t, out.err, pipeline.ErrStopped, "the stop kept nothing from starting: the failure did")
			assert.Equal(t, pipeline.OutcomeFailed, out.res.Outcome)
			assert.Zero(t, c.get("verify"), "nothing in the group runs after a failure")
			assert.Zero(t, c.get("after"))

			state, err := j.State()
			require.NoError(t, err)
			assert.Equal(t, pipeline.StatusFailed, state.Element("p/node1").Status)
			assert.False(t, state.Element("p/node1").Done)
			assert.Empty(t, state.Draining())
		})
	}
}

// A step is not tried again to get a group finished: a stop that comes
// while it waits between tries ends the wait, and the step has failed. It is
// that failure the run ends with, as when the stop was already pending: the
// run has failed, and is not reported as stopped.
func TestDrain_AStepIsNotTriedAgainUnderAStop(t *testing.T) {
	t.Parallel()

	var c calls

	fails := true
	stopper := pipeline.NewStopper()
	retrying := pipeline.ObserverFunc(func(_ context.Context, e pipeline.Event) {
		if e.Kind == pipeline.EventStepRetry {
			stopper.Stop("alice")
		}
	})

	plan := mustPlan(t,
		pipeline.Group("node1",
			bounded(pipeline.Effect("start", counted(&c, "start", &fails))).Retry(3, time.Hour),
			bounded(pipeline.Effect("verify", counted(&c, "verify", nil))),
		).DrainOnStop(),
	)

	out := resumeOn(t, pipeline.MemoryJournal(), plan, pipeline.WithStopper(stopper), pipeline.WithObservers(retrying))

	require.ErrorContains(t, out.err, "start failed")
	require.NotErrorIs(t, out.err, pipeline.ErrStopped, "when the stop came makes no difference")
	assert.Equal(t, pipeline.OutcomeFailed, out.res.Outcome)
	assert.Equal(t, 1, c.get("start"))
	assert.Zero(t, c.get("verify"))
	assert.Len(t, kinds(out.events, pipeline.EventStepRetry), 1, "the wait it ended was the first")
	assert.Contains(t, trace(out.events), "group.finished p/node1 failed")
}

// With a stop already pending, a step that fails is not tried again either,
// and it is the failure that ends the group: the run has failed, as it has
// when the step had no tries left. The stop kept nothing from starting.
func TestDrain_AStepThatFailsUnderAPendingStopHasFailedTheRun(t *testing.T) {
	t.Parallel()

	for name, retry := range map[string]func(pipeline.StepNode) pipeline.StepNode{
		"with tries left": func(s pipeline.StepNode) pipeline.StepNode { return s.Retry(3, time.Hour) },
		"with none":       func(s pipeline.StepNode) pipeline.StepNode { return s },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var c calls

			fails := true
			stopper := pipeline.NewStopper()
			plan := mustPlan(t,
				pipeline.Group("node1",
					bounded(pipeline.Effect("stop", stopsWith(stopper))),
					retry(bounded(pipeline.Effect("start", counted(&c, "start", &fails)))),
					bounded(pipeline.Effect("verify", counted(&c, "verify", nil))),
				).DrainOnStop(),
				pipeline.Effect("after", counted(&c, "after", nil)),
			)

			out := resumeOn(t, pipeline.MemoryJournal(), plan, pipeline.WithStopper(stopper))

			require.ErrorContains(t, out.err, "start failed")
			require.NotErrorIs(t, out.err, pipeline.ErrStopped)
			assert.Equal(t, pipeline.OutcomeFailed, out.res.Outcome)
			assert.Equal(t, 1, c.get("start"), "it is not tried again")
			assert.Zero(t, c.get("verify"))
			assert.Zero(t, c.get("after"))
			assert.Empty(t, kinds(out.events, pipeline.EventStepRetry))
			assert.Contains(t, trace(out.events), "group.finished p/node1 failed")
		})
	}
}

// A step that outstays its timeout has timed out, whatever it then returns:
// a skip returned late is not mistaken for one returned in time.
func TestDrain_ASkipReturnedLateIsATimeout(t *testing.T) {
	t.Parallel()

	plan := mustPlan(t,
		pipeline.Group("node1",
			pipeline.Effect("stop", pipeline.StepFunc(func(ctx context.Context, _ *pipeline.StepContext) error {
				<-ctx.Done()

				return pipeline.ErrSkipGroup
			})).Timeout(20*time.Millisecond),
		).DrainOnStop(),
	)

	out := resumeOn(t, pipeline.MemoryJournal(), plan)

	require.ErrorIs(t, out.err, context.DeadlineExceeded)
	require.ErrorContains(t, out.err, "returned after its 20ms timeout had passed")

	finished := kinds(out.events, pipeline.EventStepFinished)
	require.Len(t, finished, 1)
	require.NotNil(t, finished[0].Error)
	assert.Equal(t, pipeline.ErrorTimeout, finished[0].Error.Kind)
}

// A cancel waits for nothing: it outranks the stop the group was finishing
// under.
func TestDrain_ACancelEndsTheGroup(t *testing.T) {
	t.Parallel()

	var c calls

	stopper := pipeline.NewStopper()
	plan := mustPlan(t,
		pipeline.Group("node1",
			bounded(pipeline.Effect("stop", stopsWith(stopper))),
			bounded(pipeline.Effect("install", pipeline.StepFunc(func(ctx context.Context, _ *pipeline.StepContext) error {
				c.add("install")
				stopper.Cancel("bob")
				<-ctx.Done()

				return ctx.Err()
			}))),
			bounded(pipeline.Effect("start", counted(&c, "start", nil))),
		).DrainOnStop(),
	)

	j := pipeline.MemoryJournal()
	out := resumeOn(t, j, plan, pipeline.WithStopper(stopper))

	require.ErrorIs(t, out.err, context.Canceled)
	assert.Equal(t, pipeline.OutcomeCancelled, out.res.Outcome)
	assert.Equal(t, 1, c.get("install"), "the group was carrying on past the stop")
	assert.Zero(t, c.get("start"), "and the cancel ended it")

	state, err := j.State()
	require.NoError(t, err)
	assert.Equal(t, pipeline.StopCancel, state.Attempts[0].Stop.Mode)
	assert.Empty(t, state.Draining())
}

// A journal that can't be written ends the run, whatever it was finishing:
// no step starts that the journal could not record. The next attempt enters
// the group again, and does only what is left of it.
func TestDrain_AJournalFailureEndsTheGroupAndAResumeFinishesIt(t *testing.T) {
	t.Parallel()

	var c calls

	stopper := pipeline.NewStopper()
	plan := upgrade(t, &c, true, map[string]func(){
		"node1/stop": func() { stopper.Stop("alice") },
	})

	crash := pipelinetest.CrashAfter("p/node1/install", pipelinetest.Finished)

	res, err := pipeline.Run(t.Context(), plan, crash.Journal, pipeline.WithStopper(stopper))
	require.ErrorIs(t, err, pipelinetest.ErrCrashed)
	assert.Equal(t, pipeline.OutcomeFailed, res.Outcome)
	assert.Equal(t, []int{1, 1, 0, 0}, ran(&c, "node1"))

	crash.Restart()

	// The process died with the stop pending and the group part-way: the
	// journal says so, and that nothing is finishing it.
	state, err := crash.Journal.State()
	require.NoError(t, err)
	assert.Equal(t, []string{"p/node1"}, state.Draining())
	assert.False(t, state.Live)

	// The old request is not a new one: this attempt runs to the end.
	require.NoError(t, resumeOn(t, crash.Journal, plan).err)
	assert.Equal(t, []int{1, 1, 1, 1}, ran(&c, "node1"), "what had passed is not run again")
	assert.Equal(t, []int{1, 1, 1, 1}, ran(&c, "node2"))
}

// Ending the group early as passed is what the option rules out, so a step
// that asks for it has failed.
func TestDrain_AStepMayNotEndTheGroupEarly(t *testing.T) {
	t.Parallel()

	for name, skip := range map[string]error{"ErrSkipGroup": pipeline.ErrSkipGroup, "ErrSkipRun": pipeline.ErrSkipRun} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var c calls

			plan := mustPlan(t,
				pipeline.Group("node1",
					bounded(pipeline.Effect("stop", pipeline.StepFunc(func(context.Context, *pipeline.StepContext) error {
						return skip
					}))),
					bounded(pipeline.Effect("start", counted(&c, "start", nil))),
				).DrainOnStop(),
				pipeline.Effect("after", counted(&c, "after", nil)),
			)

			out := resumeOn(t, pipeline.MemoryJournal(), plan)

			require.ErrorContains(t, out.err, "nothing may end p/node1, a DrainOnStop group, early")
			assert.Equal(t, pipeline.OutcomeFailed, out.res.Outcome)
			assert.Zero(t, c.get("start"))
			assert.Zero(t, c.get("after"))
			assert.Contains(t, trace(out.events), "step.finished p/node1/stop failed")
		})
	}
}

// A skip that a step recorded before a later build made its group a
// DrainOnStop group is refused in the same way, once: the attempt after
// runs the step.
func TestDrain_ASkipRecordedBeforeTheOptionIsRefused(t *testing.T) {
	t.Parallel()

	var c calls

	skips := true
	plan := func(drain bool) *pipeline.Plan {
		g := pipeline.Group("node1",
			bounded(pipeline.Effect("stop", pipeline.StepFunc(func(context.Context, *pipeline.StepContext) error {
				c.add("stop")

				if skips {
					return pipeline.ErrSkipGroup
				}

				return nil
			}))),
			bounded(pipeline.Effect("start", counted(&c, "start", nil))),
		)
		if drain {
			g = g.DrainOnStop()
		}

		return mustPlan(t, g)
	}

	// The process dies once the step's skip is recorded, with its group
	// unfinished.
	crash := pipelinetest.CrashAfter("p/node1/stop", pipelinetest.Finished)

	_, err := pipeline.Run(t.Context(), plan(false), crash.Journal)
	require.ErrorIs(t, err, pipelinetest.ErrCrashed)

	crash.Restart()

	skips = false
	out := resumeOn(t, crash.Journal, plan(true))

	require.ErrorContains(t, out.err, "an earlier attempt's Run returned skip-group")
	assert.Equal(t, 1, c.get("stop"))
	assert.Zero(t, c.get("start"))

	require.NoError(t, resumeOn(t, crash.Journal, plan(true)).err)
	assert.Equal(t, 2, c.get("stop"))
	assert.Equal(t, 1, c.get("start"))
}

// A step elsewhere that ends the run early does not cut the group short
// either: the group ends, and then the run does.
func TestDrain_AStepElsewhereEndingTheRunWaitsForTheGroup(t *testing.T) {
	t.Parallel()

	var c calls

	skipped := make(chan struct{})

	var once sync.Once

	sibling := pipeline.ObserverFunc(func(_ context.Context, e pipeline.Event) {
		if e.Kind == pipeline.EventStepFinished && e.Path == "p/both/sibling" {
			once.Do(func() { close(skipped) })
		}
	})

	plan := mustPlan(t,
		pipeline.Group("both",
			pipeline.Group("node1",
				bounded(pipeline.Effect("stop", pipeline.StepFunc(func(context.Context, *pipeline.StepContext) error {
					<-skipped

					return nil
				}))),
				bounded(pipeline.Effect("start", counted(&c, "start", nil))),
			).DrainOnStop(),
			pipeline.Effect("sibling", pipeline.StepFunc(func(context.Context, *pipeline.StepContext) error {
				return pipeline.ErrSkipRun
			})),
		).Parallel(0),
		pipeline.Effect("after", counted(&c, "after", nil)),
	)

	out := resumeOn(t, pipeline.MemoryJournal(), plan, pipeline.WithObservers(sibling))

	require.NoError(t, out.err)
	assert.Equal(t, 1, c.get("start"), "the group finishes")
	assert.Zero(t, c.get("after"), "and the run ends early, as it was asked to")
	assert.Contains(t, trace(out.events), "group.finished p/both/node1 passed")
}

// A rollback takes no notice of the option: a stop takes effect between any
// two Undos.
func TestDrain_ARollbackIsStoppedBetweenAnyTwoUndos(t *testing.T) {
	t.Parallel()

	var c calls

	stopper := pipeline.NewStopper()
	plan := mustPlan(t,
		pipeline.Group("node1",
			bounded(pipeline.Effect("stop", undoable{c: &c, name: "stop"})),
			bounded(pipeline.Effect("start", undoable{c: &c, name: "start", undo: func(context.Context, *pipeline.StepContext) error {
				stopper.Stop("alice")

				return nil
			}})),
		).DrainOnStop(),
	)

	j := pipeline.MemoryJournal()
	require.NoError(t, resumeOn(t, j, plan).err)

	back := resumeRollback(t, j, plan, pipeline.WithStopper(stopper))
	require.ErrorIs(t, back.err, pipeline.ErrStopped)
	assert.Equal(t, 1, c.get("undo start"))
	assert.Zero(t, c.get("undo stop"))
}

// An attempt whose plan has a DrainOnStop group is written as version 2,
// which a program built before the option refuses. Any other attempt, in the
// same journal or not, is written as version 1.
func TestDrain_AnAttemptThatNeedsItIsWrittenAsVersion2(t *testing.T) {
	t.Parallel()

	var c calls

	fails := true
	plan := func(drain bool) *pipeline.Plan {
		g := pipeline.Group("node1", bounded(pipeline.Effect("stop", counted(&c, "stop", &fails))))
		if drain {
			g = g.DrainOnStop()
		}

		return mustPlan(t, g)
	}

	require.Equal(t, plan(false).Fingerprint(), plan(true).Fingerprint(), "the option is a setting, not shape")

	versions := func(events []pipeline.Event) []int {
		seen := map[int]bool{}

		for _, e := range events {
			seen[e.V] = true
		}

		out := make([]int, 0, len(seen))
		for v := range seen {
			out = append(out, v)
		}

		return out
	}

	j := pipeline.MemoryJournal()

	assert.Equal(t, []int{1}, versions(resumeOn(t, j, plan(false)).events))
	assert.Equal(t, []int{2}, versions(resumeOn(t, j, plan(true)).events))
	assert.Equal(t, []int{1}, versions(resumeOn(t, j, plan(false)).events))

	// A rollback's attempt is written as its plan needs, too.
	assert.Equal(t, []int{2}, versions(resumeRollback(t, j, plan(true)).events))
	assert.Equal(t, 2, pipeline.SchemaVersion)

	state, err := j.State()
	require.NoError(t, err)
	assert.Len(t, state.Attempts, 4, "this reader reads both")
}

// Nothing in the group may keep a stop waiting for ever, or end the group
// early without a failure.
func TestNewPlan_DrainOnStop(t *testing.T) {
	t.Parallel()

	step := func(name string) pipeline.StepNode { return bounded(pipeline.Effect(name, ok())) }

	for name, tc := range map[string]struct {
		group pipeline.GroupNode
		want  string
	}{
		"a step with no timeout": {
			group: pipeline.Group("g", pipeline.Effect("a", ok())).DrainOnStop(),
			want:  "p/g/a: a step inside a DrainOnStop group needs a Timeout",
		},
		"parallel": {
			group: pipeline.Group("g", step("a")).Parallel(2).DrainOnStop(),
			want:  "p/g: a DrainOnStop group may not be parallel",
		},
		"a parallel group inside": {
			group: pipeline.Group("g", pipeline.Group("inner", step("a")).Parallel(0)).DrainOnStop(),
			want:  "p/g/inner: a group inside a DrainOnStop group may not be parallel",
		},
		"continue on error": {
			group: pipeline.Group("g", step("a")).ContinueOnError().DrainOnStop(),
			want:  "p/g: a DrainOnStop group must stop at its first failure",
		},
		"continue on error inside": {
			group: pipeline.Group("g", pipeline.Group("inner", step("a")).ContinueOnError()).DrainOnStop(),
			want:  "p/g/inner: a group inside a DrainOnStop group must stop at its first failure",
		},
		"a cleanup handler": {
			group: pipeline.Group("g", step("a")).OnFailure(step("cleanup")).DrainOnStop(),
			want:  "p/g: a DrainOnStop group may not have a failure handler",
		},
		"a recovery handler inside": {
			group: pipeline.Group("g", pipeline.Group("inner", step("a")).RecoverOnFailure(step("recover"))).DrainOnStop(),
			want:  "p/g/inner: a group inside a DrainOnStop group may not have a failure handler",
		},
		"a gate": {
			group: pipeline.Group("g", step("a"), pipeline.Gate("go", pipeline.GateOptions{})).DrainOnStop(),
			want:  "p/g/go: a gate may not be inside a DrainOnStop group",
		},
		"another inside": {
			group: pipeline.Group("g", pipeline.Group("inner", step("a")).DrainOnStop()).DrainOnStop(),
			want:  "p/g/inner: a DrainOnStop group may not be inside another",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := pipeline.NewPlan("p", tc.group)
			require.ErrorContains(t, err, tc.want)
		})
	}

	_, err := pipeline.NewPlanFromGroup(pipeline.Group("p", step("a")).DrainOnStop())
	require.ErrorContains(t, err, "p: the plan's root may not be a DrainOnStop group")

	// A sequential group inside one is part of it, and two may sit side by
	// side, in a parallel group or not.
	plan, err := pipeline.NewPlan("p",
		pipeline.Group("nodes",
			pipeline.Group("node1", step("stop"), pipeline.Group("bring-up", step("start"), step("verify"))).DrainOnStop(),
			pipeline.Group("node2", step("stop")).DrainOnStop(),
		).Parallel(2),
	)
	require.NoError(t, err)
	assert.Equal(t, `p
└── nodes (parallel, limit 2)
    ├── node1 (finishes before a stop)
    │   ├── stop (effect)
    │   └── bring-up
    │       ├── start (effect)
    │       └── verify (effect)
    └── node2 (finishes before a stop)
        └── stop (effect)
`, pipeline.Describe(plan))
}

// A group inside a DrainOnStop group finishes with it.
func TestDrain_AGroupInsideFinishesToo(t *testing.T) {
	t.Parallel()

	var c calls

	stopper := pipeline.NewStopper()
	plan := mustPlan(t,
		pipeline.Group("node1",
			bounded(pipeline.Effect("stop", stopsWith(stopper))),
			pipeline.Group("bring-up",
				bounded(pipeline.Effect("start", counted(&c, "start", nil))),
				bounded(pipeline.Effect("verify", counted(&c, "verify", nil))),
			),
		).DrainOnStop(),
		pipeline.Effect("after", counted(&c, "after", nil)),
	)

	var b strings.Builder

	out := resumeOn(t, pipeline.MemoryJournal(), plan,
		pipeline.WithStopper(stopper), pipeline.WithObservers(pipeline.NewPlainObserver(&b)))

	require.Equal(t, pipeline.ErrStopped, out.err)
	assert.Equal(t, 1, c.get("start"))
	assert.Equal(t, 1, c.get("verify"))
	assert.Zero(t, c.get("after"))
	assert.Len(t, kinds(out.events, pipeline.EventMessage), 1, "the group says so once, not once for each group in it")
	assert.Contains(t, b.String(), "node1: [INFO] a stop was requested: this group finishes first\n")
}
