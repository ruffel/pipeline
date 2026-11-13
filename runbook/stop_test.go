package runbook_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ruffel/pipeline/runbook"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stopsWith returns a step that asks s to stop, then passes.
func stopsWith(s *runbook.Stopper) runbook.Step {
	return runbook.StepFunc(func(context.Context, *runbook.StepContext) error {
		s.Stop("alice")

		return nil
	})
}

func TestStop_ARunningStepFinishesAndNothingNewStarts(t *testing.T) {
	t.Parallel()

	var c calls

	stopper := runbook.NewStopper()
	plan := mustPlan(t,
		runbook.Group("g",
			runbook.Effect("a", runbook.StepFunc(func(ctx context.Context, s *runbook.StepContext) error {
				c.add("a")

				return stopsWith(stopper).Run(ctx, s)
			})),
			runbook.Effect("b", counted(&c, "b", nil)),
		),
		runbook.Effect("c", counted(&c, "c", nil)),
	)

	j := runbook.MemoryJournal()
	first := resumeOn(t, j, plan, runbook.WithStopper(stopper))

	require.ErrorIs(t, first.err, runbook.ErrStopped)
	assert.Equal(t, runbook.ErrStopped, first.err, "nothing failed, so the stop is the whole error")
	assert.Equal(t, runbook.OutcomeStopped, first.res.Outcome)
	assert.Equal(t, []string{
		"group.started p/g",
		"step.started p/g/a",
		"step.running p/g/a",
		"step.finished p/g/a passed",
		"group.finished p/g stopped",
	}, trace(first.events))

	// The request is recorded as it is made, while a still runs or just
	// after: before anything is kept from starting.
	got := trace(first.events, runbook.EventStopRequested)
	assert.Less(t, slices.Index(got, "stop.requested p"), slices.Index(got, "group.finished p/g stopped"))
	assert.Greater(t, slices.Index(got, "stop.requested p"), slices.Index(got, "step.running p/g/a"))

	state, err := j.State()
	require.NoError(t, err)
	assert.True(t, state.Unfinished(), "a stopped run is unfinished")
	assert.Equal(t, &runbook.StopRequest{Mode: runbook.StopGraceful, By: "stopper", Who: "alice"}, state.Attempts[0].Stop)
	assert.Equal(t, runbook.OutcomeStopped, state.Attempts[0].Outcome)
	assert.Equal(t, runbook.StatusStopped, state.Element("p/g").Status)
	assert.Equal(t, runbook.StatusNotReached, state.Element("p/g/b").Status)
	assert.Equal(t, runbook.StatusNotReached, state.Element("p/c").Status)

	// A resume does only what the stop left.
	second := resumeOn(t, j, plan)

	require.NoError(t, second.err)
	assert.Equal(t, []string{
		"group.started p/g",
		"step.finished p/g/a skipped/earlier-attempt",
		"step.started p/g/b",
		"step.running p/g/b",
		"step.finished p/g/b passed",
		"group.finished p/g passed",
		"step.started p/c",
		"step.running p/c",
		"step.finished p/c passed",
	}, trace(second.events, runbook.EventStopRequested))
	assert.Equal(t, 1, c.get("a"))
	assert.Equal(t, 1, c.get("b"))
	assert.Equal(t, 1, c.get("c"))
}

// A step waiting to be tried again is not: it fails with its last error.
func TestStop_EndsAWaitBetweenTries(t *testing.T) {
	t.Parallel()

	stopper := runbook.NewStopper()
	stopOnRetry := runbook.ObserverFunc(func(_ context.Context, e runbook.Event) {
		if e.Kind == runbook.EventStepRetry {
			stopper.Stop("")
		}
	})

	start := time.Now()
	out := runPlan(t, t.Context(), mustPlan(t,
		runbook.Effect("a", fail("boom")).Retry(5, time.Hour),
		runbook.Effect("b", ok()),
	), runbook.WithStopper(stopper), runbook.WithObservers(stopOnRetry))

	assert.Less(t, time.Since(start), time.Minute, "the hour's wait was cut short")
	require.ErrorIs(t, out.err, runbook.ErrStopped)
	require.ErrorContains(t, out.err, "p/a: boom")
	assert.Equal(t, runbook.OutcomeStopped, out.res.Outcome)

	assert.Equal(t, []string{
		"step.started p/a",
		"step.running p/a",
		"step.retry p/a",
		"stop.requested p",
		"step.finished p/a failed",
	}, trace(out.events, runbook.EventStepRetry, runbook.EventStopRequested))

	a := out.state.Element("p/a")
	assert.Equal(t, runbook.StatusFailed, a.Status)
	require.NotNil(t, a.Error)
	assert.Equal(t, "boom", a.Error.Message, "recorded with its last error")
}

