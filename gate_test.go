package pipeline_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ruffel/pipeline"
	"github.com/ruffel/pipeline/pipelinetest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// approver is an Approver made of a name and a function.
type approver struct {
	name string
	wait func(ctx context.Context, g pipeline.GateInfo) (string, error)
}

func (a approver) Name() string { return a.name }

func (a approver) Wait(ctx context.Context, g pipeline.GateInfo) (string, error) {
	return a.wait(ctx, g)
}

// approves returns an approver that approves at once as who.
func approves(name, who string) pipeline.Approver {
	return approver{name: name, wait: func(context.Context, pipeline.GateInfo) (string, error) { return who, nil }}
}

// never returns an approver that waits until its context is done, and
// reports when it is through done.
func never(done chan<- struct{}) pipeline.Approver {
	return approver{name: "never", wait: func(ctx context.Context, _ pipeline.GateInfo) (string, error) {
		<-ctx.Done()

		if done != nil {
			close(done)
		}

		return "", ctx.Err()
	}}
}

// fixedClock's time stands still where it is set, and its waits never end.
type fixedClock struct {
	now atomic.Pointer[time.Time]
}

func newFixedClock(t time.Time) *fixedClock {
	c := &fixedClock{}
	c.now.Store(&t)

	return c
}

func (c *fixedClock) Now() time.Time { return *c.now.Load() }

func (c *fixedClock) After(time.Duration) <-chan time.Time { return nil }

func (c *fixedClock) set(t time.Time) { c.now.Store(&t) }

func TestGate_AnApproverReleasesIt(t *testing.T) {
	t.Parallel()

	var got pipeline.GateInfo

	a := approver{name: "test", wait: func(_ context.Context, g pipeline.GateInfo) (string, error) {
		got = g

		return "alice", nil
	}}

	out := runPlan(t, t.Context(), mustPlan(t,
		pipeline.Gate("go", pipeline.GateOptions{Prompt: "Go on?", Approvers: []pipeline.Approver{a}}),
		pipeline.Effect("after", ok()),
	))

	require.NoError(t, out.err)
	assert.Equal(t, pipeline.GateInfo{Path: "p/go", Prompt: "Go on?"}, got)
	assert.Equal(t, []string{
		"step.started p/go",
		"gate.waiting p/go",
		"gate.released p/go",
		"step.finished p/go passed",
		"step.started p/after",
		"step.running p/after",
		"step.finished p/after passed",
	}, trace(out.events))

	released := kinds(out.events, pipeline.EventGateReleased)[0]
	assert.Equal(t, "test", released.By)
	assert.Equal(t, "alice", released.Who)

	waiting := kinds(out.events, pipeline.EventGateWaiting)[0]
	assert.Equal(t, "Go on?", waiting.Prompt)
	assert.True(t, waiting.Deadline.IsZero(), "a gate with no timeout has no deadline")

	el := out.state.Element("p/go")
	assert.Equal(t, pipeline.StatusPassed, el.Status)
	assert.False(t, el.Waiting)
}

