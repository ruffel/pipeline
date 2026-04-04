// Command deploy demonstrates a struct-based pipeline with inter-step state.
//
// Usage:
//
//	go run . [-format terminal|plain|json] [-dry-run]
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/ruffel/pipeline"
	jsonobs "github.com/ruffel/pipeline/observers/json"
	plainobs "github.com/ruffel/pipeline/observers/plain"
	termobs "github.com/ruffel/pipeline/observers/terminal"
)

// Config holds deployment parameters, passed by value to stages.
type Config struct {
	Region    string
	Image     string
	Tag       string
	Replicas  int
	Namespace string
	DryRun    bool
}

// State carries data produced by earlier steps (e.g. the resolved image ref)
// and consumed by later ones. Passed by pointer so stages share the same
// instance.
type State struct {
	ImageRef string
}

func main() {
	dryRun := flag.Bool("dry-run", false, "skip notification stage")
	format := flag.String("format", "terminal", "observer format: terminal, plain or json")
	flag.Parse()

	cfg := Config{
		Region:    "us-east-1",
		Image:     "registry.example.com/app",
		Tag:       "v1.2.3",
		Replicas:  3,
		Namespace: "production",
		DryRun:    *dryRun,
	}

	state := &State{}

	p := pipeline.NewPipeline("deploy",
		preflight(cfg),
		build(cfg, state),
		deploy(cfg, state),
		verify(cfg),
		notify(cfg, state),
	)

	ex := pipeline.NewExecutor(buildObserver(*format))

	if err := ex.Run(context.Background(), p); err != nil {
		fmt.Fprintf(os.Stderr, "deploy failed: %v\n", err)

		os.Exit(1)
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
