package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/ruffel/pipeline"
)

// The plan is a mock rolling upgrade. Its steps only sleep, print and leave
// marker files in a state directory, but each has the shape a real one would.
func newPlan(state, fail string) (*pipeline.Plan, error) {
	nodes := make([]pipeline.Element, 0, 4)

	for _, n := range []string{"node1", "node2", "node3", "node4"} {
		nodes = append(nodes, pipeline.Group(n,
			pipeline.Effect("packages", marked(state, n+"-packages", installPackages(n, n == fail))).
				Description("Install the new packages"),
			pipeline.Effect("restart", marked(state, n+"-restart", restart(n))),
		))
	}

	return pipeline.NewPlan("upgrade",
		pipeline.Group("prechecks",
			pipeline.Observe("health", pipeline.StepFunc(health)),
			pipeline.Ensure("disk-space", pipeline.StepFunc(diskSpace)),
		).Description("Read-only checks before anything changes"),
		pipeline.Effect("backup", marked(state, "backup", backup(state))).
			Description("Snapshot the configuration"),
		pipeline.Gate("approve", pipeline.GateOptions{
			Prompt:    "Upgrade the nodes?",
			Approvers: approvers(state),
		}),
		pipeline.Group("nodes", nodes...).Parallel(2).
			Description("Two nodes at a time"),
		pipeline.Effect("finalise", marked(state, "finalise", finalise)),
	)
}

// approvers releases the gate when a flag file appears in the state
// directory, or when someone answers yes at the terminal, if there is one.
// `detached approve` works too, from any terminal.
func approvers(state string) []pipeline.Approver {
	as := []pipeline.Approver{pipeline.FileApprover(filepath.Join(state, "approve"), time.Second)}

	if interactive() {
		as = append(as, pipeline.StdinApprover())
	}

	return as
}

// interactive reports whether someone at a terminal can answer the gate.
func interactive() bool {
	return term.IsTerminal(os.Stdin.Fd())
}

// markedStep is a step whose Check looks for the marker file its Run leaves
// when it succeeds. A resumed run checks before running, so work that was
// done, even by a run that crashed before recording it, is not done again.
type markedStep struct {
	file string
	run  func(ctx context.Context, sc *pipeline.StepContext) error
}

func marked(state, name string, run func(context.Context, *pipeline.StepContext) error) markedStep {
	return markedStep{file: filepath.Join(state, name+".done"), run: run}
}

func (m markedStep) Check(context.Context, *pipeline.StepContext) (bool, error) {
	_, err := os.Stat(m.file)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}

	return err == nil, err
}

func (m markedStep) Run(ctx context.Context, sc *pipeline.StepContext) error {
	if err := m.run(ctx, sc); err != nil {
		return err
	}

	return os.WriteFile(m.file, nil, 0o600)
}

func health(_ context.Context, sc *pipeline.StepContext) error {
	for _, n := range []string{"node1", "node2", "node3", "node4"} {
		if err := sc.Item(n, pipeline.ItemPassed, "healthy"); err != nil {
			return err
		}
	}

	return nil
}

func diskSpace(ctx context.Context, sc *pipeline.StepContext) error {
	sc.Progress("scan", "checking free space", 1, 2)

	if err := sleep(ctx, 500*time.Millisecond); err != nil {
		return err
	}

	sc.Progress("scan", "checking free space", 2, 2)

	return nil
}

func backup(state string) func(context.Context, *pipeline.StepContext) error {
	return func(ctx context.Context, sc *pipeline.StepContext) error {
		if err := sleep(ctx, time.Second); err != nil {
			return err
		}

		archive := filepath.Join(state, "config-backup.tar")
		if err := os.WriteFile(archive, []byte("mock backup\n"), 0o600); err != nil {
			return err
		}

		// A later step, or an operator reading the journal, can find it.
		return sc.Record("archive", archive)
	}
}

func installPackages(node string, fail bool) func(context.Context, *pipeline.StepContext) error {
	return func(ctx context.Context, sc *pipeline.StepContext) error {
		for _, pkg := range []string{"hyperstore-core", "hyperstore-gateway", "hyperstore-tools"} {
			_, _ = fmt.Fprintf(sc.Stdout(), "Unpacking %s (8.2) on %s ...\n", pkg, node)

			if err := sleep(ctx, 1500*time.Millisecond); err != nil {
				return err
			}
		}

		if fail {
			_, _ = fmt.Fprintf(sc.Stderr(), "E: dpkg was interrupted on %s\n", node)

			return errors.New("exit status 1")
		}

		return nil
	}
}

func restart(node string) func(context.Context, *pipeline.StepContext) error {
	return func(ctx context.Context, sc *pipeline.StepContext) error {
		sc.Info("restarting services on " + node)

		return sleep(ctx, time.Second)
	}
}

func finalise(ctx context.Context, sc *pipeline.StepContext) error {
	// The backup recorded where it put the archive. If this journal never
	// ran the backup (its marker came from another journal's run), there
	// is nothing to keep, and the upgrade has no backup: fail loudly.
	var archive string

	found, err := sc.Recorded("upgrade/backup", "archive", &archive)
	if err != nil {
		return err
	}

	if !found {
		return errors.New("no backup recorded in this journal; remove the state directory and run again")
	}

	sc.Info("keeping the backup at " + archive)

	return sleep(ctx, 500*time.Millisecond)
}

// sleep waits for d, or until ctx is done: a cancelled run cuts steps short.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
