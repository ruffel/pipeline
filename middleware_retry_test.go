package pipeline_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ruffel/pipeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWithRetry_SucceedsFirstAttempt(t *testing.T) {
	t.Parallel()

	step := pipeline.WithRetry(3, time.Millisecond, func(_ context.Context) error {
		return nil
	})

	require.NoError(t, step(t.Context()))
}

func TestWithRetry_SucceedsAfterTransientFailure(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32

	step := pipeline.WithRetry(3, time.Millisecond, func(_ context.Context) error {
		if attempts.Add(1) < 3 {
			return errors.New("transient")
		}

		return nil
	})

	require.NoError(t, step(t.Context()))
	assert.Equal(t, int32(3), attempts.Load())
}

func TestWithRetry_ExhaustsMaxAttempts(t *testing.T) {
	t.Parallel()

	permanent := errors.New("permanent")

	step := pipeline.WithRetry(3, time.Millisecond, func(_ context.Context) error {
		return permanent
	})

	err := step(t.Context())

	assert.ErrorIs(t, err, permanent)
}

func TestWithRetry_RespectsContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())

	var attempts atomic.Int32

	step := pipeline.WithRetry(10, time.Second, func(_ context.Context) error {
		if attempts.Add(1) == 1 {
			cancel()
		}

		return errors.New("fail")
	})

	err := step(ctx)

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, int32(1), attempts.Load())
}

func TestWithRetry_EmitsWarnings(t *testing.T) {
	t.Parallel()

	obs := &recordingObserver{}
	em := pipeline.NewEmitter(obs.OnEvent, pipeline.Location{
		Pipeline: "p",
		Stage:    "s",
		Step:     "step",
	})

	ctx := pipeline.WithEmitter(t.Context(), em)

	var attempts atomic.Int32

	step := pipeline.WithRetry(3, time.Millisecond, func(_ context.Context) error {
		if attempts.Add(1) < 3 {
			return errors.New("transient")
		}

		return nil
	})

	require.NoError(t, step(ctx))

	// Should have 2 warning messages (attempt 1 and 2 failed).
	var warnings int

	for _, e := range obs.events {
		if _, ok := e.(pipeline.MessageEvent); ok {
			warnings++
		}
	}

	assert.Equal(t, 2, warnings)
}

func TestWithRetry_ComposesWithTimeout(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32

	inner := func(_ context.Context) error {
		if attempts.Add(1) < 2 {
			return errors.New("transient")
		}

		return nil
	}

	step := pipeline.WithTimeout(time.Second,
		pipeline.WithRetry(3, time.Millisecond, inner))

	require.NoError(t, step(t.Context()))
	assert.Equal(t, int32(2), attempts.Load())
}

func TestWithRetry_DoesNotRetrySentinelErrors(t *testing.T) {
	t.Parallel()

	sentinels := []struct {
		name string
		err  error
	}{
		{"ErrSkipStage", pipeline.ErrSkipStage},
		{"ErrSkipPipeline", pipeline.ErrSkipPipeline},
	}

	for _, tt := range sentinels {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var attempts atomic.Int32

			step := pipeline.WithRetry(3, time.Millisecond, func(_ context.Context) error {
				attempts.Add(1)

				return tt.err
			})

			err := step(t.Context())

			require.ErrorIs(t, err, tt.err)
			assert.Equal(t, int32(1), attempts.Load(), "sentinel errors should not be retried")
		})
	}
}

func TestWithRetry_PanicsOnInvalidMaxAttempts(t *testing.T) {
	t.Parallel()

	for _, n := range []int{0, -1, -100} {
		assert.PanicsWithValue(t, "pipeline: WithRetry maxAttempts must be >= 1", func() {
			pipeline.WithRetry(n, time.Millisecond, noop)
		})
	}
}
