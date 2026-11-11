package runbook

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failingStore accepts the first n lines, then fails every write.
type failingStore struct {
	memoryStore

	mu sync.Mutex
	n  int
}

func (f *failingStore) append(line []byte, durable bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.data) >= f.n {
		return errors.New("disk full")
	}

	return f.memoryStore.append(line, durable)
}

func TestRun_JournalFailureStopsTheRun(t *testing.T) {
	t.Parallel()

	stepCancelled := make(chan error, 1)

	plan, err := NewPlan("p",
		Effect("a", StepFunc(func(ctx context.Context, s *StepContext) error {
			// The 4th write fails: run.started, step.started, step.running,
			// then this message.
			s.Info("this write fails")
			<-ctx.Done()

			stepCancelled <- context.Cause(ctx)

			return ctx.Err()
		})),
		Effect("b", StepFunc(func(context.Context, *StepContext) error {
			return errors.New("b must not run")
		})),
	)
	require.NoError(t, err)

	j := &Journal{store: &failingStore{n: 3}}

	var seen []EventKind

	res, err := Run(t.Context(), plan, j, WithObservers(ObserverFunc(func(_ context.Context, e Event) {
		seen = append(seen, e.Kind)
	})))

	require.ErrorContains(t, err, "runbook: journal: write message event: disk full")
	assert.Equal(t, OutcomeFailed, res.Outcome)
	require.ErrorContains(t, <-stepCancelled, "disk full", "the running step is cancelled with the cause")
	assert.Equal(t, []EventKind{EventRunStarted, EventStepStarted, EventStepRunning}, seen,
		"observers only ever see what the journal stored")

	events, err := j.Events()
	require.NoError(t, err)
	assert.Len(t, events, 3)
}

// A failure to write the warning about a panicking observer is a journal
// failure like any other: the run must not report success, and must not start
// a step's Run.
func TestRun_FailedObserverWarningStopsTheRun(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		panicOn EventKind
		stored  int
	}{
		"on run.finished":  {panicOn: EventRunFinished, stored: 5},
		"on step.running":  {panicOn: EventStepRunning, stored: 3},
		"on step.finished": {panicOn: EventStepFinished, stored: 4},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ran := false

			plan, err := NewPlan("p", Effect("a", StepFunc(func(context.Context, *StepContext) error {
				ran = true

				return nil
			})))
			require.NoError(t, err)

			bad := ObserverFunc(func(_ context.Context, e Event) {
				if e.Kind == tc.panicOn {
					panic("renderer bug")
				}
			})

			res, err := Run(t.Context(), plan, &Journal{store: &failingStore{n: tc.stored}}, WithObservers(bad))

			require.ErrorContains(t, err, "runbook: journal: write message event: disk full")
			assert.Equal(t, OutcomeFailed, res.Outcome)

			if tc.panicOn == EventStepRunning {
				assert.False(t, ran, "Run must not start after the journal failed")
			}
		})
	}
}

func TestRun_JournalFailureAtTheStartReturnsAtOnce(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan("p", Effect("a", StepFunc(func(context.Context, *StepContext) error {
		return errors.New("must not run")
	})))
	require.NoError(t, err)

	_, err = Run(t.Context(), plan, &Journal{store: &failingStore{n: 0}})
	require.ErrorContains(t, err, "runbook: journal: write run.started event: disk full")
}

func TestRun_OutputPastTheBudgetIsDroppedAndCounted(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan("p", Effect("a", StepFunc(func(_ context.Context, s *StepContext) error {
		w := s.Stdout()

		// Ten 9-byte lines fit a 90-byte budget; three more are dropped.
		for i := range 13 {
			_, _ = fmt.Fprintf(w, "line %04d\n", i)
		}

		_, _ = io.WriteString(s.Stderr(), "the budget covers both streams\n")

		return nil
	})))
	require.NoError(t, err)

	j := MemoryJournal()
	small := func(c *config) { c.outputBudget = 90 }

	_, err = Run(t.Context(), plan, j, small)
	require.NoError(t, err)

	events, err := j.Events()
	require.NoError(t, err)

	var lines, warnings []string

	for _, e := range events {
		switch {
		case e.Kind == EventOutput:
			lines = append(lines, e.Line)
		case e.Kind == EventMessage && e.Level == LevelWarn:
			warnings = append(warnings, e.Message)
		}
	}

	assert.Len(t, lines, 10)
	assert.Equal(t, "line 0009", lines[9])
	assert.Equal(t, []string{
		"output past 90 bytes is dropped",
		"dropped 4 output lines past the step's 90-byte budget",
	}, warnings)
	assert.NotContains(t, strings.Join(lines, ""), "both streams")
}
