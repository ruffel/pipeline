package runbooktest_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ruffel/pipeline/runbook"
	"github.com/ruffel/pipeline/runbook/runbooktest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// work is a step with Check, Run that records a value and an item, and a
// counter of Run calls.
type work struct{ runs *int }

func (w work) Check(context.Context, *runbook.StepContext) (bool, error) { return false, nil }

func (w work) Run(_ context.Context, s *runbook.StepContext) error {
	*w.runs++

	if err := s.Record("k", "v"); err != nil {
		return err
	}

	return s.Item("n", runbook.ItemPassed, "")
}

func tenSteps(t *testing.T, runs *int) *runbook.Plan {
	t.Helper()

	steps := make([]runbook.Element, 0, 10)
	for i := range 10 {
		steps = append(steps, runbook.Effect(fmt.Sprintf("s%d", i), work{runs: runs}))
	}

	plan, err := runbook.NewPlan("p", runbook.Group("g", steps...))
	require.NoError(t, err)

	return plan
}

// Crashing at every point of every step of a 10-step plan leaves a journal
// that ends at the crash and replays to where the run stood.
func TestCrashAfter_EveryPointOfATenStepPlan(t *testing.T) {
	t.Parallel()

	points := []runbooktest.Point{
		runbooktest.Started, runbooktest.Running, runbooktest.Value("k"), runbooktest.Item("n"), runbooktest.Finished,
	}

	for i := range 10 {
		for p, point := range points {
			path := fmt.Sprintf("p/g/s%d", i)

			t.Run(fmt.Sprintf("%s/%s", path, point), func(t *testing.T) {
				t.Parallel()

				runs := 0
				crash := runbooktest.CrashAfter(path, point)

				_, err := runbook.Run(t.Context(), tenSteps(t, &runs), crash.Journal)
				require.ErrorIs(t, err, runbooktest.ErrCrashed)
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
						assert.Equal(t, runbook.StatusPassed, el.Status)
					case j > i:
						assert.Equal(t, runbook.StatusNotReached, el.Status)
					case point == runbooktest.Finished:
						assert.Equal(t, runbook.StatusPassed, el.Status)
					default:
						assert.Equal(t, runbook.StatusStarted, el.Status, "interrupted")
						assert.Equal(t, point != runbooktest.Started, el.RunCalled)
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
	crash := runbooktest.CrashAfter("p/g/elsewhere", runbooktest.Running)

	res, err := runbook.Run(t.Context(), tenSteps(t, &runs), crash.Journal)
	require.NoError(t, err)
	assert.Equal(t, runbook.OutcomePassed, res.Outcome)
	assert.False(t, crash.Crashed())
}

func TestFakeClock_DrivesBackoffAndTimestamps(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	clock := runbooktest.NewFakeClock(start)

	tries := 0
	plan, err := runbook.NewPlan("p", runbook.Effect("flaky", runbook.StepFunc(
		func(context.Context, *runbook.StepContext) error {
			tries++
			if tries == 1 {
				return errors.New("first try fails")
			}

			return nil
		})).Retry(2, time.Hour))
	require.NoError(t, err)

	j := runbook.MemoryJournal()
	done := make(chan error, 1)

	go func() {
		_, err := runbook.Run(t.Context(), plan, j, runbook.WithClock(clock))
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

	clock := runbooktest.NewFakeClock(time.Unix(0, 0))

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

	plan, err := runbook.NewPlan("p", runbook.Effect("a", runbook.StepFunc(
		func(_ context.Context, s *runbook.StepContext) error {
			s.Info("hello")

			return nil
		})))
	require.NoError(t, err)

	j := runbook.MemoryJournal()
	_, err = runbook.Run(t.Context(), plan, j)
	require.NoError(t, err)

	events, err := j.Events()
	require.NoError(t, err)

	assert.Equal(t, []string{
		"run.started p", "step.started p/a", "step.running p/a", "step.finished p/a passed", "run.finished p passed",
	}, runbooktest.Trace(events))
	assert.Contains(t, runbooktest.Trace(events, runbook.EventMessage), "message p/a")
}
