// Command demo runs a simulated deployment: sequential and parallel groups,
// messages, progress, output, a custom event, a step skipped because its
// work is done, a retry, and a timeout that fails the run.
//
// Usage:
//
//	go run . [-format terminal|plain|json]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/ruffel/pipeline"
	"github.com/ruffel/pipeline/observers/terminal"
)

func main() {
	format := flag.String("format", "terminal", "how to show the run: terminal, plain or json")

	flag.Parse()

	view, err := observer(*format, os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "demo: %v\n", err)
		os.Exit(2)
	}

	plan, err := pipeline.NewPlan("deploy",
		preflight(),
		build(),
		migrate(),
		deploy(),
		release(),
		test(),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "demo: %v\n", err)
		os.Exit(1)
	}

	// The journal is in memory: this demo is about what a run shows, not
	// about resuming one. See examples/runbook and examples/detached.
	_, err = pipeline.Run(context.Background(), plan, pipeline.MemoryJournal(), pipeline.WithObservers(view))

	if _, ok := errors.AsType[*pipeline.StepError](err); ok {
		fmt.Println("\n(The integration test times out on purpose: its group continues on error, so the others finish.)")
	}
}

func observer(format string, w io.Writer) (pipeline.Observer, error) {
	switch format {
	case "terminal":
		return terminal.New(w), nil
	case "plain":
		return pipeline.NewPlainObserver(w), nil
	case "json":
		return pipeline.NewJSONObserver(w), nil
	default:
		return nil, fmt.Errorf("unknown format %q: use terminal, plain or json", format)
	}
}

func preflight() pipeline.GroupNode {
	return pipeline.Group("preflight",
		pipeline.Observe("check-cluster", pipeline.StepFunc(func(_ context.Context, sc *pipeline.StepContext) error {
			sc.Info("connecting to cluster...")
			sleep()
			sc.Info("cluster healthy ✓")

			return nil
		})),
		pipeline.Observe("validate-config", pipeline.StepFunc(func(_ context.Context, sc *pipeline.StepContext) error {
			sc.Info("validating deployment config")
			sleep()

			return nil
		})),
	).Description("Read-only checks before anything changes")
}

func build() pipeline.GroupNode {
	return pipeline.Group("build",
		pipeline.Effect("compile-api", compile("api")),
		pipeline.Effect("compile-worker", compile("worker")),
	).Parallel(2)
}

// compile simulates compiling a service.
func compile(service string) pipeline.StepFunc {
	return func(_ context.Context, sc *pipeline.StepContext) error {
		const total = 4

		for i := 1; i <= total; i++ {
			sc.Progress("compile", "compiling "+service, i, total)
			sleep()
		}

		return nil
	}
}

func migrate() pipeline.GroupNode {
	return pipeline.Group("db-migrate",
		pipeline.Effect("apply-schema", pipeline.StepFunc(func(_ context.Context, sc *pipeline.StepContext) error {
			out, errOut := sc.Stdout(), sc.Stderr()

			sc.Info("starting database migrations...")
			sleep()

			fmt.Fprintln(out, "--> applying 001_initial_schema.sql")
			sleep()
			fmt.Fprintln(out, "--> applying 002_add_user_indexes.sql")
			sleep()
			fmt.Fprintln(errOut, "WARNING: index 'idx_email' already exists, skipping")
			sleep()
			fmt.Fprintln(out, "--> applying 003_drop_legacy_tables.sql")

			sc.Info("database migrations complete")

			return nil
		})).Description("Apply the schema migrations"),
	)
}

func deploy() pipeline.GroupNode {
	return pipeline.Group("deploy",
		pipeline.Effect("push-image", pipeline.StepFunc(func(_ context.Context, sc *pipeline.StepContext) error {
			const total = 5

			for i := 1; i <= total; i++ {
				sc.Progress("push", "pushing image layers", i, total)
				sleep()
			}

			sc.Info("image pushed to registry.example.com/app:v1.2.3")

			return nil
		})),
	)
}

func release() pipeline.GroupNode {
	return pipeline.Group("release",
		pipeline.Effect("publish", pipeline.StepFunc(func(_ context.Context, sc *pipeline.StepContext) error {
			sc.Info("publishing the release")

			const total = 3

			for i := 1; i <= total; i++ {
				sleep()
				sc.Progress("upload", "uploading release notes", i, total)
			}

			return sc.Custom("deploy.released", map[string]string{"version": "v1.2.3"})
		})),
	)
}

func test() pipeline.GroupNode {
	flaky := 0

	return pipeline.Group("test",
		pipeline.Observe("unit-tests", pipeline.StepFunc(func(_ context.Context, sc *pipeline.StepContext) error {
			for _, line := range []string{
				"=== RUN   TestUserService",
				"--- PASS: TestUserService (0.3s)",
				"=== RUN   TestOrderService",
				"--- PASS: TestOrderService (0.2s)",
				"PASS",
			} {
				fmt.Fprintln(sc.Stdout(), line)
				sleep()
			}

			return nil
		})),
		// Its Check finds the work done, so its Run is never called.
		pipeline.Ensure("lint", passedInCI{}),
		pipeline.Observe("integration", pipeline.StepFunc(func(ctx context.Context, sc *pipeline.StepContext) error {
			sc.Info("starting slow integration test...")

			select {
			case <-time.After(5 * time.Second):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})).Timeout(500*time.Millisecond),
		pipeline.Observe("flaky-test", pipeline.StepFunc(func(_ context.Context, sc *pipeline.StepContext) error {
			flaky++
			if flaky < 3 {
				return errors.New("network flake")
			}

			sc.Info("passed on try 3")

			return nil
		})).Retry(3, 200*time.Millisecond),
	).Parallel(4).ContinueOnError()
}

// passedInCI is a step whose work is already done.
type passedInCI struct{}

func (passedInCI) Check(_ context.Context, sc *pipeline.StepContext) (bool, error) {
	sc.Info("already passed in CI")

	return true, nil
}

func (passedInCI) Run(context.Context, *pipeline.StepContext) error {
	return nil
}

func sleep() {
	time.Sleep(200 * time.Millisecond) //nolint:mnd // Slow enough to watch.
}
