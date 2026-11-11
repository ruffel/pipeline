package runbook_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ruffel/pipeline/runbook"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recorder is an observer that keeps every event.
type recorder struct {
	mu     sync.Mutex
	events []runbook.Event
}

func (r *recorder) OnEvent(_ context.Context, e runbook.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.events = append(r.events, e)
}

func (r *recorder) all() []runbook.Event {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]runbook.Event(nil), r.events...)
}

// outcome is a run's result, events and replayed state.
type outcome struct {
	res    *runbook.Result
	err    error
	events []runbook.Event
	state  *runbook.State
}

func runPlan(t *testing.T, ctx context.Context, plan *runbook.Plan, opts ...runbook.Option) outcome {
	t.Helper()

	j := runbook.MemoryJournal()
	rec := &recorder{}

	res, err := runbook.Run(ctx, plan, j, append([]runbook.Option{runbook.WithObservers(rec)}, opts...)...)

	journal, jerr := j.Events()
	require.NoError(t, jerr)
	assert.Equal(t, journal, rec.all(), "observers see exactly what the journal stores")

	state, serr := j.State()
	require.NoError(t, serr)

	return outcome{res: res, err: err, events: journal, state: state}
}

func mustPlan(t *testing.T, elems ...runbook.Element) *runbook.Plan {
	t.Helper()

	plan, err := runbook.NewPlan("p", elems...)
	require.NoError(t, err)

	return plan
}

// trace renders events as "kind path [outcome/reason]", leaving out the run
// events and chatter unless asked.
func trace(events []runbook.Event, kinds ...runbook.EventKind) []string {
	want := map[runbook.EventKind]bool{
		runbook.EventGroupStarted: true, runbook.EventGroupFinished: true,
		runbook.EventStepStarted: true, runbook.EventStepRunning: true, runbook.EventStepFinished: true,
	}
	for _, k := range kinds {
		want[k] = true
	}

	var out []string

	for _, e := range events {
		if !want[e.Kind] {
			continue
		}

		line := string(e.Kind) + " " + e.Path

		switch {
		case e.Outcome != "" && e.Reason != "":
			line += " " + string(e.Outcome) + "/" + string(e.Reason)
		case e.Outcome != "":
			line += " " + string(e.Outcome)
		case e.Message != "":
			line += " " + e.Message
		}

		out = append(out, line)
	}

	return out
}

func ok() runbook.Step { return noop() }

func fail(msg string) runbook.Step {
	return runbook.StepFunc(func(context.Context, *runbook.StepContext) error { return errors.New(msg) })
}

// waitForCancel blocks until its context is done.
func waitForCancel() runbook.Step {
	return runbook.StepFunc(func(ctx context.Context, _ *runbook.StepContext) error {
		<-ctx.Done()

		return ctx.Err()
	})
}

// checkStep has a Check that reports done, and counts calls.
type checkStep struct {
	done     func() bool
	run      func(ctx context.Context, s *runbook.StepContext) error
	runCalls atomic.Int32
}

func (c *checkStep) Check(context.Context, *runbook.StepContext) (bool, error) { return c.done(), nil }

func (c *checkStep) Run(ctx context.Context, s *runbook.StepContext) error {
	c.runCalls.Add(1)

	if c.run != nil {
		return c.run(ctx, s)
	}

	return nil
}

// -----------------------------------------------------------------------------
// Ordering, groups and outcomes
// -----------------------------------------------------------------------------

func TestRun_SequentialStepsInOrder(t *testing.T) {
	t.Parallel()

	var order []string

	step := func(name string) runbook.StepNode {
		return runbook.Effect(name, runbook.StepFunc(func(context.Context, *runbook.StepContext) error {
			order = append(order, name)

			return nil
		}))
	}

	plan := mustPlan(t, step("a"), runbook.Group("g", step("b"), step("c")), step("d"))
	out := runPlan(t, t.Context(), plan, runbook.WithRunLabels(runbook.L{"operation": "u-42"}))

	require.NoError(t, out.err)
	assert.Equal(t, []string{"a", "b", "c", "d"}, order)
	assert.Equal(t, runbook.OutcomePassed, out.res.Outcome)
	assert.Equal(t, 1, out.res.Attempt)
	assert.NotEmpty(t, out.res.Run)

	assert.Equal(t, []string{
		"step.started p/a", "step.running p/a", "step.finished p/a passed",
		"group.started p/g",
		"step.started p/g/b", "step.running p/g/b", "step.finished p/g/b passed",
		"step.started p/g/c", "step.running p/g/c", "step.finished p/g/c passed",
		"group.finished p/g passed",
		"step.started p/d", "step.running p/d", "step.finished p/d passed",
	}, trace(out.events))

	first, last := out.events[0], out.events[len(out.events)-1]
	assert.Equal(t, runbook.EventRunStarted, first.Kind)
	assert.Equal(t, plan.Fingerprint(), first.Fingerprint)
	assert.Equal(t, runbook.L{"operation": "u-42"}, first.Labels)
	require.NotNil(t, first.Plan)
	assert.Equal(t, plan.Spec(), *first.Plan)
	assert.Equal(t, runbook.EventRunFinished, last.Kind)
	assert.Equal(t, runbook.OutcomePassed, last.Outcome)

	for _, e := range out.events {
		assert.Equal(t, out.res.Run, e.Run, "every event carries the run ID")
	}
}

