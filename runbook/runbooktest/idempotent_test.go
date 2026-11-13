package runbooktest_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"

	"github.com/ruffel/pipeline/runbook"
	"github.com/ruffel/pipeline/runbook/runbooktest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// node is a fake system for the steps below.
type node struct {
	installed bool
	created   bool
}

// install is idempotent: its Check finds the packages installed, and it
// records before and after installing them.
type install struct{ n *node }

func (s install) Check(context.Context, *runbook.StepContext) (bool, error) {
	return s.n.installed, nil
}

func (s install) Run(_ context.Context, sc *runbook.StepContext) error {
	if err := sc.Record("started", true); err != nil {
		return err
	}

	s.n.installed = true

	if err := sc.Item("node1", runbook.ItemPassed, "installed"); err != nil {
		return err
	}

	return sc.Record("version", "8.2")
}

func TestCheckIdempotent_AStepThatSurvivesEveryCrash(t *testing.T) {
	t.Parallel()

	runs := 0

	runbooktest.CheckIdempotent(t, func(t *testing.T) runbook.StepNode {
		t.Helper()

		runs++

		n := &node{}

		t.Cleanup(func() { assert.True(t, n.installed, "the packages are installed") })

		return runbook.Effect("packages", install{n})
	}, runbooktest.Under("upgrade/node1"))

	// A clean run, then one for each point: running, started, node1, version.
	assert.Equal(t, 5, runs)
}

func TestCheckIdempotent_CrashesOncePerKey(t *testing.T) {
	t.Parallel()

	runs := 0

	runbooktest.CheckIdempotent(t, func(*testing.T) runbook.StepNode {
		runs++

		return runbook.Effect("count", runbook.StepFunc(func(_ context.Context, sc *runbook.StepContext) error {
			for i := range 3 {
				if err := sc.Record("progress", i); err != nil {
					return err
				}
			}

			return nil
		}))
	})

	// A clean run, then one for each point: running, progress.
	assert.Equal(t, 3, runs)
}

// helperFails runs the helper test in mode in a process of its own, and
// returns its output, which must show it failed.
func helperFails(t *testing.T, mode string) string {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestCheckIdempotent_HelperProcess$", "-test.v") //nolint:gosec // This test's own binary.

	cmd.Env = append(os.Environ(), "RUNBOOKTEST_IDEMPOTENT_HELPER="+mode)

	var out bytes.Buffer

	cmd.Stdout = &out
	cmd.Stderr = &out

	var exit *exec.ExitError

	require.ErrorAs(t, cmd.Run(), &exit, "the check passed:\n%s", out.String())

	return out.String()
}

func TestCheckIdempotent_FailsAStepThatCantRunTwice(t *testing.T) {
	t.Parallel()

	out := helperFails(t, "mkdir")

	assert.Contains(t, out, "test/mkdir does not survive a crash after step.running")
	assert.Contains(t, out, "test/mkdir does not survive a crash after value:dir")
	assert.Contains(t, out, "mkdir: file exists")
}

func TestCheckIdempotent_FailsAStepWithNothingToCrash(t *testing.T) {
	t.Parallel()

	assert.Contains(t, helperFails(t, "done"), "its Check found the work done")
}

func TestCheckIdempotent_FailsAStepThatChangesBetweenRuns(t *testing.T) {
	t.Parallel()

	assert.Contains(t, helperFails(t, "once"), "test/once did not reach value:first")
}

func TestCheckIdempotent_FailsAStepThatFailsWithoutACrash(t *testing.T) {
	t.Parallel()

	assert.Contains(t, helperFails(t, "broken"), "test/broken fails without a crash: ")
}

// TestCheckIdempotent_HelperProcess is the helper process the tests above
// run: each mode is a step the check must fail.
func TestCheckIdempotent_HelperProcess(t *testing.T) {
	t.Parallel()

	runs := 0

	steps := map[string]func(t *testing.T) runbook.StepNode{
		// mkdir has no Check, and fails if its directory exists.
		"mkdir": func(*testing.T) runbook.StepNode {
			n := &node{}

			return runbook.Effect("mkdir", runbook.StepFunc(func(_ context.Context, sc *runbook.StepContext) error {
				if n.created {
					return errors.New("mkdir: file exists")
				}

				n.created = true

				return sc.Record("dir", "/srv/data")
			}))
		},
		// done finds its work done before it starts.
		"done": func(*testing.T) runbook.StepNode {
			return runbook.Effect("done", install{&node{installed: true}})
		},
		// once records a value only the first time it is built.
		"once": func(*testing.T) runbook.StepNode {
			runs++
			first := runs == 1

			return runbook.Effect("once", runbook.StepFunc(func(_ context.Context, sc *runbook.StepContext) error {
				if first {
					return sc.Record("first", true)
				}

				return nil
			}))
		},
		"broken": func(*testing.T) runbook.StepNode {
			return runbook.Effect("broken", runbook.StepFunc(func(context.Context, *runbook.StepContext) error {
				return errors.New("exit status 1")
			}))
		},
	}

	mode := os.Getenv("RUNBOOKTEST_IDEMPOTENT_HELPER")
	if mode == "" {
		t.Skip("run by other tests")
	}

	runbooktest.CheckIdempotent(t, steps[mode])
}
