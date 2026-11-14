//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package pipeline_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ruffel/pipeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fastControl makes a run read its control directory often, so tests don't
// wait on the default poll.
var fastControl = pipeline.WithControlPoll(5 * time.Millisecond) //nolint:gochecknoglobals // A fixed test option.

// liveRun is a run on a file journal, going on in the background.
type liveRun struct {
	path string
	j    *pipeline.Journal
	rec  *recorder
	done chan liveResult
}

type liveResult struct {
	res *pipeline.Result
	err error
}

// startRun starts plan on a new file journal, and closes the journal when
// the run returns.
func startRun(t *testing.T, plan *pipeline.Plan, opts ...pipeline.Option) *liveRun {
	t.Helper()

	return startRunAt(t, journalPath(t), plan, opts...)
}

// startUnreadRun starts a run that reads its control directory once, as it
// starts, and then not for an hour, and returns once that first read is
// done: a request made after it returns is not read.
//
// A request for another run, left before the run starts, is deleted unread
// by that first read, so once it is gone the read is done.
func startUnreadRun(t *testing.T, plan *pipeline.Plan, opts ...pipeline.Option) *liveRun {
	t.Helper()

	path := journalPath(t)
	other := filepath.Join(path+".control", "stop.another-run")

	require.NoError(t, os.Mkdir(path+".control", 0o700))
	require.NoError(t, os.WriteFile(other, []byte(`{}`), 0o600))

	lr := startRunAt(t, path, plan, append([]pipeline.Option{pipeline.WithControlPoll(time.Hour)}, opts...)...)

	require.Eventually(t, func() bool {
		_, err := os.Stat(other)

		return errors.Is(err, os.ErrNotExist)
	}, 10*time.Second, time.Millisecond, "the run's first read of its control directory")

	return lr
}

// startRunAt is startRun on the journal at path.
func startRunAt(t *testing.T, path string, plan *pipeline.Plan, opts ...pipeline.Option) *liveRun {
	t.Helper()

	j, err := pipeline.OpenJournal(path)
	require.NoError(t, err)

	lr := &liveRun{path: path, j: j, rec: &recorder{}, done: make(chan liveResult, 1)}
	opts = append([]pipeline.Option{pipeline.WithObservers(lr.rec)}, opts...)

	go func() {
		res, err := pipeline.Run(t.Context(), plan, j, opts...)
		_ = j.Close()

		lr.done <- liveResult{res: res, err: err}
	}()

	return lr
}

func (lr *liveRun) wait(t *testing.T) liveResult {
	t.Helper()

	select {
	case out := <-lr.done:
		return out
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not finish")

		return liveResult{}
	}
}

// blocking returns a step that says when it is running, and passes when
// release is closed.
func blocking(running chan<- struct{}, release <-chan struct{}) pipeline.Step {
	return pipeline.StepFunc(func(context.Context, *pipeline.StepContext) error {
		close(running)
		<-release

		return nil
	})
}

func uid() *int {
	u := os.Getuid()

	return &u
}

func TestRequestStop_StopsARunGracefully(t *testing.T) {
	t.Parallel()

	var c calls

	running, release := make(chan struct{}), make(chan struct{})
	lr := startRun(t, mustPlan(t,
		pipeline.Effect("a", blocking(running, release)),
		pipeline.Effect("b", counted(&c, "b", nil)),
	), fastControl)

	<-running
	require.NoError(t, pipeline.RequestStop(t.Context(), lr.path, pipeline.StopGraceful, "alice"))

	// RequestStop returns once the request is in the journal, while the step
	// still runs.
	state, err := pipeline.Inspect(lr.path)
	require.NoError(t, err)
	assert.Equal(t, &pipeline.StopRequest{
		Mode: pipeline.StopGraceful, By: "control", Who: "alice", UID: uid(),
	}, state.Attempts[0].Stop)
	assert.Equal(t, pipeline.StatusStarted, state.Element("p/a").Status)

	close(release)

	out := lr.wait(t)
	assert.Equal(t, pipeline.ErrStopped, out.err)
	assert.Equal(t, pipeline.OutcomeStopped, out.res.Outcome)
	assert.Zero(t, c.get("b"), "nothing starts after the stop")

	entries, err := os.ReadDir(lr.path + ".control")
	require.NoError(t, err)
	assert.Empty(t, entries, "the run took the request")
}

