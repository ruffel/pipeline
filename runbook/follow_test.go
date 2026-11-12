//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package runbook_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/ruffel/pipeline/runbook"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fast polls often, so the tests don't wait on the default interval.
func fast(opts runbook.FollowOptions) runbook.FollowOptions {
	opts.Poll = 2 * time.Millisecond

	return opts
}

// chatty writes one output line and one message, then passes.
func chatty(line string) runbook.Step {
	return runbook.StepFunc(func(_ context.Context, s *runbook.StepContext) error {
		_, _ = s.Stdout().Write([]byte(line + "\n"))
		s.Info("info " + line)

		return nil
	})
}

func TestFollow_AFinishedRunIsReplayedAndFollowReturns(t *testing.T) {
	t.Parallel()

	path := journalPath(t)
	written := runToFile(t, path, mustPlan(t,
		runbook.Group("g", runbook.Effect("a", chatty("hello"))),
		runbook.Effect("b", fail("boom")),
	))

	rec := &recorder{}
	require.NoError(t, runbook.Follow(t.Context(), path, fast(runbook.FollowOptions{}), rec))
	assert.Equal(t, written, rec.all(), "a viewer gets exactly what the runner's observers got")

	// A viewer that replays from the start ends where the journal does.
	followed, err := runbook.Replay(rec.all())
	require.NoError(t, err)

	inspected, err := runbook.Inspect(path)
	require.NoError(t, err)
	assert.Equal(t, inspected.Element("p/b"), followed.Element("p/b"))
	assert.Equal(t, inspected.LastSeq, followed.LastSeq)
}

func TestFollow_AfterSkipsWhatTheViewerAlreadyHas(t *testing.T) {
	t.Parallel()

	path := journalPath(t)
	written := runToFile(t, path, mustPlan(t, runbook.Effect("a", chatty("hello"))))

	rec := &recorder{}
	require.NoError(t, runbook.Follow(t.Context(), path, fast(runbook.FollowOptions{After: written[2].Seq}), rec))
	assert.Equal(t, written[3:], rec.all())
}

func TestFollow_AJournalThatDoesNotExist(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	require.NoError(t, runbook.Follow(t.Context(), journalPath(t), fast(runbook.FollowOptions{}), rec),
		"nothing has run, and nothing is running")
	assert.Empty(t, rec.all())
}