func TestGate_TimesOutUnderEachPolicy(t *testing.T) {
	t.Parallel()

	gate := func(policy pipeline.TimeoutPolicy) *pipeline.Plan {
		return mustPlan(t,
			pipeline.Gate("go", pipeline.GateOptions{
				Approvers: []pipeline.Approver{never(nil)}, Timeout: time.Hour, OnTimeout: policy,
			}),
			pipeline.Effect("after", ok()),
		)
	}

	t.Run("continue", func(t *testing.T) {
		t.Parallel()

		out := runPlan(t, t.Context(), gate(pipeline.TimeoutContinue), pipeline.WithClock(readyClock{}))

		require.NoError(t, out.err)
		released := kinds(out.events, pipeline.EventGateReleased)[0]
		assert.Equal(t, "timeout", released.By)
		assert.Equal(t, pipeline.TimeoutContinue, released.Policy)
		assert.Equal(t, pipeline.StatusPassed, out.state.Element("p/after").Status)
	})

	t.Run("decline", func(t *testing.T) {
		t.Parallel()

		out := runPlan(t, t.Context(), gate(pipeline.TimeoutDecline), pipeline.WithClock(readyClock{}))

		require.ErrorIs(t, out.err, pipeline.ErrGateDeclined)
		assert.Equal(t, pipeline.OutcomeFailed, out.res.Outcome)
		assert.Equal(t, pipeline.TimeoutDecline, kinds(out.events, pipeline.EventGateReleased)[0].Policy)

		el := out.state.Element("p/go")
		assert.Equal(t, pipeline.StatusFailed, el.Status)
		assert.Equal(t, pipeline.ErrorDeclined, el.Error.Kind)
		assert.Equal(t, pipeline.StatusNotReached, out.state.Element("p/after").Status)
	})

	t.Run("stop", func(t *testing.T) {
		t.Parallel()

		out := runPlan(t, t.Context(), gate(pipeline.TimeoutStop), pipeline.WithClock(readyClock{}))

		assert.Equal(t, pipeline.ErrStopped, out.err)
		assert.Equal(t, pipeline.OutcomeStopped, out.res.Outcome)
		assert.Empty(t, kinds(out.events, pipeline.EventGateReleased), "the gate is not released")
		assert.Equal(t, &pipeline.StopRequest{
			Mode: pipeline.StopGraceful, By: "timeout", Who: "p/go",
		}, out.state.Attempts[0].Stop)
		assert.Equal(t, pipeline.StatusStopped, out.state.Element("p/go").Status)
		assert.Equal(t, pipeline.StatusNotReached, out.state.Element("p/after").Status)
	})
}

// A stop ends the wait without releasing the gate, and a resume waits again.
func TestGate_AStopInterruptsTheWait(t *testing.T) {
	t.Parallel()

	stopper := pipeline.NewStopper()
	stopWhileWaiting := pipeline.ObserverFunc(func(_ context.Context, e pipeline.Event) {
		if e.Kind == pipeline.EventGateWaiting {
			stopper.Stop("alice")
		}
	})

	gone := make(chan struct{})
	plan := func(a pipeline.Approver) *pipeline.Plan {
		return mustPlan(t,
			pipeline.Gate("go", pipeline.GateOptions{Approvers: []pipeline.Approver{a}}),
			pipeline.Effect("after", ok()),
		)
	}

	j := pipeline.MemoryJournal()
	first := resumeOn(t, j, plan(never(gone)), pipeline.WithStopper(stopper), pipeline.WithObservers(stopWhileWaiting))

	assert.Equal(t, pipeline.ErrStopped, first.err)
	assert.Equal(t, []string{
		"step.started p/go",
		"gate.waiting p/go",
		"stop.requested p",
		"step.finished p/go stopped",
	}, trace(first.events, pipeline.EventStopRequested))
	<-gone // the approver was told to give up

	second := resumeOn(t, j, plan(approves("test", "bob")))

	require.NoError(t, second.err)
	assert.Equal(t, []string{
		"step.started p/go",
		"gate.waiting p/go",
		"gate.released p/go",
		"step.finished p/go passed",
		"step.started p/after",
		"step.running p/after",
		"step.finished p/after passed",
	}, trace(second.events))
}

// A gate released in an earlier attempt doesn't ask again.
func TestGate_ReleasedInAnEarlierAttemptDoesNotWaitAgain(t *testing.T) {
	t.Parallel()

	failing := true
	asked := 0

	a := approver{name: "test", wait: func(context.Context, pipeline.GateInfo) (string, error) {
		asked++

		return "alice", nil
	}}

	var c calls

	plan := mustPlan(t,
		pipeline.Gate("go", pipeline.GateOptions{Approvers: []pipeline.Approver{a}}),
		pipeline.Effect("after", counted(&c, "after", &failing)),
	)

	j := pipeline.MemoryJournal()
	require.Error(t, resumeOn(t, j, plan).err)

	failing = false
	second := resumeOn(t, j, plan)

	require.NoError(t, second.err)
	assert.Equal(t, 1, asked)
	assert.Contains(t, trace(second.events), "step.finished p/go skipped/earlier-attempt")
}