func TestRequestStop_CancelsARun(t *testing.T) {
	t.Parallel()

	lr := startRun(t, mustPlan(t,
		pipeline.Effect("a", waitForCancel()),
		pipeline.Effect("b", ok()),
	), fastControl)

	waitFor(t, lr.rec, pipeline.EventStepRunning)
	require.NoError(t, pipeline.RequestStop(t.Context(), lr.path, pipeline.StopCancel, "bob"))

	out := lr.wait(t)
	require.ErrorIs(t, out.err, context.Canceled)
	assert.Equal(t, pipeline.OutcomeCancelled, out.res.Outcome)

	state, err := pipeline.Inspect(lr.path)
	require.NoError(t, err)
	assert.Equal(t, &pipeline.StopRequest{
		Mode: pipeline.StopCancel, By: "control", Who: "bob", UID: uid(),
	}, state.Attempts[0].Stop)
	assert.Equal(t, pipeline.ErrorCancelled, state.Element("p/a").Error.Kind)
	assert.Equal(t, pipeline.StatusNotReached, state.Element("p/b").Status)
}

// A step that won't finish on its own is cancelled after a graceful stop,
// as an operator's second Ctrl-C would.
func TestRequestStop_ACancelAfterAGracefulStop(t *testing.T) {
	t.Parallel()

	lr := startRun(t, mustPlan(t, pipeline.Effect("a", waitForCancel())), fastControl)

	waitFor(t, lr.rec, pipeline.EventStepRunning)
	require.NoError(t, pipeline.RequestStop(t.Context(), lr.path, pipeline.StopGraceful, "alice"))
	require.NoError(t, pipeline.RequestStop(t.Context(), lr.path, pipeline.StopCancel, "alice"))

	out := lr.wait(t)
	require.ErrorIs(t, out.err, context.Canceled)
	assert.Equal(t, pipeline.OutcomeCancelled, out.res.Outcome)

	state, err := pipeline.Inspect(lr.path)
	require.NoError(t, err)
	assert.Equal(t, pipeline.StopCancel, state.Attempts[0].Stop.Mode, "the cancel outranks the stop")
	assert.Len(t, kinds(lr.rec.all(), pipeline.EventStopRequested), 2, "both requests are recorded")
}

// A run already asked to stop answers a request to stop the same way, even
// one its own Stopper made.
func TestRequestStop_AnsweredByAStopAlreadyMade(t *testing.T) {
	t.Parallel()

	stopper := pipeline.NewStopper()
	running, release := make(chan struct{}), make(chan struct{})
	lr := startRun(t, mustPlan(t,
		pipeline.Effect("a", blocking(running, release)),
		pipeline.Effect("b", ok()),
	), pipeline.WithStopper(stopper), pipeline.WithControlPoll(time.Hour))

	<-running
	stopper.Stop("SIGTERM")
	waitFor(t, lr.rec, pipeline.EventStopRequested)

	// The run won't look at its control directory again for an hour.
	require.NoError(t, pipeline.RequestStop(t.Context(), lr.path, pipeline.StopGraceful, "alice"))

	close(release)
	assert.Equal(t, pipeline.ErrStopped, lr.wait(t).err)
}

