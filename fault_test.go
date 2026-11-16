//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package pipeline_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ruffel/pipeline"
	"github.com/ruffel/pipeline/pipelinetest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// remote is a system on the far side of a call. It does what it is asked
// every time it is asked, and remembers which requests it has had. It can
// lose a reply: the work is done, and the caller is told that it failed.
type remote struct {
	mu      sync.Mutex
	applied map[string]int
	total   int
	lose    int
}

func (r *remote) add(id string, n int) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.applied == nil {
		r.applied = map[string]int{}
	}

	r.applied[id]++
	r.total += n

	if r.lose > 0 {
		r.lose--

		return errors.New("connection reset")
	}

	return nil
}

func (r *remote) has(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.applied[id] > 0
}

// adds asks the remote for its work, and has no way to tell whether the
// work was done.
type adds struct {
	r  *remote
	id string
	n  int
}

func (a adds) Run(context.Context, *pipeline.StepContext) error { return a.r.add(a.id, a.n) }

// checkedAdds asks the remote first whether it has had the request.
type checkedAdds struct{ adds }

func (a checkedAdds) Check(context.Context, *pipeline.StepContext) (bool, error) {
	return a.r.has(a.id), nil
}

// The work was done, and the run never heard: the reply was lost, or the
// process died before the outcome was stored. The journal can't tell that
// from work that was never done. A step with a Check asks the system, finds
// it done, and passes without doing it again. A step without one does it
// again: its Run is all the next attempt has.
func TestResume_AnEffectWhoseSuccessWasNeverRecorded(t *testing.T) {
	t.Parallel()

	const id, path = "request-1", "p/credit"

	// Each returns the journal to run against, and what stands for the
	// process starting again before the next attempt.
	for name, lost := range map[string]func(*remote) (*pipeline.Journal, func()){
		"the reply is lost": func(r *remote) (*pipeline.Journal, func()) {
			r.lose = 1

			return pipeline.MemoryJournal(), func() {}
		},
		"the process dies before the outcome is stored": func(*remote) (*pipeline.Journal, func()) {
			crash := pipelinetest.CrashAfter(path, pipelinetest.Running)

			return crash.Journal, crash.Restart
		},
	} {
		for kind, tc := range map[string]struct {
			step  func(adds) pipeline.Step
			twice bool
		}{
			"with a Check": {step: func(a adds) pipeline.Step { return checkedAdds{a} }},
			"with none":    {step: func(a adds) pipeline.Step { return a }, twice: true},
		} {
			t.Run(name+", "+kind, func(t *testing.T) {
				t.Parallel()

				var c calls

				r := &remote{}
				j, restart := lost(r)
				plan := mustPlan(t,
					pipeline.Effect("credit", tc.step(adds{r: r, id: id, n: 100})),
					pipeline.Effect("after", counted(&c, "after", nil)),
				)

				_, err := pipeline.Run(t.Context(), plan, j)
				require.Error(t, err)
				require.Equal(t, 1, r.applied[id], "the work was done")
				require.Zero(t, c.get("after"))

				restart()

				out := resumeOn(t, j, plan)
				require.NoError(t, out.err)
				assert.Equal(t, 1, c.get("after"))

				if tc.twice {
					assert.Equal(t, 2, r.applied[id], "with no Check, the only way to finish the step is to run it")
					assert.Equal(t, 200, r.total)

					return
				}

				assert.Equal(t, 1, r.applied[id], "applied once")
				assert.Equal(t, 100, r.total)
				assert.Contains(t, trace(out.events), "step.finished "+path+" passed/recovered")
			})
		}
	}
}

// encoded returns a string whose encoding, as Record stores it, is n bytes.
func encoded(n int) string { return strings.Repeat("x", n-len(`""`)) }