// Approvers that can't answer withdraw, a panic included, and the gate goes
// on waiting for the rest.
func TestGate_ApproversThatWithdrawLeaveTheOthers(t *testing.T) {
	t.Parallel()

	// late approves once both withdrawals are recorded.
	var warned atomic.Int32

	recorded := make(chan struct{})
	watch := pipeline.ObserverFunc(func(_ context.Context, e pipeline.Event) {
		if e.Kind == pipeline.EventMessage && warned.Add(1) == 2 {
			close(recorded)
		}
	})

	lost := approver{name: "stdin", wait: func(context.Context, pipeline.GateInfo) (string, error) {
		return "", errors.New("EOF")
	}}
	broken := approver{name: "broken", wait: func(context.Context, pipeline.GateInfo) (string, error) {
		panic("boom")
	}}
	late := approver{name: "late", wait: func(ctx context.Context, _ pipeline.GateInfo) (string, error) {
		select {
		case <-recorded:
			return "carol", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}}

	out := runPlan(t, t.Context(), mustPlan(t,
		pipeline.Gate("go", pipeline.GateOptions{Approvers: []pipeline.Approver{lost, broken, late}}),
	), pipeline.WithObservers(watch))

	require.NoError(t, out.err)
	assert.Equal(t, "late", kinds(out.events, pipeline.EventGateReleased)[0].By)

	messages := kinds(out.events, pipeline.EventMessage)
	warnings := make([]string, 0, len(messages))

	for _, e := range messages {
		warnings = append(warnings, e.Message)
	}

	slices.Sort(warnings)
	require.Len(t, warnings, 2)
	assert.Equal(t, "approver broken withdrew: panic: boom", warnings[0])
	assert.Equal(t, "approver stdin withdrew: EOF", warnings[1])
}

// Approvers still waiting when the gate is released are told to give up.
func TestGate_OtherApproversAreToldToGiveUp(t *testing.T) {
	t.Parallel()

	gone := make(chan struct{})

	out := runPlan(t, t.Context(), mustPlan(t,
		pipeline.Gate("go", pipeline.GateOptions{Approvers: []pipeline.Approver{never(gone), approves("quick", "dan")}}),
	))

	require.NoError(t, out.err)

	select {
	case <-gone:
	case <-time.After(10 * time.Second):
		t.Fatal("the waiting approver's context was never done")
	}
}

func TestGate_ACancelEndsTheWait(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancelWhileWaiting := pipeline.ObserverFunc(func(_ context.Context, e pipeline.Event) {
		if e.Kind == pipeline.EventGateWaiting {
			cancel()
		}
	})

	out := runPlan(t, ctx, mustPlan(t,
		pipeline.Gate("go", pipeline.GateOptions{Approvers: []pipeline.Approver{never(nil)}}),
		pipeline.Effect("after", ok()),
	), pipeline.WithObservers(cancelWhileWaiting))

	require.ErrorIs(t, out.err, context.Canceled)
	assert.Equal(t, pipeline.OutcomeCancelled, out.res.Outcome)
	assert.Equal(t, pipeline.ErrorCancelled, out.state.Element("p/go").Error.Kind)
	assert.Equal(t, pipeline.StatusNotReached, out.state.Element("p/after").Status)
}

// A gate that was waiting when its run stopped waits its whole timeout again
// on resume, from when it starts waiting again.
func TestGate_TheTimeoutRestartsOnResume(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	clock := newFixedClock(start)

	stopper := pipeline.NewStopper()
	stopWhileWaiting := pipeline.ObserverFunc(func(_ context.Context, e pipeline.Event) {
		if e.Kind == pipeline.EventGateWaiting {
			stopper.Stop("")
		}
	})

	plan := mustPlan(t, pipeline.Gate("go", pipeline.GateOptions{
		Approvers: []pipeline.Approver{never(nil)}, Timeout: 6 * time.Hour, OnTimeout: pipeline.TimeoutContinue,
	}))

	j := pipeline.MemoryJournal()
	first := resumeOn(t, j, plan, pipeline.WithClock(clock), pipeline.WithStopper(stopper),
		pipeline.WithObservers(stopWhileWaiting))
	require.ErrorIs(t, first.err, pipeline.ErrStopped)
	assert.Equal(t, start.Add(6*time.Hour), kinds(first.events, pipeline.EventGateWaiting)[0].Deadline)

	clock.set(start.Add(5 * time.Hour))

	stopper = pipeline.NewStopper()
	second := resumeOn(t, j, plan, pipeline.WithClock(clock), pipeline.WithStopper(stopper),
		pipeline.WithObservers(stopWhileWaiting))
	require.ErrorIs(t, second.err, pipeline.ErrStopped)
	assert.Equal(t, start.Add(11*time.Hour), kinds(second.events, pipeline.EventGateWaiting)[0].Deadline)
}

func TestGate_PlanValidation(t *testing.T) {
	t.Parallel()

	var nilApprover *approver

	for _, tc := range []struct {
		opts pipeline.GateOptions
		want string
	}{
		{pipeline.GateOptions{Timeout: time.Hour}, "a timeout needs OnTimeout"},
		{pipeline.GateOptions{OnTimeout: pipeline.TimeoutContinue}, "OnTimeout needs a timeout"},
		{pipeline.GateOptions{Timeout: -1, OnTimeout: pipeline.TimeoutContinue}, "timeout may not be negative"},
		{pipeline.GateOptions{Timeout: time.Hour, OnTimeout: "wait"}, `unknown timeout policy "wait"`},
		{pipeline.GateOptions{Approvers: []pipeline.Approver{nil}}, "approver 0 is nil"},
		{pipeline.GateOptions{Approvers: []pipeline.Approver{approves("a", ""), nilApprover}}, "approver 1 is nil"},
	} {
		_, err := pipeline.NewPlan("p", pipeline.Gate("go", tc.opts))
		require.ErrorContains(t, err, "p/go: "+tc.want)
	}
}

func TestGate_SpecAndDescribe(t *testing.T) {
	t.Parallel()

	plan := mustPlan(t, pipeline.Gate("go", pipeline.GateOptions{
		Prompt: "Go on?", Timeout: time.Hour, OnTimeout: pipeline.TimeoutDecline,
	}).Description("before DC2"))

	spec := plan.Spec().Children[0]
	assert.Equal(t, pipeline.NodeSpec{
		Name: "go", Path: "p/go", Kind: pipeline.KindGate, Description: "before DC2",
		Prompt: "Go on?", Timeout: time.Hour, OnTimeout: pipeline.TimeoutDecline,
	}, spec)

	assert.Contains(t, pipeline.Describe(plan), "go (gate, decline after 1h0m0s) — before DC2")

	other := mustPlan(t, pipeline.Effect("go", ok()))
	assert.NotEqual(t, plan.Fingerprint(), other.Fingerprint(), "a gate is not a step of another kind")
}

func TestPlainObserver_AGate(t *testing.T) {
	t.Parallel()

	var buf strings.Builder

	obs := pipeline.NewPlainObserver(&buf)
	deadline := time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC)

	for _, e := range []pipeline.Event{
		{Kind: pipeline.EventGateWaiting, Path: "upgrade/go", Prompt: "Go on?", Deadline: deadline},
		{Kind: pipeline.EventGateReleased, Path: "upgrade/go", By: "control", Who: "alice"},
		{Kind: pipeline.EventGateReleased, Path: "upgrade/go", By: "timeout", Policy: pipeline.TimeoutContinue},
	} {
		obs.OnEvent(t.Context(), e)
	}

	assert.Equal(t, `  ⏸ go — Go on? (waiting until 2026-11-12T15:00:00Z)
  ▶ go approved by alice (control)
  ▶ go timed out: continue
`, buf.String())
}