func TestRequestStop_ErrNotRunning(t *testing.T) {
	t.Parallel()

	plan := mustPlan(t, pipeline.Effect("a", ok()))

	t.Run("no journal", func(t *testing.T) {
		t.Parallel()

		err := pipeline.RequestStop(t.Context(), journalPath(t), pipeline.StopGraceful, "")
		require.ErrorIs(t, err, pipeline.ErrNotRunning)
	})

	t.Run("the run finished", func(t *testing.T) {
		t.Parallel()

		path := journalPath(t)
		runToFile(t, path, plan)

		err := pipeline.RequestStop(t.Context(), path, pipeline.StopGraceful, "")
		require.ErrorIs(t, err, pipeline.ErrNotRunning)
	})

	t.Run("the run finished and the journal is still open", func(t *testing.T) {
		t.Parallel()

		path := journalPath(t)

		j, err := pipeline.OpenJournal(path)
		require.NoError(t, err)

		defer func() { _ = j.Close() }()

		_, err = pipeline.Run(t.Context(), plan, j)
		require.NoError(t, err)

		err = pipeline.RequestStop(t.Context(), path, pipeline.StopGraceful, "")
		require.ErrorIs(t, err, pipeline.ErrNotRunning)
	})

	t.Run("the runner is gone", func(t *testing.T) {
		t.Parallel()

		path := journalPath(t)

		j, err := pipeline.OpenJournal(path, pipeline.WithWriteHook(func(e pipeline.Event) error {
			if e.Kind == pipeline.EventStepFinished {
				return errors.New("crashed")
			}

			return nil
		}))
		require.NoError(t, err)

		_, err = pipeline.Run(t.Context(), plan, j)
		require.Error(t, err)
		require.NoError(t, j.Close())

		err = pipeline.RequestStop(t.Context(), path, pipeline.StopGraceful, "")
		require.ErrorIs(t, err, pipeline.ErrNotRunning)
	})
}

// A request the run never reads leaves RequestStop waiting until its
// context is done.
func TestRequestStop_WaitsUntilTheContextIsDone(t *testing.T) {
	t.Parallel()

	running, release := make(chan struct{}), make(chan struct{})
	lr := startUnreadRun(t, mustPlan(t, pipeline.Effect("a", blocking(running, release))))

	<-running

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	err := pipeline.RequestStop(ctx, lr.path, pipeline.StopGraceful, "alice")
	require.ErrorIs(t, err, context.DeadlineExceeded)

	close(release)
	require.NoError(t, lr.wait(t).err)
}

func TestRequestStop_RefusesAnUnknownMode(t *testing.T) {
	t.Parallel()

	err := pipeline.RequestStop(t.Context(), journalPath(t), "pause", "")
	require.ErrorContains(t, err, `unknown stop mode "pause"`)
}