// MaxRecordBytes is one step's allowance for one attempt, counted over
// everything it records. A Record that would go past it returns an error
// and stores nothing: nothing is cut short to fit.
func TestRun_TheRecordLimitIsTheStepsForTheAttempt(t *testing.T) {
	t.Parallel()

	const kib = 1 << 10

	record := func(fn func(s *pipeline.StepContext) error) pipeline.StepNode {
		return pipeline.Effect("a", pipeline.StepFunc(func(_ context.Context, s *pipeline.StepContext) error { return fn(s) }))
	}

	t.Run("a value of exactly the limit is stored whole", func(t *testing.T) {
		t.Parallel()

		var over error

		out := runPlan(t, t.Context(), mustPlan(t, record(func(s *pipeline.StepContext) error {
			over = s.Record("over", encoded(pipeline.MaxRecordBytes+1))

			return s.Record("whole", encoded(pipeline.MaxRecordBytes))
		})))
		require.NoError(t, out.err)
		require.ErrorContains(t, over, "would exceed 65536 bytes")

		got, ok := out.state.Value("p/a", "whole")
		require.True(t, ok)
		assert.Len(t, got, pipeline.MaxRecordBytes)

		_, ok = out.state.Value("p/a", "over")
		assert.False(t, ok, "a value that does not fit is not stored in part")
	})

	t.Run("many keys are counted together", func(t *testing.T) {
		t.Parallel()

		var last error

		out := runPlan(t, t.Context(), mustPlan(t, record(func(s *pipeline.StepContext) error {
			for i := range pipeline.MaxRecordBytes / kib {
				if err := s.Record(fmt.Sprintf("node%d", i), encoded(kib)); err != nil {
					return err
				}
			}

			last = s.Record("one-more", "")

			return nil
		})))
		require.NoError(t, out.err)
		require.ErrorContains(t, last, `record "one-more": the step's recorded values would exceed`)
	})

	t.Run("a key recorded again is counted again", func(t *testing.T) {
		t.Parallel()

		var errs []error

		out := runPlan(t, t.Context(), mustPlan(t, record(func(s *pipeline.StepContext) error {
			for range 3 {
				errs = append(errs, s.Record("state", encoded(pipeline.MaxRecordBytes/2)))
			}

			return nil
		})))
		require.NoError(t, out.err)
		require.NoError(t, errs[0])
		require.NoError(t, errs[1])
		require.ErrorContains(t, errs[2], "would exceed", "though the step holds one value under one key")
	})

	t.Run("every try of the step is counted", func(t *testing.T) {
		t.Parallel()

		var errs []error

		out := runPlan(t, t.Context(), mustPlan(t, record(func(s *pipeline.StepContext) error {
			errs = append(errs, s.Record("progress", encoded(pipeline.MaxRecordBytes/2)))

			return errors.New("try again")
		}).Retry(3, 0)))
		require.Error(t, out.err)
		require.Len(t, errs, 3)
		require.NoError(t, errs[0])
		require.NoError(t, errs[1])
		require.ErrorContains(t, errs[2], "would exceed")
	})

	t.Run("another step, and another attempt, start from nothing", func(t *testing.T) {
		t.Parallel()

		fails := true
		full := func(name string) pipeline.StepNode {
			return pipeline.Effect(name, pipeline.StepFunc(func(_ context.Context, s *pipeline.StepContext) error {
				if err := s.Record("all", encoded(pipeline.MaxRecordBytes)); err != nil {
					return err
				}

				if name == "b" && fails {
					return errors.New("b failed")
				}

				return nil
			}))
		}

		j := pipeline.MemoryJournal()
		plan := mustPlan(t, full("a"), full("b"))

		require.ErrorContains(t, resumeOn(t, j, plan).err, "b failed")

		fails = false

		require.NoError(t, resumeOn(t, j, plan).err)
	})
}

// ledger is a system that outlives the process working on it: a file that
// gains a line for each request applied to it.
type ledger struct{ path string }

func (l ledger) lines(t *testing.T) []string {
	t.Helper()

	data, err := os.ReadFile(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	require.NoError(t, err)

	return strings.Fields(string(data))
}

// post applies a request: it appends its line, and returns once the line is
// on disk.
func (l ledger) post(id string) error {
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}

	if _, err := f.WriteString(id + "\n"); err != nil {
		_ = f.Close()

		return err
	}

	if err := f.Sync(); err != nil {
		_ = f.Close()

		return err
	}

	return f.Close()
}

// posting posts one request to the ledger, and checks first whether the
// ledger already has it. at is called before and after the post.
type posting struct {
	l  ledger
	id string
	at func(s *pipeline.StepContext, stage string)
}

func (p posting) Check(context.Context, *pipeline.StepContext) (bool, error) {
	data, err := os.ReadFile(p.l.path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}

	return strings.Contains(string(data), p.id+"\n"), err
}

func (p posting) Run(_ context.Context, s *pipeline.StepContext) error {
	if err := s.Record("request", p.id); err != nil {
		return err
	}

	p.at(s, "before the effect")

	if err := p.l.post(p.id); err != nil {
		return err
	}

	p.at(s, "after the effect")

	return nil
}

