//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package runbook_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ruffel/pipeline/runbook"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func journalPath(t *testing.T) string {
	t.Helper()

	return filepath.Join(t.TempDir(), "upgrade.journal")
}

// runToFile runs plan against a new file journal at path and closes it.
func runToFile(t *testing.T, path string, plan *runbook.Plan) []runbook.Event {
	t.Helper()

	j, err := runbook.OpenJournal(path)
	require.NoError(t, err)

	rec := &recorder{}

	_, _ = runbook.Run(t.Context(), plan, j, runbook.WithObservers(rec))
	require.NoError(t, j.Close())

	return rec.all()
}

func appendBytes(t *testing.T, path, data string) {
	t.Helper()

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0) //nolint:gosec // A file in the test's own temp dir.
	require.NoError(t, err)

	_, err = f.WriteString(data)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

func TestOpenJournal_WhatIsWrittenIsWhatIsReadBack(t *testing.T) {
	t.Parallel()

	path := journalPath(t)
	big := strings.Repeat("x", 1<<20)

	seen := runToFile(t, path, mustPlan(t,
		runbook.Effect("a", runbook.StepFunc(func(_ context.Context, s *runbook.StepContext) error {
			s.Info(big) // far longer than any fixed-size line reader would take

			return s.Record("dir", "/var/backups/42")
		})),
		runbook.Effect("b", fail("boom")),
	))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "journals are private")

	j, err := runbook.OpenJournal(path)
	require.NoError(t, err)

	events, err := j.Events()
	require.NoError(t, err)
	assert.Equal(t, seen, events, "a reopened journal holds exactly what observers saw")

	state, err := j.State()
	require.NoError(t, err)
	assert.Equal(t, runbook.StatusFailed, state.Element("p/b").Status)
	assert.Equal(t, os.Getpid(), events[0].PID)

	_, err = runbook.Run(t.Context(), mustPlan(t, runbook.Effect("a", ok())), j)
	require.ErrorIs(t, err, runbook.ErrPlanChanged, "the attempt on disk is seen")
	require.NoError(t, j.Close())
}

func TestOpenJournal_HasOneWriterAtATime(t *testing.T) {
	t.Parallel()

	path := journalPath(t)

	first, err := runbook.OpenJournal(path)
	require.NoError(t, err)

	_, err = runbook.OpenJournal(path)
	require.ErrorIs(t, err, runbook.ErrJournalBusy)

	var busy *runbook.JournalBusyError
	require.ErrorAs(t, err, &busy)
	assert.Zero(t, busy.PID, "no run has been written yet")

	_, err = runbook.Run(t.Context(), mustPlan(t, runbook.Effect("a", ok())), first)
	require.NoError(t, err)

	_, err = runbook.OpenJournal(path)
	require.ErrorAs(t, err, &busy)
	assert.Equal(t, os.Getpid(), busy.PID)
	assert.Contains(t, err.Error(), "in use")

	require.NoError(t, first.Close())

	second, err := runbook.OpenJournal(path)
	require.NoError(t, err, "Close releases the lock")
	require.NoError(t, second.Close())
}

func TestInspect_ReadsWithoutTheLockAndReportsAWriter(t *testing.T) {
	t.Parallel()

	path := journalPath(t)

	state, err := runbook.Inspect(path)
	require.NoError(t, err, "a journal that doesn't exist yet is an empty state")
	assert.Empty(t, state.Attempts)
	assert.False(t, state.Live)

	j, err := runbook.OpenJournal(path)
	require.NoError(t, err)

	_, err = runbook.Run(t.Context(), mustPlan(t, runbook.Effect("a", ok())), j)
	require.NoError(t, err)

	state, err = runbook.Inspect(path)
	require.NoError(t, err)
	assert.True(t, state.Live, "a writer holds the journal")
	assert.Equal(t, runbook.StatusPassed, state.Element("p/a").Status)

	// Inspecting never gets in a writer's way.
	for range 50 {
		_, err = runbook.Inspect(path)
		require.NoError(t, err)
	}

	require.NoError(t, j.Close())

	state, err = runbook.Inspect(path)
	require.NoError(t, err)
	assert.False(t, state.Live)
	assert.False(t, state.Unfinished())
}