// Requests left behind for an earlier run, or that aren't requests at all,
// are deleted without being acted on, and a control directory anyone could
// write to is made private again.
func TestRun_IgnoresRequestsThatAreNotForIt(t *testing.T) {
	t.Parallel()

	path := journalPath(t)
	dir := path + ".control"

	require.NoError(t, os.Mkdir(dir, 0o700))
	require.NoError(t, os.Chmod(dir, 0o777)) //nolint:gosec // The looseness under test.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stop.r-earlier"), []byte(`{"who":"alice"}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cancel.r-earlier"), []byte(`{"who":"alice"}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stop"), []byte(`not a request`), 0o600))

	j, err := pipeline.OpenJournal(path)
	require.NoError(t, err)

	defer func() { _ = j.Close() }()

	// a lets the run take the requests, and a few more looks besides, before
	// b is due to start.
	taken := pipeline.StepFunc(func(context.Context, *pipeline.StepContext) error {
		for {
			if entries, err := os.ReadDir(dir); err == nil && len(entries) == 0 {
				time.Sleep(50 * time.Millisecond)

				return nil
			}

			time.Sleep(time.Millisecond)
		}
	})

	var c calls

	res, err := pipeline.Run(t.Context(), mustPlan(t,
		pipeline.Effect("a", taken),
		pipeline.Effect("b", counted(&c, "b", nil)),
	), j, fastControl)
	require.NoError(t, err)
	assert.Equal(t, pipeline.OutcomePassed, res.Outcome)
	assert.Equal(t, 1, c.get("b"))

	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestRun_RefusesAControlPathThatIsNotADirectory(t *testing.T) {
	t.Parallel()

	path := journalPath(t)
	require.NoError(t, os.WriteFile(path+".control", nil, 0o600))

	j, err := pipeline.OpenJournal(path)
	require.NoError(t, err)

	defer func() { _ = j.Close() }()

	_, err = pipeline.Run(t.Context(), mustPlan(t, pipeline.Effect("a", ok())), j)
	require.ErrorContains(t, err, "is not a directory")

	events, err := j.Events()
	require.NoError(t, err)
	assert.Empty(t, events, "the run did not start")
}

// waitFor waits until rec has seen an event of the kind.
func waitFor(t *testing.T, rec *recorder, kind pipeline.EventKind) {
	t.Helper()

	require.Eventually(t, func() bool {
		return len(kinds(rec.all(), kind)) > 0
	}, 10*time.Second, time.Millisecond)
}

// A request too big for the run to read is refused, not left to be thrown
// away while the caller waits.
func TestRequestStop_RefusesARequestTooBigToRead(t *testing.T) {
	t.Parallel()

	running, release := make(chan struct{}), make(chan struct{})
	lr := startRun(t, mustPlan(t, pipeline.Effect("a", blocking(running, release))), fastControl)

	<-running

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	err := pipeline.RequestStop(ctx, lr.path, pipeline.StopGraceful, strings.Repeat("x", 5000))
	require.ErrorContains(t, err, "too long")

	close(release)
	require.NoError(t, lr.wait(t).err)
}

func TestApprove_ReleasesAWaitingGate(t *testing.T) {
	t.Parallel()

	lr := startRun(t, mustPlan(t,
		pipeline.Gate("go", pipeline.GateOptions{Prompt: "Go on?"}),
		pipeline.Effect("after", ok()),
	), fastControl)

	waitFor(t, lr.rec, pipeline.EventGateWaiting)

	state, err := pipeline.Inspect(lr.path)
	require.NoError(t, err)
	assert.True(t, state.Element("p/go").Waiting)

	require.NoError(t, pipeline.Approve(t.Context(), lr.path, "p/go", "alice"))
	require.NoError(t, lr.wait(t).err)

	released := kinds(lr.rec.all(), pipeline.EventGateReleased)
	require.Len(t, released, 1)
	assert.Equal(t, "control", released[0].By)
	assert.Equal(t, "alice", released[0].Who)
	assert.Equal(t, uid(), released[0].UID)

	entries, err := os.ReadDir(lr.path + ".control")
	require.NoError(t, err)
	assert.Empty(t, entries, "the run took the approval")
}

func TestApprove_ErrGateNotWaiting(t *testing.T) {
	t.Parallel()

	running, release := make(chan struct{}), make(chan struct{})
	lr := startRun(t, mustPlan(t,
		pipeline.Effect("before", blocking(running, release)),
		pipeline.Gate("go", pipeline.GateOptions{Approvers: []pipeline.Approver{approves("test", "bob")}}),
	), fastControl)

	<-running

	for _, gate := range []string{"p/go", "p/before", "p/nothing"} {
		err := pipeline.Approve(t.Context(), lr.path, gate, "alice")
		require.ErrorIs(t, err, pipeline.ErrGateNotWaiting, gate)
	}

	close(release)
	require.NoError(t, lr.wait(t).err)
}

func TestApprove_ErrNotRunning(t *testing.T) {
	t.Parallel()

	err := pipeline.Approve(t.Context(), journalPath(t), "p/go", "alice")
	require.ErrorIs(t, err, pipeline.ErrNotRunning)

	path := journalPath(t)
	runToFile(t, path, mustPlan(t, pipeline.Gate("go", pipeline.GateOptions{
		Approvers: []pipeline.Approver{approves("test", "bob")},
	})))

	err = pipeline.Approve(t.Context(), path, "p/go", "alice")
	require.ErrorIs(t, err, pipeline.ErrNotRunning)
}

// A stop ends a gate's wait without releasing it, so an approval waiting to
// be read is not answered: the gate is no longer waiting.
func TestApprove_AGateAStopInterrupted(t *testing.T) {
	t.Parallel()

	stopper := pipeline.NewStopper()
	lr := startUnreadRun(t, mustPlan(t, pipeline.Gate("go", pipeline.GateOptions{})), pipeline.WithStopper(stopper))

	waitFor(t, lr.rec, pipeline.EventGateWaiting)

	done := make(chan error, 1)

	go func() { done <- pipeline.Approve(t.Context(), lr.path, "p/go", "alice") }()

	// The run won't read the approval for an hour; a stop ends the wait
	// first, and the run.
	require.Eventually(t, func() bool {
		entries, err := os.ReadDir(lr.path + ".control")

		return err == nil && len(entries) == 1
	}, 10*time.Second, time.Millisecond, "the approval was written")

	stopper.Stop("bob")
	assert.Equal(t, pipeline.ErrStopped, lr.wait(t).err)
	require.ErrorIs(t, <-done, pipeline.ErrGateNotWaiting)
}