// A stop or cancel already pending when an approval arrives wins: the gate
// is not released, and a cancel outranks a stop.
func TestGate_APendingStopOrCancelOutranksAnApproval(t *testing.T) {
	t.Parallel()

	// What ends the wait is chosen at random among what is ready, so each
	// case runs often enough to lose that draw.
	const tries = 50

	gate := func(a pipeline.Approver) *pipeline.Plan {
		return mustPlan(t,
			pipeline.Gate("go", pipeline.GateOptions{Approvers: []pipeline.Approver{a}}),
			pipeline.Effect("after", ok()),
		)
	}

	t.Run("stop", func(t *testing.T) {
		t.Parallel()

		for range tries {
			stopper := pipeline.NewStopper()
			a := approver{name: "test", wait: func(context.Context, pipeline.GateInfo) (string, error) {
				stopper.Stop("alice")

				return "bob", nil
			}}

			out := runPlan(t, t.Context(), gate(a), pipeline.WithStopper(stopper))

			require.Equal(t, pipeline.ErrStopped, out.err)
			require.Empty(t, kinds(out.events, pipeline.EventGateReleased), "the gate is not released")
			require.Equal(t, pipeline.StatusStopped, out.state.Element("p/go").Status)
		}
	})

	t.Run("cancel", func(t *testing.T) {
		t.Parallel()

		for range tries {
			ctx, cancel := context.WithCancel(t.Context())
			stopper := pipeline.NewStopper()
			a := approver{name: "test", wait: func(context.Context, pipeline.GateInfo) (string, error) {
				// The cancel comes first, so it is pending however soon the
				// stop ends the wait.
				cancel()
				stopper.Stop("alice")

				return "bob", nil
			}}

			out := runPlan(t, ctx, gate(a), pipeline.WithStopper(stopper))

			require.ErrorIs(t, out.err, context.Canceled)
			require.Equal(t, pipeline.OutcomeCancelled, out.res.Outcome)
			require.Empty(t, kinds(out.events, pipeline.EventGateReleased), "the gate is not released")
			require.Equal(t, pipeline.ErrorCancelled, out.state.Element("p/go").Error.Kind)
		}
	})
}