func TestRun_FailureStopsASequentialGroup(t *testing.T) {
	t.Parallel()

	out := runPlan(t, t.Context(), mustPlan(t,
		runbook.Effect("a", ok()),
		runbook.Effect("b", fail("boom")),
		runbook.Effect("c", ok()),
	))

	require.Error(t, out.err)
	assert.Equal(t, runbook.OutcomeFailed, out.res.Outcome)

	var se *runbook.StepError
	require.ErrorAs(t, out.err, &se)
	assert.Equal(t, "p/b", se.Path)
	assert.Equal(t, "p/b: boom", out.err.Error())

	assert.Equal(t, runbook.StatusPassed, out.state.Element("p/a").Status)
	assert.Equal(t, runbook.StatusFailed, out.state.Element("p/b").Status)
	assert.Equal(t, runbook.StatusNotReached, out.state.Element("p/c").Status)
	assert.Equal(t, &runbook.ErrorInfo{Kind: runbook.ErrorPlain, Message: "boom"}, out.state.Element("p/b").Error)
}

func TestRun_ContinueOnErrorRunsEveryChild(t *testing.T) {
	t.Parallel()

	out := runPlan(t, t.Context(), mustPlan(t,
		runbook.Group("g",
			runbook.Effect("a", fail("first")),
			runbook.Effect("b", ok()),
			runbook.Effect("c", fail("second")),
		).ContinueOnError(),
		runbook.Effect("after", ok()),
	))

	require.Error(t, out.err)
	assert.Equal(t, runbook.StatusPassed, out.state.Element("p/g/b").Status)
	assert.Equal(t, runbook.StatusFailed, out.state.Element("p/g/c").Status)
	assert.Equal(t, runbook.StatusFailed, out.state.Element("p/g").Status)
	assert.Equal(t, runbook.StatusNotReached, out.state.Element("p/after").Status, "the group failed, so the root stops")
	require.ErrorContains(t, out.err, "p/g/a: first")
	require.ErrorContains(t, out.err, "p/g/c: second")
}

func TestRun_StepErrorUnwrapsToTheStepsOwnError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("sentinel")

	out := runPlan(t, t.Context(), mustPlan(t, runbook.Effect("a", runbook.StepFunc(
		func(context.Context, *runbook.StepContext) error { return fmt.Errorf("wrapped: %w", sentinel) }))))

	require.ErrorIs(t, out.err, sentinel)
}

// -----------------------------------------------------------------------------
// Parallel groups
// -----------------------------------------------------------------------------

func TestRun_ParallelLimitIsHonoured(t *testing.T) {
	t.Parallel()

	var running, peak atomic.Int32

	step := runbook.StepFunc(func(context.Context, *runbook.StepContext) error {
		now := running.Add(1)
		defer running.Add(-1)

		for {
			p := peak.Load()
			if now <= p || peak.CompareAndSwap(p, now) {
				break
			}
		}

		time.Sleep(20 * time.Millisecond)

		return nil
	})

	children := make([]runbook.Element, 0, 6)
	for i := range 6 {
		children = append(children, runbook.Effect(fmt.Sprintf("s%d", i), step))
	}

	out := runPlan(t, t.Context(), mustPlan(t, runbook.Group("g", children...).Parallel(2)))

	require.NoError(t, out.err)
	assert.Equal(t, int32(2), peak.Load())
}

func TestRun_ParallelUnlimitedRunsTogether(t *testing.T) {
	t.Parallel()

	var arrived sync.WaitGroup

	arrived.Add(3)

	// Each step waits for all three to start, which only works if they run at
	// the same time.
	step := runbook.StepFunc(func(context.Context, *runbook.StepContext) error {
		arrived.Done()
		arrived.Wait()

		return nil
	})

	out := runPlan(t, t.Context(), mustPlan(t, runbook.Group("g",
		runbook.Effect("a", step), runbook.Effect("b", step), runbook.Effect("c", step)).Parallel(0)))

	require.NoError(t, out.err)
}

// A3: with a limit, children still queued are not started after a sibling
// has failed. They are not reached.
func TestRun_A3_QueuedChildrenDoNotStartAfterAFailure(t *testing.T) {
	t.Parallel()

	out := runPlan(t, t.Context(), mustPlan(t, runbook.Group("g",
		runbook.Effect("fail", fail("x")),
		runbook.Effect("b", ok()),
		runbook.Effect("c", ok()),
	).Parallel(1)))

	require.Error(t, out.err)
	assert.Equal(t, runbook.StatusNotReached, out.state.Element("p/g/b").Status)
	assert.Equal(t, runbook.StatusNotReached, out.state.Element("p/g/c").Status)
}

