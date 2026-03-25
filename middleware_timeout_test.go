package pipeline_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ruffel/pipeline"
	"github.com/stretchr/testify/assert"
)

func TestWithTimeout_FastStep(t *testing.T) {
	t.Parallel()

	step := pipeline.WithTimeout(time.Second, func(_ context.Context) error {
		return nil
	})

	assert.NoError(t, step(t.Context()))
}

func TestWithTimeout_SlowStep(t *testing.T) {
	t.Parallel()

	step := pipeline.WithTimeout(50*time.Millisecond, func(ctx context.Context) error {
		<-ctx.Done()

		return ctx.Err()
	})

	err := step(t.Context())

	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestWithTimeout_PreservesStepError(t *testing.T) {
	t.Parallel()

	stepErr := errors.New("deploy failed")
	step := pipeline.WithTimeout(time.Second, func(_ context.Context) error {
		return stepErr
	})

	err := step(t.Context())

	assert.ErrorIs(t, err, stepErr)
}

func TestWithTimeout_PropagatesParentCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	step := pipeline.WithTimeout(time.Second, func(ctx context.Context) error {
		return ctx.Err()
	})

	err := step(ctx)

	assert.ErrorIs(t, err, context.Canceled)
}