func TestStop_AParallelGroupFinishesWhatIsRunningAndStartsNothingMore(t *testing.T) {
	t.Parallel()

	var (
		c       calls
		running sync.WaitGroup
	)

	stopper := runbook.NewStopper()
	release := make(chan struct{})
	recorded := make(chan struct{})

	blocking := func(name string) runbook.Step {
		return runbook.StepFunc(func(context.Context, *runbook.StepContext) error {
			c.add(name)
			running.Done()
			<-release

			return nil
		})
	}

	running.Add(2)

	go func() {
		running.Wait()
		stopper.Stop("")
		<-recorded
		close(release)
	}()

	seen := runbook.ObserverFunc(func(_ context.Context, e runbook.Event) {
		if e.Kind == runbook.EventStopRequested {
			close(recorded)
		}
	})

	out := runPlan(t, t.Context(), mustPlan(t,
		runbook.Group("g",
			runbook.Effect("a", blocking("a")),
			runbook.Effect("b", blocking("b")),
			runbook.Effect("c", counted(&c, "c", nil)),
			runbook.Effect("d", counted(&c, "d", nil)),
		).Parallel(2),
	), runbook.WithStopper(stopper), runbook.WithObservers(seen))

	assert.Equal(t, runbook.ErrStopped, out.err)
	assert.Equal(t, runbook.OutcomeStopped, out.res.Outcome)
	assert.Equal(t, runbook.StatusPassed, out.state.Element("p/g/a").Status)
	assert.Equal(t, runbook.StatusPassed, out.state.Element("p/g/b").Status)
	assert.Equal(t, runbook.StatusNotReached, out.state.Element("p/g/c").Status)
	assert.Equal(t, runbook.StatusNotReached, out.state.Element("p/g/d").Status)
	assert.Equal(t, runbook.StatusStopped, out.state.Element("p/g").Status)
	assert.Zero(t, c.get("c")+c.get("d"))
}

func TestStop_BeforeTheRunStartsNothing(t *testing.T) {
	t.Parallel()

	stopper := runbook.NewStopper()
	stopper.Stop("ops")

	out := runPlan(t, t.Context(), mustPlan(t, runbook.Effect("a", ok())), runbook.WithStopper(stopper))

	assert.Equal(t, runbook.ErrStopped, out.err)
	assert.Equal(t, runbook.OutcomeStopped, out.res.Outcome)

	kinds := make([]runbook.EventKind, 0, len(out.events))
	for _, e := range out.events {
		kinds = append(kinds, e.Kind)
	}

	assert.Equal(t, []runbook.EventKind{
		runbook.EventRunStarted, runbook.EventStopRequested, runbook.EventRunFinished,
	}, kinds)
	assert.Equal(t, "ops", out.events[1].Who)
}

// A stop that keeps nothing from happening changes nothing, but is still
// recorded.
func TestStop_AfterTheLastStepStartedChangesNothing(t *testing.T) {
	t.Parallel()

	stopper := runbook.NewStopper()

	out := runPlan(t, t.Context(), mustPlan(t,
		runbook.Effect("a", ok()),
		runbook.Effect("last", stopsWith(stopper)),
	), runbook.WithStopper(stopper))

	require.NoError(t, out.err)
	assert.Equal(t, runbook.OutcomePassed, out.res.Outcome)
	assert.False(t, out.state.Unfinished())
	assert.NotNil(t, out.state.Attempts[0].Stop, "the request is recorded all the same")
}

// A failure and a stop in one run: the stop decides the outcome, and Run
// returns both.
func TestStop_WithAFailureReturnsBoth(t *testing.T) {
	t.Parallel()

	stopper := runbook.NewStopper()
	boom := errors.New("boom")

	out := runPlan(t, t.Context(), mustPlan(t,
		runbook.Group("g",
			runbook.Effect("a", runbook.StepFunc(func(context.Context, *runbook.StepContext) error {
				stopper.Stop("")

				return boom
			})),
			runbook.Effect("b", ok()),
		).ContinueOnError(),
		runbook.Effect("c", ok()),
	), runbook.WithStopper(stopper))

	require.ErrorIs(t, out.err, runbook.ErrStopped)
	require.ErrorIs(t, out.err, boom)
	assert.Equal(t, runbook.OutcomeStopped, out.res.Outcome)
	assert.Equal(t, runbook.StatusFailed, out.state.Element("p/g").Status, "something in the group failed")
	assert.Equal(t, runbook.StatusNotReached, out.state.Element("p/g/b").Status)
	assert.Equal(t, runbook.StatusNotReached, out.state.Element("p/c").Status)
}

