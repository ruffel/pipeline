// Command runbook demonstrates an operator-facing runbook: descriptions on
// every entry, approval gates, resumable steps, and a JSON audit log.
//
// Usage:
//
//	go run . [-yes] [-format terminal|plain|json] [-audit runbook.jsonl] [-state .runbook-state]
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
	yes := flag.Bool("yes", false, "auto-approve all gates")
	format := flag.String("format", "terminal", "observer format: terminal, plain or json")
	audit := flag.String("audit", "runbook.jsonl", "JSON Lines audit log path (empty to disable)")
	state := flag.String("state", ".runbook-state", "marker directory for resumable steps")
	flag.Parse()

	if err := run(*yes, *format, *audit, *state); err != nil {
		fmt.Fprintf(os.Stderr, "runbook failed: %v\n", err)

		os.Exit(1)
	}
}

func run(yes bool, format, audit, stateDir string) error {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}

	observers := []pipeline.Observer{buildObserver(format)}

	// The audit log records every event of every run, tagged with the run ID.
	if audit != "" {
		f, err := os.OpenFile(audit, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()

		observers = append(observers, jsonobs.New(f))
	}

	ex := pipeline.NewExecutorWithOptions(
		pipeline.WithObservers(observers...),
		pipeline.WithRunID(fmt.Sprintf("failover-%d", time.Now().Unix())),
	)

	p := pipeline.NewPipeline("db-failover",
		preflight(),
		approval(yes),
		failover(stateDir),
		verify(),
		wrapUp(yes),
	)

	return ex.Run(context.Background(), p)
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
			return simulate(ctx, "primary and replica reachable", 300*time.Millisecond)
		}).WithDescription("Verifies both database nodes are reachable"),
		pipeline.NewStep("check-replica-lag", func(ctx context.Context) error {
			return simulate(ctx, "replication lag 0.4s — within threshold", 300*time.Millisecond)
		}).WithDescription("Replica must be <5s behind before cutting over"),
	).WithDescription("Non-destructive checks — safe to re-run")
}

// approval is a gate in its own sequential, single-step stage: nothing else
// writes to the terminal while the prompt waits.
func approval(yes bool) pipeline.Stage {
	return pipeline.NewStage("approval",
		pipeline.NewStep("confirm-failover", confirm("Proceed with failover? Traffic pauses for ~30s")).
			WithDescription("Operator sign-off before any destructive action").
			WithCondition(autoApproved(yes)),
	).WithDescription("The last safe moment to abort")
}

func failover(stateDir string) pipeline.Stage {
	return pipeline.NewStage("failover",
		tracked(stateDir, "demote-primary", func(ctx context.Context) error {
			return simulate(ctx, "primary set read-only", 400*time.Millisecond)
		}).WithDescription("Sets the current primary read-only"),
		tracked(stateDir, "promote-replica", func(ctx context.Context) error {
			return simulate(ctx, "replica promoted", 400*time.Millisecond)
		}).WithDescription("Promotes the replica to primary"),
		tracked(stateDir, "update-dns", func(ctx context.Context) error {
			return simulate(ctx, "db.internal points at the new primary", 400*time.Millisecond)
		}).WithDescription("Repoints db.internal at the new primary"),
	).WithDescription("Destructive steps — markers make re-runs resume, not repeat")
}

func verify() pipeline.Stage {
	return pipeline.NewParallelStage("verify",
		pipeline.NewStep("smoke-test", func(ctx context.Context) error {
			return simulate(ctx, "round-trip write OK", 500*time.Millisecond)
		}).WithDescription("Round-trip write against the new primary"),
		pipeline.NewStep("check-metrics", func(ctx context.Context) error {
			return simulate(ctx, "error rate 0.02% — nominal", 600*time.Millisecond)
		}).WithDescription("Error rate must stay under 0.1%"),
	).WithDescription("Post-failover health checks")
}

// wrapUp shows the "do-nothing" pattern: the tool waits while a human does
// work it can't (yet) automate.
func wrapUp(yes bool) pipeline.Stage {
	return pipeline.NewStage("wrap-up",
		pipeline.NewStep("confirm-incident-doc", confirm("Update the incident doc with the failover time, then confirm")).
			WithDescription("Manual step — the paper trail lives outside this tool").
			WithCondition(autoApproved(yes)),
	).WithDescription("Close out the runbook")
}