// B6: the same with ContinueOnError, after a cancel.
func TestRun_B6_QueuedChildrenDoNotStartAfterACancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())

	out := runPlan(t, ctx, mustPlan(t, runbook.Group("g",
		runbook.Effect("a", runbook.StepFunc(func(context.Context, *runbook.StepContext) error {
			cancel()

			return nil
		})),
		runbook.Effect("b", ok()),
		runbook.Effect("c", ok()),
	).Parallel(1).ContinueOnError()))

	require.ErrorIs(t, out.err, context.Canceled)
	assert.Equal(t, runbook.OutcomeCancelled, out.res.Outcome)
	assert.Equal(t, runbook.StatusPassed, out.state.Element("p/g/a").Status)
	assert.Equal(t, runbook.StatusNotReached, out.state.Element("p/g/b").Status)
	assert.Equal(t, runbook.StatusNotReached, out.state.Element("p/g/c").Status)
}

func TestRun_FailureLetsRunningSiblingsFinish(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	finished := make(chan struct{})

	out := runPlan(t, t.Context(), mustPlan(t, runbook.Group("g",
		runbook.Effect("slow", runbook.StepFunc(func(ctx context.Context, _ *runbook.StepContext) error {
			<-release

			defer close(finished)

			return ctx.Err()
		})),
		runbook.Effect("fail", runbook.StepFunc(func(context.Context, *runbook.StepContext) error {
			defer close(release)

			return errors.New("x")
		})),
	).Parallel(0)))

	<-finished
	require.Error(t, out.err)
	assert.Equal(t, runbook.StatusPassed, out.state.Element("p/g/slow").Status, "not cancelled by its sibling's failure")
}

func TestRun_CancelOnErrorCancelsRunningSiblings(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})

	out := runPlan(t, t.Context(), mustPlan(t, runbook.Group("g",
		runbook.Effect("waiter", runbook.StepFunc(func(ctx context.Context, _ *runbook.StepContext) error {
			close(started)
			<-ctx.Done()

			return ctx.Err()
		})),
		runbook.Effect("fail", runbook.StepFunc(func(context.Context, *runbook.StepContext) error {
			<-started

			return errors.New("x")
		})),
	).Parallel(0).CancelOnError()))

	require.Error(t, out.err)

	waiter := out.state.Element("p/g/waiter")
	assert.Equal(t, runbook.StatusFailed, waiter.Status)
	assert.Equal(t, runbook.ErrorCancelled, waiter.Error.Kind)
	assert.Equal(t, runbook.OutcomeFailed, out.res.Outcome, "a group's own cancel is a failure, not a cancelled run")
}

// -----------------------------------------------------------------------------
// Check, tries and timeouts
// -----------------------------------------------------------------------------

func TestRun_CheckBeforeRun(t *testing.T) {
	t.Parallel()

	done := &checkStep{done: func() bool { return true }}
	todo := &checkStep{done: func() bool { return false }}

	out := runPlan(t, t.Context(), mustPlan(t, runbook.Effect("done", done), runbook.Effect("todo", todo)))

	require.NoError(t, out.err)
	assert.Equal(t, int32(0), done.runCalls.Load(), "Check said done, so Run is not called")
	assert.Equal(t, int32(1), todo.runCalls.Load())
	assert.Equal(t, []string{
		"step.started p/done", "step.finished p/done skipped/done",
		"step.started p/todo", "step.running p/todo", "step.finished p/todo passed",
	}, trace(out.events))
	assert.False(t, out.state.Element("p/done").RunCalled)
	assert.True(t, out.state.Element("p/done").Done)
}

func TestRun_RetryRecoversWhenCheckFindsTheWorkDone(t *testing.T) {
	t.Parallel()

	var applied atomic.Bool

	step := &checkStep{
		done: applied.Load,
		run: func(context.Context, *runbook.StepContext) error {
			applied.Store(true)

			return errors.New("connection reset after applying")
		},
	}

	out := runPlan(t, t.Context(), mustPlan(t, runbook.Effect("a", step).Retry(3, time.Millisecond)))

	require.NoError(t, out.err)
	assert.Equal(t, int32(1), step.runCalls.Load())
	assert.Equal(t, []string{
		"step.started p/a", "step.running p/a",
		"step.retry p/a",
		"step.finished p/a passed/recovered",
	}, trace(out.events, runbook.EventStepRetry))
}

func TestRun_RetriesThenFails(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	out := runPlan(t, t.Context(), mustPlan(t, runbook.Effect("a", runbook.StepFunc(
		func(context.Context, *runbook.StepContext) error {
			return fmt.Errorf("try %d", calls.Add(1))
		})).Retry(3, time.Millisecond)))

	require.Error(t, out.err)
	assert.Equal(t, int32(3), calls.Load())
	assert.Equal(t, "p/a: try 3", out.err.Error())

	var retries []int

	for _, e := range out.events {
		if e.Kind == runbook.EventStepRetry {
			retries = append(retries, e.Try)
		}
	}

	assert.Equal(t, []int{1, 2}, retries)
}

