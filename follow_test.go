//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package pipeline_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/ruffel/pipeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fast polls often, so the tests don't wait on the default interval.
func fast(opts pipeline.FollowOptions) pipeline.FollowOptions {
	opts.Poll = 2 * time.Millisecond

	return opts
}

// chatty writes one output line and one message, then passes.
func chatty(line string) pipeline.Step {
	return pipeline.StepFunc(func(_ context.Context, s *pipeline.StepContext) error {
		_, _ = s.Stdout().Write([]byte(line + "\n"))
		s.Info("info " + line)

		return nil
	})
}

func TestFollow_AFinishedRunIsReplayedAndFollowReturns(t *testing.T) {
	t.Parallel()

	path := journalPath(t)
	written := runToFile(t, path, mustPlan(t,
		pipeline.Group("g", pipeline.Effect("a", chatty("hello"))),
		pipeline.Effect("b", fail("boom")),
	))

	rec := &recorder{}
	require.NoError(t, pipeline.Follow(t.Context(), path, fast(pipeline.FollowOptions{}), rec))
	assert.Equal(t, written, rec.all(), "a viewer gets exactly what the runner's observers got")

	// A viewer that replays from the start ends where the journal does.
	followed, err := pipeline.Replay(rec.all())
	require.NoError(t, err)

	inspected, err := pipeline.Inspect(path)
	require.NoError(t, err)
	assert.Equal(t, inspected.Element("p/b"), followed.Element("p/b"))
	assert.Equal(t, inspected.LastSeq, followed.LastSeq)
}

func TestFollow_AfterSkipsWhatTheViewerAlreadyHas(t *testing.T) {
	t.Parallel()

	path := journalPath(t)
	written := runToFile(t, path, mustPlan(t, pipeline.Effect("a", chatty("hello"))))

	rec := &recorder{}
	require.NoError(t, pipeline.Follow(t.Context(), path, fast(pipeline.FollowOptions{After: written[2].Seq}), rec))
	assert.Equal(t, written[3:], rec.all())
}

