package pipelinetest_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ruffel/pipeline"
	"github.com/ruffel/pipeline/pipelinetest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// work is a step with Check, Run that records a value and an item, and a
// counter of Run calls.
type work struct{ runs *int }

func (w work) Check(context.Context, *pipeline.StepContext) (bool, error) { return false, nil }

func (w work) Run(_ context.Context, s *pipeline.StepContext) error {
	*w.runs++

	if err := s.Record("k", "v"); err != nil {
		return err
	}

	return s.Item("n", pipeline.ItemPassed, "")
}

func tenSteps(t *testing.T, runs *int) *pipeline.Plan {
	t.Helper()

	steps := make([]pipeline.Element, 0, 10)
	for i := range 10 {
		steps = append(steps, pipeline.Effect(fmt.Sprintf("s%d", i), work{runs: runs}))
	}

	plan, err := pipeline.NewPlan("p", pipeline.Group("g", steps...))
	require.NoError(t, err)

	return plan
}

// Crashing at every point of every step of a 10-step plan leaves a journal
// that ends at the crash and replays to where the run stood.
func TestCrashAfter_EveryPointOfATenStepPlan(t *testing.T) {
	t.Parallel()

	points := []pipelinetest.Point{
		pipelinetest.Started, pipelinetest.Running, pipelinetest.Value("k"), pipelinetest.Item("n"), pipelinetest.Finished,
	}

	for i := range 10 {
		for p, point := range points {
			path := fmt.Sprintf("p/g/s%d", i)

			t.Run(fmt.Sprintf("%s/%s", path, point), func(t *testing.T) {
				t.Parallel()

				runs := 0
				crash := pipelinetest.CrashAfter(path, point)

				_, err := pipeline.Run(t.Context(), tenSteps(t, &runs), crash.Journal)
				require.ErrorIs(t, err, pipelinetest.ErrCrashed)
				assert.True(t, crash.Crashed())

				events, err := crash.Journal.Events()
				require.NoError(t, err)

				last := events[len(events)-1]
				assert.Equal(t, path, last.Path, "the journal ends at the crash")

				state, err := crash.Journal.State()
				require.NoError(t, err)
				assert.True(t, state.Unfinished())

				for j := range 10 {
					el := state.Element(fmt.Sprintf("p/g/s%d", j))

					switch {
					case j < i:
						assert.Equal(t, pipeline.StatusPassed, el.Status)
					case j > i:
						assert.Equal(t, pipeline.StatusNotReached, el.Status)
					case point == pipelinetest.Finished:
						assert.Equal(t, pipeline.StatusPassed, el.Status)
					default:
						assert.Equal(t, pipeline.StatusStarted, el.Status, "interrupted")
						assert.Equal(t, point != pipelinetest.Started, el.RunCalled)
					}
				}

				_, recorded := state.Value(path, "k")
				assert.Equal(t, p >= 2, recorded, "the value is recorded once reached")
				assert.Equal(t, p >= 3, len(state.Items(path)) == 1, "the item is recorded once reached")
				assert.Equal(t, i+min(p, 1), runs, "Run is called only once past Running")
			})
		}
	}
}

func TestCrashAfter_PointNotReached(t *testing.T) {
	t.Parallel()

	runs := 0
	crash := pipelinetest.CrashAfter("p/g/elsewhere", pipelinetest.Running)

	res, err := pipeline.Run(t.Context(), tenSteps(t, &runs), crash.Journal)
	require.NoError(t, err)
	assert.Equal(t, pipeline.OutcomePassed, res.Outcome)
	assert.False(t, crash.Crashed())
}

func TestFakeClock_DrivesBackoffAndTimestamps(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	clock := pipelinetest.NewFakeClock(start)

	tries := 0
	plan, err := pipeline.NewPlan("p", pipeline.Effect("flaky", pipeline.StepFunc(
		func(context.Context, *pipeline.StepContext) error {
			tries++
			if tries == 1 {
				return errors.New("first try fails")
			}

			return nil
		})).Retry(2, time.Hour))
	require.NoError(t, err)

	j := pipeline.MemoryJournal()
	done := make(chan error, 1)

	go func() {
		_, err := pipeline.Run(t.Context(), plan, j, pipeline.WithClock(clock))
		done <- err
	}()

	// The step waits an hour of fake time before its second try.
	require.Eventually(t, func() bool { return clock.Waiters() == 1 }, 5*time.Second, time.Millisecond)
	clock.Advance(time.Hour)

	require.NoError(t, <-done)
	assert.Equal(t, 2, tries)

	events, err := j.Events()
	require.NoError(t, err)
	assert.Equal(t, start, events[0].TS)
	assert.Equal(t, start.Add(time.Hour), events[len(events)-1].TS)
}