// A viewer started before the run sees it live, and stops when told to.
func TestFollow_KeepFollowingSeesARunThatStartsLater(t *testing.T) {
	t.Parallel()

	path := journalPath(t)
	rec := &recorder{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() {
		done <- runbook.Follow(ctx, path, fast(runbook.FollowOptions{KeepFollowing: true}), rec)
	}()

	written := runToFile(t, path, mustPlan(t, runbook.Effect("a", chatty("hello"))))

	require.Eventually(t, func() bool { return len(rec.all()) == len(written) }, 10*time.Second, time.Millisecond)

	select {
	case err := <-done:
		t.Fatalf("Follow returned %v while told to keep following", err)
	case <-time.After(20 * time.Millisecond):
	}

	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	assert.Equal(t, written, rec.all())
}

// A viewer attaching late can skip old output, and still gets everything
// written from then on.
func TestFollow_LifecycleHistoryThenEverythingLive(t *testing.T) {
	t.Parallel()

	path := journalPath(t)
	failing := true

	plan := func() *runbook.Plan {
		return mustPlan(t,
			runbook.Effect("old", chatty("old output")),
			runbook.Effect("flaky", runbook.StepFunc(func(_ context.Context, s *runbook.StepContext) error {
				if failing {
					return errors.New("first attempt fails")
				}

				_, _ = s.Stdout().Write([]byte("new output\n"))

				return nil
			})),
		)
	}

	// The runner stays open between its two attempts, so the viewer attaches
	// to a live journal that already has history.
	j, err := runbook.OpenJournal(path)
	require.NoError(t, err)

	_, err = runbook.Run(t.Context(), plan(), j)
	require.Error(t, err)

	rec := &recorder{}
	done := make(chan error, 1)

	go func() {
		done <- runbook.Follow(t.Context(), path, fast(runbook.FollowOptions{History: runbook.HistoryLifecycle}), rec)
	}()

	// Wait until the viewer has caught up with the first attempt.
	require.Eventually(t, func() bool {
		return len(kinds(rec.all(), runbook.EventRunFinished)) == 1
	}, 10*time.Second, time.Millisecond)

	failing = false

	_, err = runbook.Run(t.Context(), plan(), j)
	require.NoError(t, err)
	require.NoError(t, j.Close())
	require.NoError(t, <-done, "the run finished and the runner closed the journal")

	output := kinds(rec.all(), runbook.EventOutput)
	lines := make([]string, 0, len(output))

	for _, e := range output {
		lines = append(lines, e.Line)
	}

	assert.Equal(t, []string{"new output"}, lines, "old output is skipped, live output is not")
	assert.Empty(t, kinds(rec.all(), runbook.EventMessage))
	assert.Len(t, kinds(rec.all(), runbook.EventRunStarted), 2, "lifecycle history is all there")

	state, err := runbook.Replay(rec.all())
	require.NoError(t, err)
	assert.False(t, state.Unfinished())
	assert.Equal(t, runbook.StatusSkipped, state.Element("p/old").Status)
}

// A run that stopped without finishing, with nobody holding the journal: the
// viewer is told, instead of waiting forever.
func TestFollow_ReportsARunnerThatIsGone(t *testing.T) {
	t.Parallel()

	path := journalPath(t)

	// The journal stops taking writes after the step starts: the run ends
	// with no run.finished, as a crash would leave it.
	j, err := runbook.OpenJournal(path, runbook.WithWriteHook(func(e runbook.Event) error {
		if e.Kind == runbook.EventStepFinished {
			return errors.New("crashed")
		}

		return nil
	}))
	require.NoError(t, err)

	_, err = runbook.Run(t.Context(), mustPlan(t, runbook.Effect("a", ok())), j)
	require.Error(t, err)

	rec := &recorder{}
	done := make(chan error, 1)

	go func() { done <- runbook.Follow(t.Context(), path, fast(runbook.FollowOptions{}), rec) }()

	// While the runner still holds the journal, the viewer waits.
	select {
	case err := <-done:
		t.Fatalf("Follow returned %v while a process still holds the journal", err)
	case <-time.After(30 * time.Millisecond):
	}

	require.NoError(t, j.Close())
	require.ErrorIs(t, <-done, runbook.ErrRunnerGone)
	assert.Len(t, rec.all(), 3, "run.started, step.started and step.running were delivered")
}

// The viewer is a separate process from the runner in real use. Here the
// runner is a real child process, killed while the viewer follows it.
func TestFollow_ARunnerKilledWhileAViewerWatches(t *testing.T) {
	t.Parallel()

	path := journalPath(t)

	//nolint:gosec // Re-runs this test binary.
	child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestOpenJournal_KilledWriterChild$")

	child.Env = append(os.Environ(), "RUNBOOK_KILL_TEST_JOURNAL="+path)
	require.NoError(t, child.Start())

	t.Cleanup(func() { _ = child.Process.Kill() })

	// Attach once the runner is up. A viewer that starts before any runner
	// has the journal returns at once: nothing has run.
	require.Eventually(t, func() bool {
		state, err := runbook.Inspect(path)

		return err == nil && state.Live
	}, 30*time.Second, 5*time.Millisecond)

	rec := &recorder{}
	done := make(chan error, 1)

	go func() { done <- runbook.Follow(t.Context(), path, fast(runbook.FollowOptions{}), rec) }()

	// The viewer sees the step record its value while the child is alive.
	require.Eventually(t, func() bool {
		return len(kinds(rec.all(), runbook.EventValue)) == 1
	}, 30*time.Second, 5*time.Millisecond)

	require.NoError(t, child.Process.Kill())
	_ = child.Wait()

	require.ErrorIs(t, <-done, runbook.ErrRunnerGone)
	assert.Empty(t, kinds(rec.all(), runbook.EventRunFinished))
}

func TestFollow_WaitsForAHalfWrittenLineToBeCompleted(t *testing.T) {
	t.Parallel()

	path := journalPath(t)
	written := runToFile(t, path, mustPlan(t, runbook.Effect("a", ok())))

	// A line being written: its first half is in the file, the rest is not.
	appendBytes(t, path, `{"v":1,"seq":99,"kind":"mess`)

	rec := &recorder{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() {
		done <- runbook.Follow(ctx, path, fast(runbook.FollowOptions{KeepFollowing: true}), rec)
	}()

	require.Eventually(t, func() bool { return len(rec.all()) == len(written) }, 10*time.Second, time.Millisecond)

	appendBytes(t, path, `age","level":"info","message":"late"}`+"\n")

	require.Eventually(t, func() bool { return len(rec.all()) == len(written)+1 }, 10*time.Second, time.Millisecond)
	assert.Equal(t, "late", rec.all()[len(written)].Message)

	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestFollow_RefusesACorruptJournal(t *testing.T) {
	t.Parallel()

	for name, bad := range map[string]string{
		"not an event":       "this is not json\n",
		"sequence goes back": `{"v":1,"seq":1,"kind":"run.finished"}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			path := journalPath(t)
			runToFile(t, path, mustPlan(t, runbook.Effect("a", ok())))
			appendBytes(t, path, bad)

			err := runbook.Follow(t.Context(), path, fast(runbook.FollowOptions{}), &recorder{})
			require.ErrorIs(t, err, runbook.ErrJournalCorrupt)
			require.ErrorContains(t, err, "line 6")
		})
	}
}

func TestFollow_AnObserverPanicEndsFollowWithAnError(t *testing.T) {
	t.Parallel()

	path := journalPath(t)
	runToFile(t, path, mustPlan(t, runbook.Effect("a", ok())))

	bad := runbook.ObserverFunc(func(context.Context, runbook.Event) { panic("renderer bug") })

	err := runbook.Follow(t.Context(), path, fast(runbook.FollowOptions{}), bad)
	require.ErrorContains(t, err, "renderer bug")
	require.NotErrorIs(t, err, runbook.ErrJournalCorrupt, "the journal is fine; the viewer is not")
}

// Output written while the viewer is still replaying history is live output:
// the history filter covers only what the journal held when Follow began.
func TestFollow_HistoryEndsWhereTheJournalStoodAtTheStart(t *testing.T) {
	t.Parallel()

	path := journalPath(t)
	written := runToFile(t, path, mustPlan(t, runbook.Effect("a", chatty("old output"))))
	last := written[len(written)-1].Seq

	rec := &recorder{}
	appended := false

	// The "runner" writes a new line while the viewer handles its first
	// replayed event.
	duringReplay := runbook.ObserverFunc(func(ctx context.Context, e runbook.Event) {
		if !appended {
			appended = true

			appendBytes(t, path, fmt.Sprintf(`{"v":1,"seq":%d,"kind":"output","path":"p/a","line":"live output"}`+"\n", last+1))
		}

		rec.OnEvent(ctx, e)
	})

	opts := fast(runbook.FollowOptions{History: runbook.HistoryLifecycle})
	require.NoError(t, runbook.Follow(t.Context(), path, opts, duringReplay))

	output := kinds(rec.all(), runbook.EventOutput)
	require.Len(t, output, 1, "old output is skipped; output written during the replay is not")
	assert.Equal(t, "live output", output[0].Line)
}

// Cancelling stops delivery at the next event, and is not journal damage.
func TestFollow_CancellationStopsDeliveryAtOnce(t *testing.T) {
	t.Parallel()

	path := journalPath(t)
	runToFile(t, path, mustPlan(t, runbook.Effect("a", chatty("hello")), runbook.Effect("b", ok())))

	ctx, cancel := context.WithCancel(t.Context())
	rec := &recorder{}

	cancelOnFirst := runbook.ObserverFunc(func(ctx context.Context, e runbook.Event) {
		rec.OnEvent(ctx, e)
		cancel()
	})

	err := runbook.Follow(ctx, path, fast(runbook.FollowOptions{}), cancelOnFirst)
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, runbook.ErrJournalCorrupt)
	assert.Len(t, rec.all(), 1, "nothing is delivered after the cancel")

	// Already cancelled: nothing is delivered, and the answer is the cancel,
	// not "the run has ended".
	rec = &recorder{}
	err = runbook.Follow(ctx, path, fast(runbook.FollowOptions{}), rec)
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, rec.all())
}
