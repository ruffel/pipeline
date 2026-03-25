package pipeline

import (
	"context"
	"time"
)

// WithTimeout wraps a [StepFn] with a per-step deadline. If the step does not
// complete within d, the context is cancelled and the step receives
// [context.DeadlineExceeded].
//
//	pipeline.Step{
//	    Name: "deploy",
//	    Run:  pipeline.WithTimeout(30*time.Second, deploy),
//	}
func WithTimeout(d time.Duration, fn StepFn) StepFn {
	return func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, d)
		defer cancel()

		return fn(ctx)
	}
}
