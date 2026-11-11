package runbook

import (
	"context"
	"errors"
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

func TestRun_JournalFailureAtTheStartReturnsAtOnce(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan("p", Effect("a", StepFunc(func(context.Context, *StepContext) error {
		return errors.New("must not run")
	})))
	require.NoError(t, err)

	_, err = Run(t.Context(), plan, &Journal{store: &failingStore{n: 0}})
	require.ErrorContains(t, err, "runbook: journal: write run.started event: disk full")
}
