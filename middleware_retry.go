package pipeline

import (
	"context"
	"errors"
	"time"
)

// WithRetry wraps a [StepFn] to retry on failure. The step is called up to
// maxAttempts times with a fixed delay between attempts. Each failed attempt
// emits a warning via [EmitWarnf]. Sentinel skip errors ([ErrSkipStage],
// [ErrSkipPipeline]) are never retried.
//
//	pipeline.Step{
//	    Name: "deploy",
//	    Run:  pipeline.WithRetry(3, time.Second, deploy),
//	}
//
// If the context ends during or between attempts, the loop stops without
// announcing a retry and returns the final attempt's error, joined with the
// context error unless the attempt's error already reports it. Panics if
// maxAttempts < 1.
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
			if errors.Is(err, ErrSkipStage) || errors.Is(err, ErrSkipPipeline) {
				return err
			}

			// Don't wait after the last attempt.
			if attempt == maxAttempts-1 {
				break
			}

			// The context ended during the attempt: no retry will follow, so
			// none is announced. The attempt's error stays visible.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return joinContextErr(err, ctxErr)
			}

			EmitWarnf(ctx, "attempt %d/%d failed: %v, retrying", attempt+1, maxAttempts, err)

			timer := time.NewTimer(backoff)

			select {
			case <-ctx.Done():
				timer.Stop()

				// Keep the attempt's error visible alongside the cancellation.
				return joinContextErr(err, ctx.Err())
			case <-timer.C:
			}
		}

		return err
	}
}

// joinContextErr returns err joined with ctxErr, unless err already reports
// ctxErr, in which case err is returned as is so the cause is not repeated.
func joinContextErr(err, ctxErr error) error {
	if errors.Is(err, ctxErr) {
		return err
	}

	return errors.Join(err, ctxErr)
}