func TestRun_TimeoutBoundsEachTry(t *testing.T) {
	t.Parallel()

	out := runPlan(t, t.Context(), mustPlan(t, runbook.Effect("a", waitForCancel()).Timeout(10*time.Millisecond)))

	require.ErrorIs(t, out.err, context.DeadlineExceeded)
	assert.Equal(t, runbook.ErrorTimeout, out.state.Element("p/a").Error.Kind)
	assert.Equal(t, runbook.OutcomeFailed, out.res.Outcome)
}

func TestRun_RunDoesNotStartWhenCheckUsedUpTheTry(t *testing.T) {
	t.Parallel()

	step := &checkStep{done: func() bool { return false }}
	slowCheck := &slowChecker{checkStep: step}

	out := runPlan(t, t.Context(), mustPlan(t, runbook.Effect("a", slowCheck).Timeout(10*time.Millisecond)))

	require.ErrorIs(t, out.err, context.DeadlineExceeded)
	assert.Equal(t, int32(0), step.runCalls.Load(), "Run must not start after the try's time ran out")
	assert.False(t, out.state.Element("p/a").RunCalled)
}

// slowChecker's Check outlasts its context, then reports not done.
type slowChecker struct{ *checkStep }

func (c *slowChecker) Check(ctx context.Context, _ *runbook.StepContext) (bool, error) {
	<-ctx.Done()

	return false, nil
}

func TestRun_CheckErrorFailsTheStep(t *testing.T) {
	t.Parallel()

	step := &erroringCheck{}
	out := runPlan(t, t.Context(), mustPlan(t, runbook.Effect("a", step)))

	require.ErrorContains(t, out.err, "p/a: cannot tell")
	assert.False(t, step.ran)
}

type erroringCheck struct{ ran bool }

func (e *erroringCheck) Check(context.Context, *runbook.StepContext) (bool, error) {
	return false, errors.New("cannot tell")
}

func (e *erroringCheck) Run(context.Context, *runbook.StepContext) error {
	e.ran = true

	return nil
}

// -----------------------------------------------------------------------------
// Panics (A2, A5, B1)
// -----------------------------------------------------------------------------

type panicCheck struct{}

func (panicCheck) Check(context.Context, *runbook.StepContext) (bool, error) { panic("boom") }
func (panicCheck) Run(context.Context, *runbook.StepContext) error           { return nil }

// A2 and B1: a panic in Check fails that step and is recorded. Nothing
// escapes Run, and nothing kills the process from a parallel group.
func TestRun_A2_B1_PanicInCheckFailsTheStep(t *testing.T) {
	t.Parallel()

	for _, parallel := range []bool{false, true} {
		t.Run(fmt.Sprintf("parallel=%v", parallel), func(t *testing.T) {
			t.Parallel()

			g := runbook.Group("g", runbook.Effect("a", panicCheck{}), runbook.Effect("b", ok()))
			if parallel {
				g = g.Parallel(0).ContinueOnError()
			}

			out := runPlan(t, t.Context(), mustPlan(t, g))

			var pe *runbook.PanicError
			require.ErrorAs(t, out.err, &pe)
			assert.Equal(t, "boom", pe.Value)

			el := out.state.Element("p/g/a")
			assert.Equal(t, runbook.StatusFailed, el.Status)
			assert.Equal(t, runbook.ErrorPanic, el.Error.Kind)
		})
	}
}

// A5: the error from a panic has the step's path and a type, and the stack is
// recorded separately instead of in the message.
func TestRun_A5_PanicErrorHasPathAndKeepsTheStackOutOfTheMessage(t *testing.T) {
	t.Parallel()

	out := runPlan(t, t.Context(), mustPlan(t, runbook.Effect("a", runbook.StepFunc(
		func(context.Context, *runbook.StepContext) error { panic("boom") }))))

	assert.Equal(t, "p/a: panic: boom", out.err.Error())

	info := out.state.Element("p/a").Error
	assert.Equal(t, "panic: boom", info.Message)
	assert.Contains(t, info.Stack, "goroutine", "the stack is recorded")
	assert.NotContains(t, info.Message, "\n")
}

// -----------------------------------------------------------------------------
// Events from steps (A6, B5)
// -----------------------------------------------------------------------------

type chattyCheck struct{}

func (chattyCheck) Check(_ context.Context, s *runbook.StepContext) (bool, error) {
	s.Info("from check")

	return false, nil
}

func (chattyCheck) Run(context.Context, *runbook.StepContext) error { return nil }

