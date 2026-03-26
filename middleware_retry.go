package pipeline

import (
	"context"
	"errors"
	"time"
)

// WithRetry wraps a [StepFn] to retry on failure. The step is called up to
// maxAttempts times with a fixed delay between attempts. Each failed attempt
// emits a warning via [EmitWarnf]. Sentinel skip errors ([ErrSkipStep],
// [ErrSkipStage], [ErrSkipPipeline]) are never retried.
//
//	pipeline.Step{
//	    Name: "deploy",
//	    Run:  pipeline.WithRetry(3, time.Second, deploy),
//	}
//
// If the context is cancelled between attempts, the retry loop stops and
// returns the context error. Panics if maxAttempts < 1.
func WithRetry(maxAttempts int, backoff time.Duration, fn StepFn) StepFn {
	if maxAttempts < 1 {
		panic("pipeline: WithRetry maxAttempts must be >= 1")
	}

	return func(ctx context.Context) error {
		var err error

		for attempt := range maxAttempts {
			if err = fn(ctx); err == nil {
				return nil
			}

			// Sentinel skip errors are intentional flow control — propagate immediately.
			if errors.Is(err, ErrSkipStep) || errors.Is(err, ErrSkipStage) || errors.Is(err, ErrSkipPipeline) {
				return err
			}

			// Don't wait after the last attempt.
			if attempt == maxAttempts-1 {
				break
			}

			EmitWarnf(ctx, "attempt %d/%d failed: %v, retrying", attempt+1, maxAttempts, err)

			timer := time.NewTimer(backoff)

			select {
			case <-ctx.Done():
				timer.Stop()

				return ctx.Err()
			case <-timer.C:
			}
		}

		return err
	}
}