func TestFakeClock_AfterZeroFiresAtOnce(t *testing.T) {
	t.Parallel()

	clock := pipelinetest.NewFakeClock(time.Unix(0, 0))

	select {
	case <-clock.After(0):
	default:
		t.Fatal("After(0) should fire at once")
	}

	ch := clock.After(time.Minute)
	clock.Advance(59 * time.Second)

	select {
	case <-ch:
		t.Fatal("fired early")
	default:
	}

	clock.Advance(time.Second)
	<-ch
	assert.Zero(t, clock.Waiters())
}

func TestTrace(t *testing.T) {
	t.Parallel()

	plan, err := pipeline.NewPlan("p", pipeline.Effect("a", pipeline.StepFunc(
		func(_ context.Context, s *pipeline.StepContext) error {
			s.Info("hello")

			return nil
		})))
	require.NoError(t, err)

	j := pipeline.MemoryJournal()
	_, err = pipeline.Run(t.Context(), plan, j)
	require.NoError(t, err)

	events, err := j.Events()
	require.NoError(t, err)

	assert.Equal(t, []string{
		"run.started p", "step.started p/a", "step.running p/a", "step.finished p/a passed", "run.finished p passed",
	}, pipelinetest.Trace(events))
	assert.Contains(t, pipelinetest.Trace(events, pipeline.EventMessage), "message p/a")
}

// applied is the system a plan changes: how many times each step's work
// was done.
type applied struct {
	mu   sync.Mutex
	done map[string]int
}

func (a *applied) count(name string) int {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.done[name]
}

// realWork does its work once, and has a Check that looks at the system.
type realWork struct {
	sys  *applied
	name string
}

func (w realWork) Check(context.Context, *pipeline.StepContext) (bool, error) {
	return w.sys.count(w.name) > 0, nil
}

func (w realWork) Run(_ context.Context, s *pipeline.StepContext) error {
	w.sys.mu.Lock()
	w.sys.done[w.name]++
	w.sys.mu.Unlock()

	if err := s.Record("k", "v"); err != nil {
		return err
	}

	return s.Item("n", pipeline.ItemPassed, "")
}

// Whatever point a run crashes at, resuming it finishes the plan with every
// step's work done exactly once: nothing repeated, nothing missed.
func TestCrashAfter_ResumeFromEveryPoint(t *testing.T) {
	t.Parallel()

	points := []pipelinetest.Point{
		pipelinetest.Started, pipelinetest.Running, pipelinetest.Value("k"), pipelinetest.Item("n"), pipelinetest.Finished,
	}

	for i := range 10 {
		for _, point := range points {
			path := fmt.Sprintf("p/g/s%d", i)

			t.Run(fmt.Sprintf("%s/%s", path, point), func(t *testing.T) {
				t.Parallel()

				sys := &applied{done: map[string]int{}}

				steps := make([]pipeline.Element, 0, 10)

				for n := range 10 {
					name := fmt.Sprintf("s%d", n)
					steps = append(steps, pipeline.Effect(name, realWork{sys: sys, name: name}))
				}

				plan, err := pipeline.NewPlan("p", pipeline.Group("g", steps...))
				require.NoError(t, err)

				crash := pipelinetest.CrashAfter(path, point)

				_, err = pipeline.Run(t.Context(), plan, crash.Journal)
				require.ErrorIs(t, err, pipelinetest.ErrCrashed)

				crash.Restart()

				res, err := pipeline.Run(t.Context(), plan, crash.Journal)
				require.NoError(t, err)
				assert.Equal(t, 2, res.Attempt)
				assert.Equal(t, pipeline.OutcomePassed, res.Outcome)

				for n := range 10 {
					assert.Equal(t, 1, sys.count(fmt.Sprintf("s%d", n)), "step s%d's work", n)
				}

				state, err := crash.Journal.State()
				require.NoError(t, err)
				assert.False(t, state.Unfinished())

				// The step that was mid-Run when the process died is recorded
				// as recovered: its Check found the work done.
				if point != pipelinetest.Started && point != pipelinetest.Finished {
					assert.Equal(t, pipeline.ReasonRecovered, state.Element(path).Reason)
				}
			})
		}
	}
}