// A cancel that arrives with a stop is the stronger of the two.
func TestStop_ACancelOutranksAStop(t *testing.T) {
	t.Parallel()

	stopper := runbook.NewStopper()
	ctx, cancel := context.WithCancel(t.Context())

	out := runPlan(t, ctx, mustPlan(t,
		runbook.Effect("a", runbook.StepFunc(func(context.Context, *runbook.StepContext) error {
			stopper.Stop("")
			cancel()

			return nil
		})),
		runbook.Effect("b", ok()),
	), runbook.WithStopper(stopper))

	require.ErrorIs(t, out.err, context.Canceled)
	assert.Equal(t, runbook.OutcomeCancelled, out.res.Outcome)
}

// readyClock's waits are over as soon as they begin, so a wait between tries
// is ready at the same moment as anything else.
type readyClock struct{}

func (readyClock) Now() time.Time { return time.Now() }

func (readyClock) After(time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	ch <- time.Now()

	return ch
}

// No try starts after a stop, even when the wait before it is already over:
// a select between the two would pick either.
func TestStop_NoTryStartsAfterAStopEvenWhenTheWaitIsOver(t *testing.T) {
	t.Parallel()

	for range 100 {
		var tries atomic.Int32

		stopper := runbook.NewStopper()

		out := runPlan(t, t.Context(), mustPlan(t,
			runbook.Effect("a", runbook.StepFunc(func(context.Context, *runbook.StepContext) error {
				tries.Add(1)
				stopper.Stop("")

				return errors.New("boom")
			})).Retry(5, 0),
		), runbook.WithStopper(stopper), runbook.WithClock(readyClock{}))

		require.ErrorIs(t, out.err, runbook.ErrStopped)
		require.Equal(t, int32(1), tries.Load(), "a try started after the stop")
	}
}

// A nested group the stop cut short has not failed: a parallel parent that
// cancels on failure must not cancel the siblings still running.
func TestStop_ANestedGroupCutShortDoesNotCancelParallelSiblings(t *testing.T) {
	t.Parallel()

	stopper := runbook.NewStopper()
	release := make(chan struct{})

	finishedG := runbook.ObserverFunc(func(_ context.Context, e runbook.Event) {
		if e.Kind == runbook.EventGroupFinished && e.Path == "p/par/g" {
			close(release)
		}
	})

	out := runPlan(t, t.Context(), mustPlan(t,
		runbook.Group("par",
			runbook.Group("g",
				runbook.Effect("a", stopsWith(stopper)),
				runbook.Effect("b", ok()),
			),
			runbook.Effect("sibling", runbook.StepFunc(func(ctx context.Context, _ *runbook.StepContext) error {
				<-release

				return ctx.Err()
			})),
		).Parallel(2).CancelOnError(),
	), runbook.WithStopper(stopper), runbook.WithObservers(finishedG))

	assert.Equal(t, runbook.ErrStopped, out.err)
	assert.Equal(t, runbook.StatusPassed, out.state.Element("p/par/sibling").Status)
	assert.Equal(t, runbook.StatusStopped, out.state.Element("p/par/g").Status)
	assert.Equal(t, runbook.StatusStopped, out.state.Element("p/par").Status)
}

// A Stopper's cancel cancels running steps, and is recorded with who asked.
func TestStopper_CancelCancelsTheRun(t *testing.T) {
	t.Parallel()

	for _, stopFirst := range []bool{false, true} {
		stopper := runbook.NewStopper()
		cancelWhileRunning := runbook.ObserverFunc(func(_ context.Context, e runbook.Event) {
			if e.Kind == runbook.EventStepRunning {
				if stopFirst {
					stopper.Stop("bob")
				}

				stopper.Cancel("alice")
			}
		})

		var c calls

		out := runPlan(t, t.Context(), mustPlan(t,
			runbook.Effect("wait", waitForCancel()),
			runbook.Effect("after", counted(&c, "after", nil)),
		), runbook.WithStopper(stopper), runbook.WithObservers(cancelWhileRunning))

		require.ErrorIs(t, out.err, context.Canceled)
		assert.Equal(t, runbook.OutcomeCancelled, out.res.Outcome)
		assert.Equal(t, &runbook.StopRequest{
			Mode: runbook.StopCancel, By: "stopper", Who: "alice",
		}, out.state.Attempts[0].Stop, "a cancel outranks a stop")
		assert.Equal(t, runbook.ErrorCancelled, out.state.Element("p/wait").Error.Kind)
		assert.Zero(t, c.get("after"))
	}
}

