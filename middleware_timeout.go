package pipeline

import (
	"context"
	"time"
)

// WithTimeout wraps a [StepFn] with a per-step deadline. If the step does not
// complete within d, its context is cancelled with [context.DeadlineExceeded].
//
// Enforcement is cooperative: the wrapper cannot interrupt a step that
// ignores its context, so such a step will overrun the deadline. Steps must
// honour ctx cancellation (as any long-running work should) for the timeout
// to take effect.
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