// A6: Check runs after step.started, so nothing it emits comes first.
func TestRun_A6_CheckRunsAfterTheStepStarts(t *testing.T) {
	t.Parallel()

	out := runPlan(t, t.Context(), mustPlan(t, runbook.Effect("a", chattyCheck{})))

	require.NoError(t, out.err)
	assert.Equal(t, []string{
		"step.started p/a", "message p/a from check", "step.running p/a", "step.finished p/a passed",
	}, trace(out.events, runbook.EventMessage))
}

// B5: a StepContext stops working when its step returns.
func TestRun_B5_StepContextIsInertAfterTheStepReturns(t *testing.T) {
	t.Parallel()

	leaked := make(chan *runbook.StepContext, 1)

	out := runPlan(t, t.Context(), mustPlan(t,
		runbook.Effect("a", runbook.StepFunc(func(_ context.Context, s *runbook.StepContext) error {
			leaked <- s

			return nil
		})),
		runbook.Effect("b", runbook.StepFunc(func(context.Context, *runbook.StepContext) error {
			s := <-leaked
			s.Info("late")

			if err := s.Record("k", 1); !errors.Is(err, runbook.ErrStepReturned) {
				return fmt.Errorf("Record after return: %w", err)
			}

			if _, err := s.Recorded("p/a", "k", new(int)); !errors.Is(err, runbook.ErrStepReturned) {
				return fmt.Errorf("Recorded after return: %w", err)
			}

			return nil
		})),
	))

	require.NoError(t, out.err)

	for _, e := range out.events {
		assert.NotEqual(t, "late", e.Message)
	}
}

func TestRun_StepContextBasics(t *testing.T) {
	t.Parallel()

	var path, attempt string

	var labels runbook.L

	out := runPlan(t, t.Context(), mustPlan(t, runbook.Group("g",
		runbook.Effect("a", runbook.StepFunc(func(_ context.Context, s *runbook.StepContext) error {
			path, attempt, labels = s.Path(), strconv.Itoa(s.Attempt()), s.Labels()
			s.Warn("careful")
			s.Progress("copy", "copying", 3, 10)

			return nil
		})).Labels(runbook.L{"node": "n1"}),
	).Labels(runbook.L{"dc": "DC1"})))

	require.NoError(t, out.err)
	assert.Equal(t, "p/g/a", path)
	assert.Equal(t, "1", attempt)
	assert.Equal(t, runbook.L{"dc": "DC1", "node": "n1"}, labels)

	var sawWarn, sawProgress bool

	for _, e := range out.events {
		if e.Kind == runbook.EventMessage && e.Level == "warn" && e.Message == "careful" && e.Path == "p/g/a" {
			sawWarn = true
		}

		if e.Kind == runbook.EventProgress && e.Key == "copy" && e.Current == 3 && e.Total == 10 {
			sawProgress = true
		}
	}

	assert.True(t, sawWarn)
	assert.True(t, sawProgress)
}

func TestStepContext_ZeroValueIsInert(t *testing.T) {
	t.Parallel()

	var s runbook.StepContext

	s.Info("nothing happens")
	assert.Empty(t, s.Path())
	require.ErrorIs(t, s.Record("k", 1), runbook.ErrStepReturned)

	_, err := s.Recorded("p/a", "k", new(int))
	require.ErrorIs(t, err, runbook.ErrStepReturned)
	require.ErrorIs(t, s.Item("n1", runbook.ItemPassed, ""), runbook.ErrStepReturned)
	assert.Empty(t, s.PreviousItems())
}

// -----------------------------------------------------------------------------
// Recorded values, items and custom events
// -----------------------------------------------------------------------------

func TestRun_RecordedValuesAreReadableByLaterSteps(t *testing.T) {
	t.Parallel()

	var got string

	var found bool

	out := runPlan(t, t.Context(), mustPlan(t,
		runbook.Effect("backup", runbook.StepFunc(func(_ context.Context, s *runbook.StepContext) error {
			return s.Record("dir", "/var/backups/42")
		})),
		runbook.Effect("restore", runbook.StepFunc(func(_ context.Context, s *runbook.StepContext) error {
			var err error

			found, err = s.Recorded("p/backup", "dir", &got)
			if err != nil {
				return err
			}

			missing, err := s.Recorded("p/backup", "nope", &got)
			if missing || err != nil {
				return errors.New("a missing value should report not found")
			}

			return nil
		})),
	))

	require.NoError(t, out.err)
	assert.True(t, found)
	assert.Equal(t, "/var/backups/42", got)

	v, ok := out.state.Value("p/backup", "dir")
	require.True(t, ok)
	assert.JSONEq(t, `"/var/backups/42"`, string(v))
}

