package pipelinetest_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"

	"github.com/ruffel/pipeline"
	"github.com/ruffel/pipeline/pipelinetest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// node is a fake system for the steps below.
type node struct {
	installed bool
	created   bool
}

func newNode(*testing.T) *node { return &node{} }

// install is idempotent: its Check finds the packages installed, and it
// records before and after installing them.
type install struct{ n *node }

func (s install) Check(context.Context, *pipeline.StepContext) (bool, error) {
	return s.n.installed, nil
}

func (s install) Run(_ context.Context, sc *pipeline.StepContext) error {
	if err := sc.Record("started", true); err != nil {
		return err
	}

	sc.Info("installing")

	s.n.installed = true

	if err := sc.Item("node1", pipeline.ItemPassed, "installed"); err != nil {
		return err
	}

	return sc.Record("version", "8.2")
}

func TestCheckIdempotent_AStepThatSurvivesEveryCrash(t *testing.T) {
	t.Parallel()

	systems, steps := 0, 0

	pipelinetest.CheckIdempotent(t, func(t *testing.T) *node {
		t.Helper()

		systems++

		n := &node{}

		t.Cleanup(func() { assert.True(t, n.installed, "the packages are installed") })

		return n
	}, func(n *node) pipeline.StepNode {
		steps++

		return pipeline.Effect("packages", install{n})
	}, pipelinetest.Under("upgrade/node1"))

	// A clean run, then a crash after each event: started, running, the
	// started value, the message, node1, version. Each crash builds the step
	// twice: for the run that crashes, and for the resume.
	assert.Equal(t, 7, systems)
	assert.Equal(t, 13, steps)
}

func TestCheckIdempotent_CrashesAfterEveryOccurrence(t *testing.T) {
	t.Parallel()

	systems := 0

	pipelinetest.CheckIdempotent(t, func(*testing.T) *node {
		systems++

		return &node{}
	}, func(*node) pipeline.StepNode {
		return pipeline.Effect("count", pipeline.StepFunc(func(_ context.Context, sc *pipeline.StepContext) error {
			for i := range 3 {
				if err := sc.Record("progress", i); err != nil {
					return err
				}
			}

			return nil
		}))
	})

	// A clean run, then a crash after each event: started, running, and
	// each of the three values.
	assert.Equal(t, 6, systems)
}

// helperFails runs the helper test in mode in a process of its own, and
// returns its output, which must show it failed.
func helperFails(t *testing.T, mode string) string {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestCheckIdempotent_HelperProcess$", "-test.v") //nolint:gosec // This test's own binary.

	cmd.Env = append(os.Environ(), "PIPELINETEST_IDEMPOTENT_HELPER="+mode)

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

	assert.Contains(t, out, "test/mkdir does not survive a crash after its event 2 (step.running)")
	assert.Contains(t, out, "test/mkdir does not survive a crash after its event 3 (value:dir)")
	assert.Contains(t, out, "mkdir: file exists")
}

func TestCheckIdempotent_FailsAStepWhoseCheckReadsItsOwnMemory(t *testing.T) {
	t.Parallel()

	assert.Contains(t, helperFails(t, "memory"), "test/memory does not survive a crash after its event 3 (value:k)")
}

func TestCheckIdempotent_CrashesAfterChatter(t *testing.T) {
	t.Parallel()

	// The message is the step's last event: a crash after it lets the step
	// make its change, and then its outcome is lost.
	assert.Contains(t, helperFails(t, "chatty"), "test/chatty does not survive a crash after its event 3 (message)")
}

func TestCheckIdempotent_CrashesAfterALaterOccurrence(t *testing.T) {
	t.Parallel()

	out := helperFails(t, "later")

	assert.Contains(t, out, "test/later does not survive a crash after its event 4 (value:p)")
	assert.NotContains(t, out, "crash after its event 3 (value:p)")
}

func TestCheckIdempotent_FailsAStepWithNothingToCrash(t *testing.T) {
	t.Parallel()

	assert.Contains(t, helperFails(t, "done"), "its Check found the work done")
}

func TestCheckIdempotent_FailsAStepThatChangesBetweenRuns(t *testing.T) {
	t.Parallel()

	assert.Contains(t, helperFails(t, "once"), "test/once did not write value:first as its event 3 this time")
}

func TestCheckIdempotent_FailsAStepThatFailsWithoutACrash(t *testing.T) {
	t.Parallel()

	assert.Contains(t, helperFails(t, "broken"), "test/broken fails without a crash: ")
}

// memory's Check reads a flag its own Run set, not the system: a restarted
// process has lost it.
type memory struct {
	n    *node
	done bool
}

func (m *memory) Check(context.Context, *pipeline.StepContext) (bool, error) { return m.done, nil }

func (m *memory) Run(_ context.Context, sc *pipeline.StepContext) error {
	if m.n.created {
		return errors.New("mkdir: file exists")
	}

	m.n.created = true
	m.done = true

	return sc.Record("k", 1)
}

// TestCheckIdempotent_HelperProcess is the helper process the tests above
// run: each mode is a step the check must fail.
func TestCheckIdempotent_HelperProcess(t *testing.T) {
	t.Parallel()

	builds := 0

	steps := map[string]func(n *node) pipeline.StepNode{
		// mkdir has no Check, and fails if its directory exists.
		"mkdir": func(n *node) pipeline.StepNode {
			return pipeline.Effect("mkdir", pipeline.StepFunc(func(_ context.Context, sc *pipeline.StepContext) error {
				if n.created {
					return errors.New("mkdir: file exists")
				}

				n.created = true

				return sc.Record("dir", "/srv/data")
			}))
		},
		"memory": func(n *node) pipeline.StepNode {
			return pipeline.Effect("memory", &memory{n: n})
		},
		// chatty says what it will do, gives up if cancelled, and then makes
		// its change.
		"chatty": func(n *node) pipeline.StepNode {
			return pipeline.Effect("chatty", pipeline.StepFunc(func(ctx context.Context, sc *pipeline.StepContext) error {
				sc.Info("creating /srv/data")

				if err := ctx.Err(); err != nil {
					return err
				}

				if n.created {
					return errors.New("mkdir: file exists")
				}

				n.created = true

				return nil
			}))
		},
		// later makes its change between its second and third values.
		"later": func(n *node) pipeline.StepNode {
			return pipeline.Effect("later", pipeline.StepFunc(func(_ context.Context, sc *pipeline.StepContext) error {
				for i := range 3 {
					if i == 2 {
						if n.created {
							return errors.New("mkdir: file exists")
						}

						n.created = true
					}

					if err := sc.Record("p", i); err != nil {
						return err
					}
				}

				return nil
			}))
		},
		// done finds its work done before it starts.
		"done": func(n *node) pipeline.StepNode {
			n.installed = true

			return pipeline.Effect("done", install{n})
		},
		// once records a value only the first time it is built.
		"once": func(*node) pipeline.StepNode {
			builds++
			first := builds == 1

			return pipeline.Effect("once", pipeline.StepFunc(func(_ context.Context, sc *pipeline.StepContext) error {
				if first {
					return sc.Record("first", true)
				}

				return nil
			}))
		},
		"broken": func(*node) pipeline.StepNode {
			return pipeline.Effect("broken", pipeline.StepFunc(func(context.Context, *pipeline.StepContext) error {
				return errors.New("exit status 1")
			}))
		},
	}

	mode := os.Getenv("PIPELINETEST_IDEMPOTENT_HELPER")
	if mode == "" {
		t.Skip("run by other tests")
	}

	pipelinetest.CheckIdempotent(t, newNode, steps[mode])
}
