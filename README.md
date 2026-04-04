# pipeline

Observable task pipelines for Go.

Define stages and steps. The executor handles sequencing, parallelism,
timeouts, and retries. Observers receive structured events as progress.

![demo](https://vhs.charm.sh/vhs-6b0W5VnjsJ8GpqRxTUpyt9.gif)

## Install

```bash
go get github.com/ruffel/pipeline
```

## Quick Start

```go
p := pipeline.NewPipeline("deploy",
    pipeline.NewStage("build",
        pipeline.NewStep("compile", func(ctx context.Context) error {
            pipeline.EmitInfo(ctx, "compiling...")
            return nil
        }),
    ),
    pipeline.NewParallelStage("test",
        pipeline.NewStep("unit", runUnitTests),
        pipeline.NewStep("lint", runLinter),
    ),
)

ex := pipeline.NewExecutor(terminal.New(os.Stdout))
ex.Run(context.Background(), p)
```

## Features

- **Sequential & parallel stages** — `NewStage` runs steps in order, `NewParallelStage` runs concurrently
- **Conditions** — skip stages or steps based on runtime checks
- **Retry & timeout** — `WithRetry(3, time.Second, fn)`, `WithTimeout(30*time.Second, fn)`
- **Flow control** — `ErrSkipStage` and `ErrSkipPipeline` for early exit without failure
- **Progress & output** — `EmitProgress` for progress bars, `OutputWriter` for subprocess streaming
- **Multiple observers** — styled terminal, plain text, JSON lines, etc.
- **Async observers** — `AsyncObserver` wraps slow observers so they don't block execution

## Struct-Based Steps

For context and state management between steps, define steps as structs with explicit dependencies:

```go
type PushImage struct {
    Image string
    Tag   string
    State *State // shared mutable state between steps
}

func (p *PushImage) Run(ctx context.Context) error {
    ref := fmt.Sprintf("%s:%s", p.Image, p.Tag)

    for i := 1; i <= 4; i++ {
        pipeline.EmitProgress(ctx, "push", fmt.Sprintf("pushing %s", ref), i, 4)
    }

    p.State.ImageRef = ref

    return nil
}
```

Wire steps into stages using bound method values:

```go
func build(cfg Config, state *State) pipeline.Stage {
    return pipeline.NewParallelStage("build",
        pipeline.NewStep("compile-api", (&Compile{Service: "api"}).Run),
        pipeline.NewStep("compile-worker", (&Compile{Service: "worker"}).Run),
        pipeline.NewStep("push-image", (&PushImage{
            Image: cfg.Image,
            Tag:   cfg.Tag,
            State: state,
        }).Run),
    )
}
```

This keeps `main()` as a readable table of contents:

```go
p := pipeline.NewPipeline("deploy",
    preflight(cfg),
    build(cfg, state),
    deploy(cfg, state),
    verify(cfg),
    notify(cfg, state),
)
```

See [`examples/deploy`](./examples/deploy) for the full implementation.

## Observers

| Observer | Package                 | Output                                |
| -------- | ----------------------- | ------------------------------------- |
| Terminal | `observers/terminal`    | Styled boxes, colors, aligned columns |
| Plain    | `observers/plain`       | Simple text lines                     |
| JSON     | `observers/json`        | JSON Lines — one object per event     |
| Custom   | `pipeline.ObserverFunc` | Any function                          |

Observers are optional submodules — import only what you use:

```go
import termobs "github.com/ruffel/pipeline/observers/terminal"

ex := pipeline.NewExecutor(
    termobs.New(os.Stdout),
    jsonobs.New(logFile),
)
```

## Flow Control

Stages and steps can be conditionally skipped before execution:

```go
pipeline.NewStage("notify", steps...).WithCondition(func(_ context.Context) string {
    if dryRun {
        return "dry-run mode" // non-empty reason = skip
    }
    return "" // empty = run
})
```

## Examples

- **[demo](./examples/demo)** — inline steps covering all pipeline features
- **[deploy](./examples/deploy)** — struct-based steps with state management and production patterns