func TestRun_RecordRejectsWhatItCannotStore(t *testing.T) {
	t.Parallel()

	var errs []error

	out := runPlan(t, t.Context(), mustPlan(t, runbook.Effect("a", runbook.StepFunc(
		func(_ context.Context, s *runbook.StepContext) error {
			errs = append(errs,
				s.Record("", 1),
				s.Record("nan", math.NaN()),
				s.Record("big", strings.Repeat("x", runbook.MaxRecordBytes)),
				s.Record("fits", strings.Repeat("x", runbook.MaxRecordBytes/2)),
				s.Record("over", strings.Repeat("x", runbook.MaxRecordBytes/2)),
			)

			return nil
		}))))

	require.NoError(t, out.err)
	require.ErrorContains(t, errs[0], "record key is empty")
	require.ErrorContains(t, errs[1], `record "nan"`)
	require.ErrorContains(t, errs[2], "would exceed")
	require.NoError(t, errs[3])
	require.ErrorContains(t, errs[4], "would exceed", "the limit is per step, not per value")

	_, ok := out.state.Value("p/a", "nan")
	assert.False(t, ok, "a rejected value is not recorded")
}

func TestRun_Items(t *testing.T) {
	t.Parallel()

	var badStatus, previous error

	out := runPlan(t, t.Context(), mustPlan(t, runbook.Effect("stage", runbook.StepFunc(
		func(_ context.Context, s *runbook.StepContext) error {
			if len(s.PreviousItems()) != 0 {
				previous = errors.New("a first attempt has no previous items")
			}

			badStatus = s.Item("node9", "maybe", "")

			if err := s.Item("node7", runbook.ItemFailed, "timeout"); err != nil {
				return err
			}

			return s.Item("node8", runbook.ItemPassed, "")
		}))))

	require.NoError(t, out.err)
	require.NoError(t, previous)
	require.ErrorContains(t, badStatus, `unknown item status "maybe"`)
	assert.Equal(t, map[string]runbook.ItemState{
		"node7": {Status: runbook.ItemFailed, Message: "timeout", Run: out.res.Run},
		"node8": {Status: runbook.ItemPassed, Run: out.res.Run},
	}, out.state.Items("p/stage"))
}

func TestRun_CustomEvents(t *testing.T) {
	t.Parallel()

	var errs []error

	out := runPlan(t, t.Context(), mustPlan(t, runbook.Effect("a", runbook.StepFunc(
		func(_ context.Context, s *runbook.StepContext) error {
			errs = append(errs,
				s.Custom("deploy.endpoint-ready", map[string]any{"url": "https://x", "port": 443}),
				s.Custom("bad type", 1),
				s.Custom("x.nan", math.NaN()),
				s.Custom("x.big", strings.Repeat("x", runbook.MaxCustomBytes)),
			)

			return nil
		}))))

	require.NoError(t, out.err)
	require.NoError(t, errs[0])
	require.ErrorContains(t, errs[1], "custom event type")
	require.ErrorContains(t, errs[2], `custom event "x.nan"`)
	require.ErrorContains(t, errs[3], "over the")

	var customs []runbook.Event

	for _, e := range out.events {
		if e.Kind == runbook.EventCustom {
			customs = append(customs, e)
		}
	}

	require.Len(t, customs, 1, "rejected custom events record nothing")
	assert.Equal(t, "deploy.endpoint-ready", customs[0].Type)
	assert.Equal(t, "p/a", customs[0].Path)
	assert.JSONEq(t, `{"url": "https://x", "port": 443}`, string(customs[0].Data))
}

// -----------------------------------------------------------------------------
// Cancellation (A7)
// -----------------------------------------------------------------------------

// A7: steps not reached after a cancel are reported as not reached, and the
// run as cancelled.
func TestRun_A7_CancelLeavesEveryElementResolved(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())

	out := runPlan(t, ctx, mustPlan(t,
		runbook.Effect("a", runbook.StepFunc(func(context.Context, *runbook.StepContext) error {
			cancel()

			return nil
		})),
		runbook.Effect("b", ok()),
		runbook.Group("g", runbook.Effect("c", ok())),
	))

	require.ErrorIs(t, out.err, context.Canceled)
	assert.Equal(t, runbook.OutcomeCancelled, out.res.Outcome)

	for path, want := range map[string]runbook.Status{
		"p/a":   runbook.StatusPassed,
		"p/b":   runbook.StatusNotReached,
		"p/g":   runbook.StatusNotReached,
		"p/g/c": runbook.StatusNotReached,
	} {
		assert.Equal(t, want, out.state.Element(path).Status, path)
	}

	require.Len(t, out.state.Attempts, 1)
	assert.Equal(t, runbook.OutcomeCancelled, out.state.Attempts[0].Outcome)
	assert.True(t, out.state.Unfinished())
}

func TestRun_CancelDuringAStepFailsItAsCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())

	out := runPlan(t, ctx, mustPlan(t, runbook.Effect("a", runbook.StepFunc(
		func(ctx context.Context, _ *runbook.StepContext) error {
			cancel()
			<-ctx.Done()

			return ctx.Err()
		})).Retry(3, time.Hour)))

	require.ErrorIs(t, out.err, context.Canceled)
	assert.Equal(t, runbook.ErrorCancelled, out.state.Element("p/a").Error.Kind)
	assert.Equal(t, runbook.OutcomeCancelled, out.res.Outcome)
}

