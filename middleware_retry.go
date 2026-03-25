package pipeline

import (
	"context"
	"time"
)

// WithRetry wraps a [StepFn] to retry on failure. The step is called up to
// max times with a fixed delay between attempts. Each failed attempt emits a
// warning via [EmitWarnf].
//
//	pipeline.Step{
//	    Name: "deploy",
//	    Run:  pipeline.WithRetry(3, time.Second, deploy),
//	}
//
// If the context is cancelled between attempts, the retry loop stops and
// returns the context error.
func WithRetry(maxAttempts int, backoff time.Duration, fn StepFn) StepFn {
	return func(ctx context.Context) error {
		var err error

		for attempt := range maxAttempts {
			if err = fn(ctx); err == nil {
				return nil
			}

			// Don't wait after the last attempt.
			if attempt == maxAttempts-1 {
				break
			}

			EmitWarnf(ctx, "attempt %d/%d failed: %v, retrying", attempt+1, maxAttempts, err)

			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}

		return err
	}
}