// A crash after a gate is released, before it finishes, doesn't ask again on
// resume. One its timeout declined does wait again, as declining fails it.
func TestGate_ACrashAfterItsReleaseKeepsIt(t *testing.T) {
	t.Parallel()

	t.Run("approved", func(t *testing.T) {
		t.Parallel()

		var asked atomic.Int32

		a := approver{name: "test", wait: func(context.Context, pipeline.GateInfo) (string, error) {
			asked.Add(1)

			return "alice", nil
		}}
		plan := mustPlan(t,
			pipeline.Gate("go", pipeline.GateOptions{Approvers: []pipeline.Approver{a}}),
			pipeline.Effect("after", ok()),
		)

		crash := pipelinetest.CrashAfter("p/go", pipelinetest.GateReleased)
		_, err := pipeline.Run(t.Context(), plan, crash.Journal)
		require.ErrorIs(t, err, pipelinetest.ErrCrashed)

		state, err := crash.Journal.State()
		require.NoError(t, err)
		assert.True(t, state.Element("p/go").Done)

		crash.Restart()

		second := resumeOn(t, crash.Journal, plan)
		require.NoError(t, second.err)
		assert.Equal(t, int32(1), asked.Load())
		assert.Contains(t, trace(second.events), "step.finished p/go skipped/earlier-attempt")
	})

	t.Run("declined", func(t *testing.T) {
		t.Parallel()

		plan := mustPlan(t,
			pipeline.Gate("go", pipeline.GateOptions{
				Approvers: []pipeline.Approver{never(nil)}, Timeout: time.Hour, OnTimeout: pipeline.TimeoutDecline,
			}),
			pipeline.Effect("after", ok()),
		)

		crash := pipelinetest.CrashAfter("p/go", pipelinetest.GateReleased)
		_, err := pipeline.Run(t.Context(), plan, crash.Journal, pipeline.WithClock(readyClock{}))
		require.ErrorIs(t, err, pipelinetest.ErrCrashed)

		state, err := crash.Journal.State()
		require.NoError(t, err)
		assert.False(t, state.Element("p/go").Done)

		crash.Restart()

		second := resumeOn(t, crash.Journal, plan, pipeline.WithClock(readyClock{}))
		require.ErrorIs(t, second.err, pipeline.ErrGateDeclined)
		assert.Contains(t, trace(second.events), "gate.waiting p/go", "the gate waits again")
	})
}