func TestRun_CancelAfterAFailureLeavesTheRunFailed(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())

	// The cancel arrives as the failure is reported, after it interrupted
	// nothing.
	cancelOnFinish := runbook.ObserverFunc(func(_ context.Context, e runbook.Event) {
		if e.Kind == runbook.EventStepFinished {
			cancel()
		}
	})

	out := runPlan(t, ctx, mustPlan(t, runbook.Effect("a", fail("boom"))), runbook.WithObservers(cancelOnFinish))

	require.ErrorContains(t, out.err, "p/a: boom")
	assert.Equal(t, runbook.OutcomeFailed, out.res.Outcome)
}

func TestRun_CancelAfterTheLastStepLeavesTheRunPassed(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())

	out := runPlan(t, ctx, mustPlan(t, runbook.Effect("a", runbook.StepFunc(
		func(context.Context, *runbook.StepContext) error {
			cancel()

			return nil
		}))))

	require.NoError(t, out.err)
	assert.Equal(t, runbook.OutcomePassed, out.res.Outcome)
}

// -----------------------------------------------------------------------------
// Observers (B2)
// -----------------------------------------------------------------------------

// B2: an observer panic is recovered outside the step, so the step still
// passes. The observer is dropped and a warning recorded.
func TestRun_B2_ObserverPanicDoesNotFailTheStep(t *testing.T) {
	t.Parallel()

	var badCalls atomic.Int32

	bad := runbook.ObserverFunc(func(_ context.Context, e runbook.Event) {
		badCalls.Add(1)

		if e.Kind == runbook.EventStepRunning {
			panic("renderer bug")
		}
	})

	out := runPlan(t, t.Context(), mustPlan(t, runbook.Effect("a", ok()), runbook.Effect("b", ok())),
		runbook.WithObservers(bad))

	require.NoError(t, out.err)
	assert.Equal(t, runbook.StatusPassed, out.state.Element("p/a").Status)
	assert.Equal(t, int32(3), badCalls.Load(), "run.started, step.started, then the panic; nothing after")

	var warned bool

	for _, e := range out.events {
		if e.Kind == runbook.EventMessage && e.Level == "warn" && strings.Contains(e.Message, "renderer bug") {
			warned = true
		}
	}

	assert.True(t, warned)
}

func TestRun_ObserverCallsAreSerialised(t *testing.T) {
	t.Parallel()

	var inside, overlaps atomic.Int32

	obs := runbook.ObserverFunc(func(context.Context, runbook.Event) {
		if inside.Add(1) > 1 {
			overlaps.Add(1)
		}

		time.Sleep(time.Millisecond)
		inside.Add(-1)
	})

	chatty := runbook.StepFunc(func(_ context.Context, s *runbook.StepContext) error {
		for i := range 5 {
			s.Info(strconv.Itoa(i))
		}

		return nil
	})

	out := runPlan(t, t.Context(), mustPlan(t, runbook.Group("g",
		runbook.Effect("a", chatty), runbook.Effect("b", chatty), runbook.Effect("c", chatty)).Parallel(0)),
		runbook.WithObservers(obs))

	require.NoError(t, out.err)
	assert.Equal(t, int32(0), overlaps.Load())
}

// -----------------------------------------------------------------------------
// Journal rules
// -----------------------------------------------------------------------------

func TestRun_RefusesAJournalWithAttempts(t *testing.T) {
	t.Parallel()

	j := runbook.MemoryJournal()
	plan := mustPlan(t, runbook.Effect("a", ok()))

	_, err := runbook.Run(t.Context(), plan, j)
	require.NoError(t, err)

	_, err = runbook.Run(t.Context(), plan, j)
	require.ErrorIs(t, err, runbook.ErrResumeNotSupported)
}

func TestRun_TimestampsComeFromTheClock(t *testing.T) {
	t.Parallel()

	clock := &tickingClock{now: time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)}

	out := runPlan(t, t.Context(), mustPlan(t, runbook.Effect("a", ok())), runbook.WithClock(clock))

	require.NoError(t, out.err)

	for _, e := range out.events {
		assert.False(t, e.TS.Before(clock.now), "timestamps come from the injected clock")
		assert.Equal(t, time.UTC, e.TS.Location())
	}

	finished := out.events[len(out.events)-2]
	require.Equal(t, runbook.EventStepFinished, finished.Kind)
	assert.Positive(t, finished.DurationMs)
}

// tickingClock advances a second every time it is read.
type tickingClock struct {
	mu   sync.Mutex
	now  time.Time
	read int
}

func (c *tickingClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.read++

	return c.now.Add(time.Duration(c.read-1) * time.Second)
}