func TestFollow_AJournalThatDoesNotExist(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	require.NoError(t, pipeline.Follow(t.Context(), journalPath(t), fast(pipeline.FollowOptions{}), rec),
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
		done <- pipeline.Follow(ctx, path, fast(pipeline.FollowOptions{KeepFollowing: true}), rec)
	}()

	written := runToFile(t, path, mustPlan(t, pipeline.Effect("a", chatty("hello"))))

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

	plan := func() *pipeline.Plan {
		return mustPlan(t,
			pipeline.Effect("old", chatty("old output")),
			pipeline.Effect("flaky", pipeline.StepFunc(func(_ context.Context, s *pipeline.StepContext) error {
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
	j, err := pipeline.OpenJournal(path)
	require.NoError(t, err)

	_, err = pipeline.Run(t.Context(), plan(), j)
	require.Error(t, err)

	rec := &recorder{}
	done := make(chan error, 1)

	go func() {
		done <- pipeline.Follow(t.Context(), path, fast(pipeline.FollowOptions{History: pipeline.HistoryLifecycle}), rec)
	}()

	// Wait until the viewer has caught up with the first attempt.
	require.Eventually(t, func() bool {
		return len(kinds(rec.all(), pipeline.EventRunFinished)) == 1
	}, 10*time.Second, time.Millisecond)

	failing = false

	_, err = pipeline.Run(t.Context(), plan(), j)
	require.NoError(t, err)
	require.NoError(t, j.Close())
	require.NoError(t, <-done, "the run finished and the runner closed the journal")

	output := kinds(rec.all(), pipeline.EventOutput)
	lines := make([]string, 0, len(output))

	for _, e := range output {
		lines = append(lines, e.Line)
	}

	assert.Equal(t, []string{"new output"}, lines, "old output is skipped, live output is not")
	assert.Empty(t, kinds(rec.all(), pipeline.EventMessage))
	assert.Len(t, kinds(rec.all(), pipeline.EventRunStarted), 2, "lifecycle history is all there")

	state, err := pipeline.Replay(rec.all())
	require.NoError(t, err)
	assert.False(t, state.Unfinished())
	assert.Equal(t, pipeline.StatusSkipped, state.Element("p/old").Status)
}

// A run that stopped without finishing, with nobody holding the journal: the
// viewer is told, instead of waiting forever.
func TestFollow_ReportsARunnerThatIsGone(t *testing.T) {
	t.Parallel()

	path := journalPath(t)

	// The journal stops taking writes after the step starts: the run ends
	// with no run.finished, as a crash would leave it.
	j, err := pipeline.OpenJournal(path, pipeline.WithWriteHook(func(e pipeline.Event) error {
		if e.Kind == pipeline.EventStepFinished {
			return errors.New("crashed")
		}

		return nil
	}))
	require.NoError(t, err)

	_, err = pipeline.Run(t.Context(), mustPlan(t, pipeline.Effect("a", ok())), j)
	require.Error(t, err)

	rec := &recorder{}
	done := make(chan error, 1)

	go func() { done <- pipeline.Follow(t.Context(), path, fast(pipeline.FollowOptions{}), rec) }()

	// While the runner still holds the journal, the viewer waits.
	select {
	case err := <-done:
		t.Fatalf("Follow returned %v while a process still holds the journal", err)
	case <-time.After(30 * time.Millisecond):
	}

	require.NoError(t, j.Close())
	require.ErrorIs(t, <-done, pipeline.ErrRunnerGone)
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
		state, err := pipeline.Inspect(path)

		return err == nil && state.Live
	}, 30*time.Second, 5*time.Millisecond)

	rec := &recorder{}
	done := make(chan error, 1)

	go func() { done <- pipeline.Follow(t.Context(), path, fast(pipeline.FollowOptions{}), rec) }()

	// The viewer sees the step record its value while the child is alive.
	require.Eventually(t, func() bool {
		return len(kinds(rec.all(), pipeline.EventValue)) == 1
	}, 30*time.Second, 5*time.Millisecond)

	require.NoError(t, child.Process.Kill())
	_ = child.Wait()

	require.ErrorIs(t, <-done, pipeline.ErrRunnerGone)
	assert.Empty(t, kinds(rec.all(), pipeline.EventRunFinished))
}

func TestFollow_WaitsForAHalfWrittenLineToBeCompleted(t *testing.T) {
	t.Parallel()

	path := journalPath(t)
	written := runToFile(t, path, mustPlan(t, pipeline.Effect("a", ok())))

	// A line being written: its first half is in the file, the rest is not.
	appendBytes(t, path, `{"v":1,"seq":99,"kind":"mess`)

	rec := &recorder{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() {
		done <- pipeline.Follow(ctx, path, fast(pipeline.FollowOptions{KeepFollowing: true}), rec)
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
			runToFile(t, path, mustPlan(t, pipeline.Effect("a", ok())))
			appendBytes(t, path, bad)

			err := pipeline.Follow(t.Context(), path, fast(pipeline.FollowOptions{}), &recorder{})
			require.ErrorIs(t, err, pipeline.ErrJournalCorrupt)
			require.ErrorContains(t, err, "line 6")
		})
	}
}

func TestFollow_AnObserverPanicEndsFollowWithAnError(t *testing.T) {
	t.Parallel()

	path := journalPath(t)
	runToFile(t, path, mustPlan(t, pipeline.Effect("a", ok())))

	bad := pipeline.ObserverFunc(func(context.Context, pipeline.Event) { panic("renderer bug") })

	err := pipeline.Follow(t.Context(), path, fast(pipeline.FollowOptions{}), bad)
	require.ErrorContains(t, err, "renderer bug")
	require.NotErrorIs(t, err, pipeline.ErrJournalCorrupt, "the journal is fine; the viewer is not")
}

// Output written while the viewer is still replaying history is live output:
// the history filter covers only what the journal held when Follow began.
func TestFollow_HistoryEndsWhereTheJournalStoodAtTheStart(t *testing.T) {
	t.Parallel()

	path := journalPath(t)
	written := runToFile(t, path, mustPlan(t, pipeline.Effect("a", chatty("old output"))))
	last := written[len(written)-1].Seq

	rec := &recorder{}
	appended := false

	// The "runner" writes a new line while the viewer handles its first
	// replayed event.
	duringReplay := pipeline.ObserverFunc(func(ctx context.Context, e pipeline.Event) {
		if !appended {
			appended = true

			appendBytes(t, path, fmt.Sprintf(`{"v":1,"seq":%d,"kind":"output","path":"p/a","line":"live output"}`+"\n", last+1))
		}

		rec.OnEvent(ctx, e)
	})

	opts := fast(pipeline.FollowOptions{History: pipeline.HistoryLifecycle})
	require.NoError(t, pipeline.Follow(t.Context(), path, opts, duringReplay))

	output := kinds(rec.all(), pipeline.EventOutput)
	require.Len(t, output, 1, "old output is skipped; output written during the replay is not")
	assert.Equal(t, "live output", output[0].Line)
}

// Cancelling stops delivery at the next event, and is not journal damage.
func TestFollow_CancellationStopsDeliveryAtOnce(t *testing.T) {
	t.Parallel()

	path := journalPath(t)
	runToFile(t, path, mustPlan(t, pipeline.Effect("a", chatty("hello")), pipeline.Effect("b", ok())))

	ctx, cancel := context.WithCancel(t.Context())
	rec := &recorder{}

	cancelOnFirst := pipeline.ObserverFunc(func(ctx context.Context, e pipeline.Event) {
		rec.OnEvent(ctx, e)
		cancel()
	})

	err := pipeline.Follow(ctx, path, fast(pipeline.FollowOptions{}), cancelOnFirst)
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, pipeline.ErrJournalCorrupt)
	assert.Len(t, rec.all(), 1, "nothing is delivered after the cancel")

	// Already cancelled: nothing is delivered, and the answer is the cancel,
	// not "the run has ended".
	rec = &recorder{}
	err = pipeline.Follow(ctx, path, fast(pipeline.FollowOptions{}), rec)
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, rec.all())
}
