package main

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ruffel/pipeline"
)

// errDeclined marks a gate the operator answered with anything but yes.
var errDeclined = errors.New("declined by operator")

// stdinLines is fed by the single goroutine that reads os.Stdin for the whole
// process. Sharing one reader means buffered input is never lost between
// gates, and a cancelled gate leaves no reader goroutine behind.
var (
	stdinOnce  sync.Once
	stdinLines chan string
)

// stdin returns the channel of trimmed lines read from os.Stdin, starting the
// shared reader on first use. The channel closes when stdin reaches EOF.
func stdin() <-chan string {
	stdinOnce.Do(func() {
		stdinLines = make(chan string)

		go func() {
			defer close(stdinLines)

			sc := bufio.NewScanner(os.Stdin)
			for sc.Scan() {
				stdinLines <- strings.TrimSpace(sc.Text())
			}
		}()
	})

	return stdinLines
}

// confirm returns a gate step that blocks until the operator answers y/N on
// stdin. Keep gates in their own sequential, single-step stage so no peer
// output interleaves with the prompt, and don't wrap the terminal observer in
// an AsyncObserver when using them (delayed output breaks prompt ordering).
func confirm(prompt string) pipeline.StepFn {
	return func(ctx context.Context) error {
		pipeline.EmitInfo(ctx, prompt+" [y/N]")

		select {
		case <-ctx.Done():
			return ctx.Err()
		case answer, ok := <-stdin():
			if !ok {
				// EOF: nobody is there to approve.
				return errDeclined
			}

			if strings.EqualFold(answer, "y") || strings.EqualFold(answer, "yes") {
				return nil
			}

			return errDeclined
		}
	}
}

// autoApproved skips a gate when -yes was passed.
func autoApproved(yes bool) pipeline.ConditionFn {
	return func(_ context.Context) string {
		if yes {
			return "auto-approved (-yes)"
		}

		return ""
	}
}

// tracked wraps a step with a marker file: a completed run leaves the marker,
// and re-runs skip the step while it exists. Delete the state directory to
// start fresh.
func tracked(dir, name string, fn pipeline.StepFn) pipeline.Step {
	marker := filepath.Join(dir, name)

	return pipeline.NewStep(name, func(ctx context.Context) error {
		if err := fn(ctx); err != nil {
			return err
		}

		return os.WriteFile(marker, nil, 0o644)
	}).WithCondition(func(_ context.Context) string {
		if _, err := os.Stat(marker); err == nil {
			return "already completed (marker file)"
		}

		return ""
	})
}

// simulate emits a message and idles for d, honouring cancellation.
func simulate(ctx context.Context, msg string, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
	}

	pipeline.EmitInfo(ctx, msg)

	return nil
}
