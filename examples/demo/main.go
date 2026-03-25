// Command demo runs a simulated deployment pipeline...
//
// Usage:
//
//	go run . [-format terminal|json]
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/ruffel/pipeline"
	jsonobs "github.com/ruffel/pipeline/observers/json"
	termobs "github.com/ruffel/pipeline/observers/terminal"
)

func main() {
	format := flag.String("format", "terminal", "observer format: terminal or json")
	flag.Parse()

	ex := pipeline.NewExecutor(buildObserver(*format))

	p := pipeline.NewPipeline("deploy",
		preflight(),
		build(),
		test(),
		release(),
	)

	if err := ex.Run(context.Background(), p); err != nil {
		log.Fatalf("pipeline failed: %v", err)
	}
}

func buildObserver(format string) pipeline.Observer {
	switch format {
	case "json":
		return jsonobs.New(os.Stdout)
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

func test() pipeline.Stage {
	return pipeline.NewStage("test",
		pipeline.NewStep("unit-tests", func(ctx context.Context) error {
			w := pipeline.OutputWriter(ctx, pipeline.Stdout)

			lines := []string{
				"=== RUN   TestUserService",
				"--- PASS: TestUserService (0.3s)",
				"=== RUN   TestOrderService",
				"--- PASS: TestOrderService (0.2s)",
				"PASS",
			}

			// Simulate a subprocess writing line by line.
			for _, line := range lines {
				fmt.Fprintln(w, line)
				sleep()
			}

			return w.Close()
		}),
		pipeline.NewStep("lint", func(_ context.Context) error {
			return fmt.Errorf("already passed in CI: %w", pipeline.ErrSkipStep)
		}),
	)
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