// A Stopper cancelled before the run starts nothing.
func TestStopper_CancelledBeforeTheRun(t *testing.T) {
	t.Parallel()

	stopper := runbook.NewStopper()
	stopper.Cancel("alice")

	var c calls

	out := runPlan(t, t.Context(), mustPlan(t,
		runbook.Effect("first", counted(&c, "first", nil)),
	), runbook.WithStopper(stopper))

	require.ErrorIs(t, out.err, context.Canceled)
	assert.Equal(t, runbook.OutcomeCancelled, out.res.Outcome)
	assert.Zero(t, c.get("first"))
	assert.Equal(t, runbook.StopCancel, out.state.Attempts[0].Stop.Mode)
}

// A Stopper both stopped and cancelled before the run is cancelled: a
// cancel outranks a stop.
func TestStopper_StoppedAndCancelledBeforeTheRun(t *testing.T) {
	t.Parallel()

	for range 50 {
		stopper := runbook.NewStopper()
		stopper.Stop("bob")
		stopper.Cancel("alice")

		var c calls

		out := runPlan(t, t.Context(), mustPlan(t,
			runbook.Effect("first", counted(&c, "first", nil)),
		), runbook.WithStopper(stopper))

		require.ErrorIs(t, out.err, context.Canceled)
		require.Equal(t, runbook.OutcomeCancelled, out.res.Outcome)
		require.Zero(t, c.get("first"))
	}
}

// A parallel group asked to stop and cancel between two children starts no
// more, and is cut short by the cancel.
func TestStopper_StoppedAndCancelledInAParallelGroup(t *testing.T) {
	t.Parallel()

	for range 50 {
		stopper := runbook.NewStopper()
		first := runbook.StepFunc(func(context.Context, *runbook.StepContext) error {
			stopper.Cancel("alice")
			stopper.Stop("bob")

			return nil
		})

		var c calls

		out := runPlan(t, t.Context(), mustPlan(t,
			runbook.Group("g",
				runbook.Effect("first", first),
				runbook.Effect("second", counted(&c, "second", nil)),
			).Parallel(1),
		), runbook.WithStopper(stopper))

		require.ErrorIs(t, out.err, context.Canceled)
		require.Equal(t, runbook.OutcomeCancelled, out.res.Outcome)
		require.Zero(t, c.get("second"))
		require.Equal(t, runbook.StatusFailed, out.state.Element("p/g").Status, "cut short, not stopped")
	}
}

// A gate whose approval comes with a stop and a cancel, all from a Stopper,
// finishes cancelled.
func TestStopper_ACancelPendingAtAGateOutranksItsApproval(t *testing.T) {
	t.Parallel()

	for range 50 {
		stopper := runbook.NewStopper()
		a := approver{name: "test", wait: func(context.Context, runbook.GateInfo) (string, error) {
			// The cancel comes first, so it is pending however soon the stop
			// ends the wait.
			stopper.Cancel("alice")
			stopper.Stop("bob")

			return "carol", nil
		}}

		out := runPlan(t, t.Context(), mustPlan(t,
			runbook.Gate("go", runbook.GateOptions{Approvers: []runbook.Approver{a}}),
			runbook.Effect("after", ok()),
		), runbook.WithStopper(stopper))

		require.ErrorIs(t, out.err, context.Canceled)
		require.Equal(t, runbook.OutcomeCancelled, out.res.Outcome)
		require.Empty(t, kinds(out.events, runbook.EventGateReleased))
	}
}

// A step waiting to be tried again when a Stopper cancels and stops is
// cancelled: a cancel outranks a stop.
func TestStopper_ACancelPendingBetweenTriesOutranksAStop(t *testing.T) {
	t.Parallel()

	for range 50 {
		stopper := runbook.NewStopper()
		cancelOnRetry := runbook.ObserverFunc(func(_ context.Context, e runbook.Event) {
			if e.Kind == runbook.EventStepRetry {
				stopper.Cancel("alice")
				stopper.Stop("bob")
			}
		})

		out := runPlan(t, t.Context(), mustPlan(t,
			runbook.Effect("a", fail("boom")).Retry(5, time.Hour),
		), runbook.WithStopper(stopper), runbook.WithObservers(cancelOnRetry))

		require.ErrorIs(t, out.err, context.Canceled)
		require.Equal(t, runbook.OutcomeCancelled, out.res.Outcome)
		require.Equal(t, runbook.ErrorCancelled, out.state.Element("p/a").Error.Kind)
	}
}