func TestOpenJournal_RemovesAHalfWrittenLastLine(t *testing.T) {
	t.Parallel()

	path := journalPath(t)
	seen := runToFile(t, path, mustPlan(t, runbook.Effect("a", ok())))

	whole, err := os.Stat(path)
	require.NoError(t, err)

	appendBytes(t, path, `{"v":1,"seq":99,"kind":"step.sta`)

	state, err := runbook.Inspect(path)
	require.NoError(t, err, "readers ignore the torn line")
	assert.Equal(t, seen[len(seen)-1].Seq, state.LastSeq)

	j, err := runbook.OpenJournal(path)
	require.NoError(t, err)

	events, err := j.Events()
	require.NoError(t, err)
	assert.Equal(t, seen, events)
	require.NoError(t, j.Close())

	repaired, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, whole.Size(), repaired.Size(), "the torn line is gone from the file")
}

func TestOpenJournal_RefusesACorruptJournal(t *testing.T) {
	t.Parallel()

	for name, bad := range map[string]string{
		"not an event":          "this is not json\n",
		"newer schema version":  `{"v":99,"seq":100,"kind":"run.started"}` + "\n",
		"sequence goes back":    `{"v":1,"seq":1,"kind":"run.finished"}` + "\n",
		"damage before the end": "garbage\n" + `{"v":1,"seq":100,"kind":"run.finished"}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			path := journalPath(t)
			runToFile(t, path, mustPlan(t, runbook.Effect("a", ok())))
			appendBytes(t, path, bad)

			_, err := runbook.OpenJournal(path)
			require.ErrorIs(t, err, runbook.ErrJournalCorrupt)
			assert.Contains(t, err.Error(), "line 6", "the error names the line")

			before, err := os.ReadFile(path) //nolint:gosec // A file in the test's own temp dir.
			require.NoError(t, err)
			assert.True(t, strings.HasSuffix(string(before), bad), "a corrupt journal is left as it is")
		})
	}
}

// A journal opened by a relative path keeps reading the same file after the
// process changes directory. It is not parallel: it changes that directory.
func TestOpenJournal_RelativePathSurvivesChdir(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	j, err := runbook.OpenJournal("upgrade.journal")
	require.NoError(t, err)

	_, err = runbook.Run(t.Context(), mustPlan(t, runbook.Effect("a", ok())), j)
	require.NoError(t, err)

	t.Chdir(t.TempDir())

	events, err := j.Events()
	require.NoError(t, err, "reads still find the journal")
	assert.Len(t, events, 5)
	require.NoError(t, j.Close())

	_, err = os.Stat(filepath.Join(dir, "upgrade.journal"))
	require.NoError(t, err)
}

// Inspect and OpenJournal call the same damage the same thing.
func TestInspect_ReportsReplayDamageAsCorrupt(t *testing.T) {
	t.Parallel()

	path := journalPath(t)
	runToFile(t, path, mustPlan(t, runbook.Effect("a", ok())))
	appendBytes(t, path, `{"v":1,"seq":1,"kind":"run.finished"}`+"\n")

	_, err := runbook.Inspect(path)
	require.ErrorIs(t, err, runbook.ErrJournalCorrupt)
	require.ErrorContains(t, err, "sequence numbers must increase")

	_, err = runbook.OpenJournal(path)
	require.ErrorIs(t, err, runbook.ErrJournalCorrupt)
}

// The child half of TestOpenJournal_SurvivesAKilledWriter: it runs a plan
// against the journal named in the environment, and never returns.
//
//nolint:paralleltest // Runs alone, as the whole of a child process.
func TestOpenJournal_KilledWriterChild(t *testing.T) {
	path := os.Getenv("RUNBOOK_KILL_TEST_JOURNAL")
	if path == "" {
		t.Skip("runs only as a child process")
	}

	j, err := runbook.OpenJournal(path)
	require.NoError(t, err)

	_, _ = runbook.Run(context.Background(), mustPlan(t,
		runbook.Effect("done", ok()),
		runbook.Effect("interrupted", runbook.StepFunc(func(_ context.Context, s *runbook.StepContext) error {
			if err := s.Record("previous-version", "8.1.2"); err != nil {
				return err
			}

			// "Changing the system" until killed. A timer, not a bare select:
			// with every goroutine blocked and no timer pending, the runtime
			// can end the process itself as deadlocked.
			time.Sleep(10 * time.Minute)

			return errors.New("the child was never killed")
		})),
	), j)
}

// A writer killed with SIGKILL leaves a journal the next process can open:
// the kernel releases the lock, and everything durable is there.
func TestOpenJournal_SurvivesAKilledWriter(t *testing.T) {
	t.Parallel()

	path := journalPath(t)

	//nolint:gosec // Re-runs this test binary.
	child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestOpenJournal_KilledWriterChild$")

	child.Env = append(os.Environ(), "RUNBOOK_KILL_TEST_JOURNAL="+path)
	require.NoError(t, child.Start())

	t.Cleanup(func() { _ = child.Process.Kill() })

	// Wait until the child's step has recorded its value and is "working".
	require.Eventually(t, func() bool {
		state, err := runbook.Inspect(path)
		if err != nil {
			return false
		}

		_, recorded := state.Value("p/interrupted", "previous-version")

		return recorded && state.Live
	}, 30*time.Second, 10*time.Millisecond)

	_, err := runbook.OpenJournal(path)
	require.ErrorIs(t, err, runbook.ErrJournalBusy, "the child holds the journal")

	var busy *runbook.JournalBusyError
	require.ErrorAs(t, err, &busy)
	assert.Equal(t, child.Process.Pid, busy.PID)

	require.NoError(t, child.Process.Kill())
	_ = child.Wait()

	state, err := runbook.Inspect(path)
	require.NoError(t, err)
	assert.False(t, state.Live, "the kernel released the lock")
	assert.True(t, state.Unfinished())

	j, err := runbook.OpenJournal(path)
	require.NoError(t, err)

	state, err = j.State()
	require.NoError(t, err)
	assert.Equal(t, runbook.StatusPassed, state.Element("p/done").Status)

	interrupted := state.Element("p/interrupted")
	assert.Equal(t, runbook.StatusStarted, interrupted.Status)
	assert.True(t, interrupted.RunCalled)

	v, ok := state.Value("p/interrupted", "previous-version")
	require.True(t, ok, "what the step recorded before it died is there for Undo and resume")
	assert.JSONEq(t, `"8.1.2"`, string(v))
	require.NoError(t, j.Close())
}

// The point of the file journal: a run that failed in one process is resumed
// by another, from what is on disk.
func TestOpenJournal_ResumeAcrossAReopen(t *testing.T) {
	t.Parallel()

	path := journalPath(t)
	c := &calls{}
	failing := true

	plan := func() *runbook.Plan {
		return mustPlan(t,
			runbook.Effect("a", runbook.StepFunc(func(_ context.Context, s *runbook.StepContext) error {
				c.add("a")

				return s.Record("dir", "/var/backups/42")
			})),
			runbook.Effect("b", counted(c, "b", &failing)),
			runbook.Effect("c", runbook.StepFunc(func(_ context.Context, s *runbook.StepContext) error {
				var dir string

				if found, err := s.Recorded("p/a", "dir", &dir); err != nil || !found || dir != "/var/backups/42" {
					return fmt.Errorf("recorded dir: found=%v dir=%q: %w", found, dir, err)
				}

				return nil
			})),
		)
	}

	first, err := runbook.OpenJournal(path)
	require.NoError(t, err)

	_, err = runbook.Run(t.Context(), plan(), first)
	require.ErrorContains(t, err, "p/b: b failed")
	require.NoError(t, first.Close())

	failing = false

	second, err := runbook.OpenJournal(path)
	require.NoError(t, err)

	res, err := runbook.Run(t.Context(), plan(), second)
	require.NoError(t, err)
	assert.Equal(t, 2, res.Attempt)
	require.NoError(t, second.Close())

	assert.Equal(t, 1, c.get("a"), "a ran in the first process only")
	assert.Equal(t, 2, c.get("b"))

	state, err := runbook.Inspect(path)
	require.NoError(t, err)
	require.Len(t, state.Attempts, 2)
	assert.Equal(t, runbook.OutcomeFailed, state.Attempts[0].Outcome)
	assert.Equal(t, runbook.OutcomePassed, state.Attempts[1].Outcome)
	assert.False(t, state.Unfinished())
	assert.False(t, state.Live)
}

// State computed by passing over output undecoded must equal the state
// computed by decoding every event.
func TestInspect_FastReplayMatchesAFullDecode(t *testing.T) {
	t.Parallel()

	path := journalPath(t)
	failing := true

	plan := mustPlan(t,
		runbook.Group("g",
			runbook.Effect("chatty", runbook.StepFunc(func(_ context.Context, s *runbook.StepContext) error {
				s.Info(`a message with "kind":"run.finished" in it`)
				s.Warn(strings.Repeat("long ", 30000)) // longer than the reader's buffer
				s.Progress("copy", "copying", 1, 2)
				_, _ = s.Stdout().Write([]byte("line one\nline two\n"))

				if err := s.Custom("x.y", map[string]any{"kind": "step.finished"}); err != nil {
					return err
				}

				if err := s.Item("node1", runbook.ItemFailed, "busy"); err != nil {
					return err
				}

				return s.Record("dir", "/var/backups/42")
			})),
			runbook.Effect("flaky", counted(&calls{}, "flaky", &failing)).Retry(2, time.Millisecond),
		),
	)

	j, err := runbook.OpenJournal(path)
	require.NoError(t, err)

	_, err = runbook.Run(t.Context(), plan, j)
	require.Error(t, err)

	failing = false

	_, err = runbook.Run(t.Context(), plan, j)
	require.NoError(t, err)

	events, err := j.Events()
	require.NoError(t, err)

	decoded, err := runbook.Replay(events)
	require.NoError(t, err)

	fromJournal, err := j.State()
	require.NoError(t, err)
	require.NoError(t, j.Close())

	inspected, err := runbook.Inspect(path)
	require.NoError(t, err)

	assert.Equal(t, decoded, fromJournal, "Journal.State")
	assert.Equal(t, decoded, inspected, "Inspect")
}

// Damage inside an output line: OpenJournal, which checks every line before
// a run builds on the journal, refuses it. Inspect reads only the envelope of
// output lines, and passes over it.
func TestOpenJournal_ChecksOutputLinesThatInspectPassesOver(t *testing.T) {
	t.Parallel()

	path := journalPath(t)
	written := runToFile(t, path, mustPlan(t, runbook.Effect("a", ok())))
	next := written[len(written)-1].Seq + 1

	appendBytes(t, path, fmt.Sprintf(
		`{"v":1,"seq":%d,"ts":"2026-11-11T09:00:00Z","kind":"output","line":"a bad \x escape"}`+"\n", next))

	state, err := runbook.Inspect(path)
	require.NoError(t, err)
	assert.Equal(t, next, state.LastSeq)

	_, err = runbook.OpenJournal(path)
	require.ErrorIs(t, err, runbook.ErrJournalCorrupt)
}

// A line that sets its sequence number twice: the decoder uses the later
// one, and so must every reader, fast path or not.
func TestInspect_ALaterDuplicateKeyIsReadAsTheDecoderReadsIt(t *testing.T) {
	t.Parallel()

	path := journalPath(t)
	written := runToFile(t, path, mustPlan(t, runbook.Effect("a", ok())))
	last := written[len(written)-1].Seq

	appendBytes(t, path, fmt.Sprintf(
		`{"v":1,"seq":%d,"ts":"2026-11-11T09:00:00Z","kind":"output","line":"x","seq":%d}`+"\n", last+1, last+50))

	state, err := runbook.Inspect(path)
	require.NoError(t, err)
	assert.Equal(t, last+50, state.LastSeq, "Inspect")

	j, err := runbook.OpenJournal(path)
	require.NoError(t, err)

	t.Cleanup(func() { _ = j.Close() })

	state, err = j.State()
	require.NoError(t, err)
	assert.Equal(t, last+50, state.LastSeq, "Journal.State")
}