func postingPlan(t *testing.T, c *calls, l ledger, at func(*pipeline.StepContext, string)) *pipeline.Plan {
	t.Helper()

	return mustPlan(t,
		pipeline.Effect("post", posting{l: l, id: "request-1", at: at}),
		pipeline.Effect("after", counted(c, "after", nil)),
	)
}

// The child half of TestResume_AProcessKilledAroundItsEffect: it runs the
// plan against the journal and ledger named in the environment, records
// that it has reached the stage it is to die at, and waits there to be
// killed.
//
//nolint:paralleltest // Runs alone, as the whole of a child process.
func TestResume_KilledAroundItsEffectChild(t *testing.T) {
	dir, stage := os.Getenv("RUNBOOK_KILL_AT_DIR"), os.Getenv("RUNBOOK_KILL_AT_STAGE")
	if dir == "" {
		t.Skip("runs only as a child process")
	}

	j, err := pipeline.OpenJournal(filepath.Join(dir, "journal"))
	require.NoError(t, err)

	var c calls

	at := func(s *pipeline.StepContext, reached string) {
		if reached != stage {
			return
		}

		if err := s.Record("reached", reached); err != nil {
			panic(err)
		}

		// A timer, not a bare select: with every goroutine blocked and no
		// timer pending, the runtime can end the process as deadlocked.
		time.Sleep(10 * time.Minute)
	}

	_, _ = pipeline.Run(context.Background(), postingPlan(t, &c, ledger{filepath.Join(dir, "ledger")}, at), j)
}

// A process that is killed, not one whose journal is made to fail: on either
// side of the effect, with a real file journal and a system that outlives
// it. The next process resumes from what is on disk, and the request is in
// the ledger once.
//
// SIGKILL ends the process. It is not a power cut: what the process had
// written is still handed to the disk by the kernel. What the journal has
// synced is what a power cut would leave, and the file journal's own tests
// are about that.
func TestResume_AProcessKilledAroundItsEffect(t *testing.T) {
	t.Parallel()

	for stage, want := range map[string]string{
		// Nothing was posted: this attempt's Check says so, and its Run posts.
		"before the effect": "step.finished p/post passed",
		// It was posted, and no outcome was stored: Check finds it there.
		"after the effect": "step.finished p/post passed/recovered",
	} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			l := ledger{filepath.Join(dir, "ledger")}

			//nolint:gosec // Re-runs this test binary.
			child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestResume_KilledAroundItsEffectChild$")

			child.Env = append(os.Environ(), "RUNBOOK_KILL_AT_DIR="+dir, "RUNBOOK_KILL_AT_STAGE="+stage)
			require.NoError(t, child.Start())

			// Only ever this child, by the process handle it was started with.
			t.Cleanup(func() { _ = child.Process.Kill() })

			require.Eventually(t, func() bool {
				state, err := pipeline.Inspect(filepath.Join(dir, "journal"))
				if err != nil {
					return false
				}

				_, reached := state.Value("p/post", "reached")

				return reached && state.Live
			}, 30*time.Second, 10*time.Millisecond, "the child reaches the stage")

			require.NoError(t, child.Process.Kill())
			_ = child.Wait()

			posted := len(l.lines(t))
			assert.Equal(t, stage == "after the effect", posted == 1)

			j, err := pipeline.OpenJournal(filepath.Join(dir, "journal"))
			require.NoError(t, err)

			t.Cleanup(func() { _ = j.Close() })

			state, err := j.State()
			require.NoError(t, err)
			assert.True(t, state.Element("p/post").RunCalled, "the journal says Run may have been called, and no more")
			assert.Equal(t, pipeline.StatusStarted, state.Element("p/post").Status)

			request, recorded := state.Value("p/post", "request")
			require.True(t, recorded, "what was recorded before the effect is there to find it by")
			assert.JSONEq(t, `"request-1"`, string(request))

			var c calls

			out := resumeOn(t, j, postingPlan(t, &c, l, func(*pipeline.StepContext, string) {}))
			require.NoError(t, out.err)
			assert.Contains(t, trace(out.events), want)
			assert.Equal(t, []string{"request-1"}, l.lines(t), "posted once")
			assert.Equal(t, 1, c.get("after"))
		})
	}
}
