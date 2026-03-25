// Command demo runs a simulated deployment pipeline.
//
// Usage:
//
//	go run . [-format terminal|plain|json]
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/ruffel/pipeline"
	jsonobs "github.com/ruffel/pipeline/observers/json"
	plainobs "github.com/ruffel/pipeline/observers/plain"
	termobs "github.com/ruffel/pipeline/observers/terminal"
)

func main() {
	format := flag.String("format", "terminal", "observer format: terminal, plain or json")
	flag.Parse()

	ex := pipeline.NewExecutor(buildObserver(*format))

	p := pipeline.NewPipeline("deploy",
		preflight(),
		build(),
		dbMigrate(),
		deploy(),
		release(),
		test(),
	)

	if err := ex.Run(context.Background(), p); err != nil {
		fmt.Printf("\n(Note: The pipeline intentionally fails in the demo to showcase ContinueOnError)\n")
	}
}

func buildObserver(format string) pipeline.Observer {
	switch format {
	case "json":
		return jsonobs.New(os.Stdout)
	case "plain":
		return plainobs.New(os.Stdout)
	default:
		return termobs.New(os.Stdout)
	}
}

// -----------------------------------------------------------------------------
// Stages
// -----------------------------------------------------------------------------

func preflight() pipeline.Stage {
	return pipeline.NewStage("preflight",
		pipeline.NewStep("check-cluster", func(ctx context.Context) error {
			pipeline.EmitInfo(ctx, "connecting to cluster...")
			sleep()
			pipeline.EmitInfo(ctx, "cluster healthy ✓")

			return nil
		}),
		pipeline.NewStep("validate-config", func(ctx context.Context) error {
			pipeline.EmitInfo(ctx, "validating deployment config")
			sleep()

			return nil
		}),
	)
}

func build() pipeline.Stage {
	return pipeline.NewParallelStage("build",
		pipeline.NewStep("compile-api", compileStep("api")),
		pipeline.NewStep("compile-worker", compileStep("worker")),
	)
}

var flakyAttempts int

func dbMigrate() pipeline.Stage {
	return pipeline.NewStage("db-migrate",
		pipeline.NewStep("apply-schema", func(ctx context.Context) error {
			out := pipeline.OutputWriter(ctx, pipeline.Stdout)
			errOut := pipeline.OutputWriter(ctx, pipeline.Stderr)

			pipeline.EmitInfo(ctx, "starting database migrations...")
			sleep()

			fmt.Fprintln(out, "--> applying 001_initial_schema.sql")
			sleep()
			fmt.Fprintln(out, "--> applying 002_add_user_indexes.sql")
			sleep()
			fmt.Fprintln(errOut, "WARNING: index 'idx_email' already exists, skipping")
			sleep()
			fmt.Fprintln(out, "--> applying 003_drop_legacy_tables.sql")

			out.Close()
			errOut.Close()

			pipeline.EmitInfo(ctx, "database migrations complete")
			return nil
		}),
	)
}

func test() pipeline.Stage {
	return pipeline.NewParallelStage("test",
		pipeline.NewStep("unit-tests", func(ctx context.Context) error {
			w := pipeline.OutputWriter(ctx, pipeline.Stdout)

			lines := []string{
				"=== RUN   TestUserService",
				"--- PASS: TestUserService (0.3s)",
				"=== RUN   TestOrderService",
				"--- PASS: TestOrderService (0.2s)",
				"PASS",
			}

			for _, line := range lines {
				fmt.Fprintln(w, line)
				sleep()
			}

			return w.Close()
		}),
		pipeline.NewStep("lint", func(_ context.Context) error {
			return fmt.Errorf("already passed in CI: %w", pipeline.ErrSkipStep)
		}),
		pipeline.NewStep("integration", func(ctx context.Context) error {
			pipeline.EmitInfo(ctx, "starting slow integration test...")
			select {
			case <-time.After(5 * time.Second):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}).WithTimeout(500*time.Millisecond),
		pipeline.NewStep("flaky-test", func(ctx context.Context) error {
			flakyAttempts++
			if flakyAttempts < 3 {
				return fmt.Errorf("network flake")
			}
			pipeline.EmitInfo(ctx, "passed on attempt 3")
			return nil
		}).WithRetry(3, 200*time.Millisecond),
	).WithContinueOnError(true)
}

func release() pipeline.Stage {
	return pipeline.NewStage("release",
		pipeline.NewStep("publish", func(ctx context.Context) error {
			pipeline.EmitInfo(ctx, "pushing image to registry")
			sleep()
			pipeline.EmitProgress(ctx, "upload", "uploading image", 1, 3)
			sleep()
			pipeline.EmitProgress(ctx, "upload", "uploading image", 2, 3)
			sleep()
			pipeline.EmitProgress(ctx, "upload", "uploading image", 3, 3)
			pipeline.EmitCustom(ctx, "deploy.image-pushed", map[string]string{
				"image": "registry.example.com/app:v1.2.3",
			})

			return nil
		}),
	)
}

func deploy() pipeline.Stage {
	return pipeline.NewStage("deploy",
		pipeline.NewStep("push-image", func(ctx context.Context) error {
			total := 5

			for i := 1; i <= total; i++ {
				pipeline.EmitProgress(ctx, "push", "pushing image layers", i, total)
				sleep()
			}

			pipeline.EmitInfo(ctx, "image pushed to registry.example.com/app:v1.2.3")

			return nil
		}),
	)
}

// compileStep returns a step function that simulates compiling a service.
func compileStep(service string) pipeline.StepFn {
	return func(ctx context.Context) error {
		total := 4

		for i := 1; i <= total; i++ {
			pipeline.EmitProgress(ctx, "compile", fmt.Sprintf("compiling %s", service), i, total)
			sleep()
		}

		return nil
	}
}

func sleep() {
	time.Sleep(200 * time.Millisecond) //nolint:mnd
}
