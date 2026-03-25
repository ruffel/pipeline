# pipeline

An Go library for executing multi-stage task pipelines with custom observers.

The pipeline handles execution, timeouts, and retries, only the tasks need defining.

## Quick Start

```go
package main

import (
	"context"
	"os"

	"github.com/ruffel/pipeline"
	"github.com/ruffel/pipeline/observers/terminal"
)

func main() {
	p := pipeline.NewPipeline("deploy",
		pipeline.NewStage("build",
			pipeline.NewStep("compile", func(ctx context.Context) error {
				pipeline.EmitInfo(ctx, "compiling binary...")
				return nil
			}),
		),
	)

	ex := pipeline.NewExecutor(terminal.New(os.Stdout))
	if err := ex.Run(context.Background(), p); err != nil {
		os.Exit(1)
	}
}
```

## Install

```bash
go get github.com/ruffel/pipeline
```

See the [demo](./examples/demo/main.go) for a larger example showing:
- Parallel execution with error boundaries (`ContinueOnError`)
- Flow control (`ErrSkipStep`)
- Subprocess output streaming (`OutputWriter`)
- Middlewares (`WithTimeout`, `WithRetry`)
- Multiple observers (Styled Terminal, Plain, JSON)