func (c *tickingClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// -----------------------------------------------------------------------------
// Output
// -----------------------------------------------------------------------------

// outputOf returns "stream: line" for every output event, and the warnings.
func outputOf(events []runbook.Event) ([]string, []string) {
	var lines, warnings []string

	for _, e := range events {
		switch {
		case e.Kind == runbook.EventOutput:
			lines = append(lines, e.Stream+": "+e.Line)
		case e.Kind == runbook.EventMessage && e.Level == "warn":
			warnings = append(warnings, e.Message)
		}
	}

	return lines, warnings
}

func TestRun_StdoutAndStderrAreRecordedLineByLine(t *testing.T) {
	t.Parallel()

	out := runPlan(t, t.Context(), mustPlan(t, runbook.Effect("a", runbook.StepFunc(
		func(_ context.Context, s *runbook.StepContext) error {
			_, _ = io.WriteString(s.Stdout(), "one\r\ntw")
			_, _ = io.WriteString(s.Stderr(), "oops\n")
			_, _ = io.WriteString(s.Stdout(), "o\n\r10%\r20%\rno newline")

			return nil
		}))))

	require.NoError(t, out.err)

	lines, warnings := outputOf(out.events)
	assert.Equal(t, []string{
		"stdout: one", "stderr: oops", "stdout: two", "stdout: 10%", "stdout: 20%", "stdout: no newline",
	}, lines)
	assert.Empty(t, warnings)

	// The unterminated tail is recorded before the step finishes.
	tail := slices.IndexFunc(out.events, func(e runbook.Event) bool { return e.Line == "no newline" })
	finished := slices.IndexFunc(out.events, func(e runbook.Event) bool { return e.Kind == runbook.EventStepFinished })
	assert.Less(t, tail, finished)
}

func TestRun_SubprocessOutputWithAnOverLongLine(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("needs sh")
	}

	out := runPlan(t, t.Context(), mustPlan(t, runbook.Effect("a", runbook.StepFunc(
		func(ctx context.Context, s *runbook.StepContext) error {
			cmd := exec.CommandContext(ctx, "sh", "-c",
				`echo before; head -c 40000 /dev/zero | tr '\0' a; echo; echo after >&2`)
			cmd.Stdout, cmd.Stderr = s.Stdout(), s.Stderr()

			return cmd.Run()
		}))))

	require.NoError(t, out.err)

	var pieces []runbook.Event

	for _, e := range out.events {
		if e.Kind == runbook.EventOutput && strings.HasPrefix(e.Line, "aaa") {
			pieces = append(pieces, e)
		}
	}

	require.Len(t, pieces, 3, "40000 bytes in pieces of 16 KiB")
	assert.False(t, pieces[0].Continued)
	assert.True(t, pieces[1].Continued)
	assert.True(t, pieces[2].Continued)
	assert.Len(t, pieces[0].Line, runbook.MaxOutputLine)

	lines, warnings := outputOf(out.events)
	assert.Equal(t, "stdout: before", lines[0])
	assert.Contains(t, lines, "stderr: after")
	assert.Len(t, warnings, 1, "one warning for the one long line")
}

func TestRun_WritesAfterTheStepReturnsFail(t *testing.T) {
	t.Parallel()

	leaked := make(chan io.Writer, 1)

	out := runPlan(t, t.Context(), mustPlan(t,
		runbook.Effect("a", runbook.StepFunc(func(_ context.Context, s *runbook.StepContext) error {
			leaked <- s.Stdout()

			return nil
		})),
		runbook.Effect("b", runbook.StepFunc(func(context.Context, *runbook.StepContext) error {
			if _, err := io.WriteString(<-leaked, "late\n"); !errors.Is(err, runbook.ErrStepReturned) {
				return fmt.Errorf("write after return: %w", err)
			}

			return nil
		})),
	))

	require.NoError(t, out.err)

	lines, _ := outputOf(out.events)
	assert.Empty(t, lines)

	var zero runbook.StepContext

	_, err := zero.Stdout().Write([]byte("x\n"))
	require.ErrorIs(t, err, runbook.ErrStepReturned)
}

// A write the step's writer accepts is always recorded, even when it races
// the step returning: a write either fails with ErrStepReturned or appears.
func TestRun_AcceptedWritesAreNeverLost(t *testing.T) {
	t.Parallel()

	for range 20 {
		var accepted atomic.Int32

		writerDone := make(chan struct{})

		out := runPlan(t, t.Context(), mustPlan(t, runbook.Effect("a", runbook.StepFunc(
			func(_ context.Context, s *runbook.StepContext) error {
				w := s.Stdout()

				go func() {
					defer close(writerDone)

					for {
						if _, err := io.WriteString(w, "line\n"); err != nil {
							return
						}

						accepted.Add(1)
					}
				}()

				time.Sleep(time.Millisecond)

				return nil
			}))))

		<-writerDone
		require.NoError(t, out.err)

		lines, _ := outputOf(out.events)
		require.Len(t, lines, int(accepted.Load()))
	}
}
