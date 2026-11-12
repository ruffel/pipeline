//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package runbook_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ruffel/pipeline/runbook"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fastControl makes a run read its control directory often, so tests don't
// wait on the default poll.
var fastControl = runbook.WithControlPoll(5 * time.Millisecond) //nolint:gochecknoglobals // A fixed test option.

// liveRun is a run on a file journal, going on in the background.
type liveRun struct {
	path string
	j    *runbook.Journal
	rec  *recorder
	done chan liveResult
}

type liveResult struct {
	res *runbook.Result
	err error
}

// startRun starts plan on a new file journal, and closes the journal when
// the run returns.
func startRun(t *testing.T, plan *runbook.Plan, opts ...runbook.Option) *liveRun {
	t.Helper()

	path := journalPath(t)

	j, err := runbook.OpenJournal(path)
	require.NoError(t, err)

	lr := &liveRun{path: path, j: j, rec: &recorder{}, done: make(chan liveResult, 1)}
	opts = append([]runbook.Option{runbook.WithObservers(lr.rec)}, opts...)

	go func() {
		res, err := runbook.Run(t.Context(), plan, j, opts...)
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
func blocking(running chan<- struct{}, release <-chan struct{}) runbook.Step {
	return runbook.StepFunc(func(context.Context, *runbook.StepContext) error {
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
		runbook.Effect("a", blocking(running, release)),
		runbook.Effect("b", counted(&c, "b", nil)),
	), fastControl)

	<-running
	require.NoError(t, runbook.RequestStop(t.Context(), lr.path, runbook.StopGraceful, "alice"))

	// RequestStop returns once the request is in the journal, while the step
	// still runs.
	state, err := runbook.Inspect(lr.path)
	require.NoError(t, err)
	assert.Equal(t, &runbook.StopRequest{
		Mode: runbook.StopGraceful, By: "control", Who: "alice", UID: uid(),
	}, state.Attempts[0].Stop)
	assert.Equal(t, runbook.StatusStarted, state.Element("p/a").Status)

	close(release)

	out := lr.wait(t)
	assert.Equal(t, runbook.ErrStopped, out.err)
	assert.Equal(t, runbook.OutcomeStopped, out.res.Outcome)
	assert.Zero(t, c.get("b"), "nothing starts after the stop")

	entries, err := os.ReadDir(lr.path + ".control")
	require.NoError(t, err)
	assert.Empty(t, entries, "the run took the request")
}

func TestRequestStop_CancelsARun(t *testing.T) {
	t.Parallel()

	lr := startRun(t, mustPlan(t,
		runbook.Effect("a", waitForCancel()),
		runbook.Effect("b", ok()),
	), fastControl)

	waitFor(t, lr.rec, runbook.EventStepRunning)
	require.NoError(t, runbook.RequestStop(t.Context(), lr.path, runbook.StopCancel, "bob"))

	out := lr.wait(t)
	require.ErrorIs(t, out.err, context.Canceled)
	assert.Equal(t, runbook.OutcomeCancelled, out.res.Outcome)

	state, err := runbook.Inspect(lr.path)
	require.NoError(t, err)
	assert.Equal(t, &runbook.StopRequest{
		Mode: runbook.StopCancel, By: "control", Who: "bob", UID: uid(),
	}, state.Attempts[0].Stop)
	assert.Equal(t, runbook.ErrorCancelled, state.Element("p/a").Error.Kind)
	assert.Equal(t, runbook.StatusNotReached, state.Element("p/b").Status)
}

// A step that won't finish on its own is cancelled after a graceful stop,
// as an operator's second Ctrl-C would.
func TestRequestStop_ACancelAfterAGracefulStop(t *testing.T) {
	t.Parallel()

	lr := startRun(t, mustPlan(t, runbook.Effect("a", waitForCancel())), fastControl)

	waitFor(t, lr.rec, runbook.EventStepRunning)
	require.NoError(t, runbook.RequestStop(t.Context(), lr.path, runbook.StopGraceful, "alice"))
	require.NoError(t, runbook.RequestStop(t.Context(), lr.path, runbook.StopCancel, "alice"))

	out := lr.wait(t)
	require.ErrorIs(t, out.err, context.Canceled)
	assert.Equal(t, runbook.OutcomeCancelled, out.res.Outcome)

	state, err := runbook.Inspect(lr.path)
	require.NoError(t, err)
	assert.Equal(t, runbook.StopCancel, state.Attempts[0].Stop.Mode, "the cancel outranks the stop")
	assert.Len(t, kinds(lr.rec.all(), runbook.EventStopRequested), 2, "both requests are recorded")
}

// A run already asked to stop answers a request to stop the same way, even
// one its own Stopper made.
func TestRequestStop_AnsweredByAStopAlreadyMade(t *testing.T) {
	t.Parallel()

	stopper := runbook.NewStopper()
	running, release := make(chan struct{}), make(chan struct{})
	lr := startRun(t, mustPlan(t,
		runbook.Effect("a", blocking(running, release)),
		runbook.Effect("b", ok()),
	), runbook.WithStopper(stopper), runbook.WithControlPoll(time.Hour))

	<-running
	stopper.Stop("SIGTERM")
	waitFor(t, lr.rec, runbook.EventStopRequested)

	// The run won't look at its control directory again for an hour.
	require.NoError(t, runbook.RequestStop(t.Context(), lr.path, runbook.StopGraceful, "alice"))

	close(release)
	assert.Equal(t, runbook.ErrStopped, lr.wait(t).err)
}

func TestRequestStop_ErrNotRunning(t *testing.T) {
	t.Parallel()

	plan := mustPlan(t, runbook.Effect("a", ok()))

	t.Run("no journal", func(t *testing.T) {
		t.Parallel()

		err := runbook.RequestStop(t.Context(), journalPath(t), runbook.StopGraceful, "")
		require.ErrorIs(t, err, runbook.ErrNotRunning)
	})

	t.Run("the run finished", func(t *testing.T) {
		t.Parallel()

		path := journalPath(t)
		runToFile(t, path, plan)

		err := runbook.RequestStop(t.Context(), path, runbook.StopGraceful, "")
		require.ErrorIs(t, err, runbook.ErrNotRunning)
	})

	t.Run("the run finished and the journal is still open", func(t *testing.T) {
		t.Parallel()

		path := journalPath(t)

		j, err := runbook.OpenJournal(path)
		require.NoError(t, err)

		defer func() { _ = j.Close() }()

		_, err = runbook.Run(t.Context(), plan, j)
		require.NoError(t, err)

		err = runbook.RequestStop(t.Context(), path, runbook.StopGraceful, "")
		require.ErrorIs(t, err, runbook.ErrNotRunning)
	})

	t.Run("the runner is gone", func(t *testing.T) {
		t.Parallel()

		path := journalPath(t)

		j, err := runbook.OpenJournal(path, runbook.WithWriteHook(func(e runbook.Event) error {
			if e.Kind == runbook.EventStepFinished {
				return errors.New("crashed")
			}

			return nil
		}))
		require.NoError(t, err)

		_, err = runbook.Run(t.Context(), plan, j)
		require.Error(t, err)
		require.NoError(t, j.Close())

		err = runbook.RequestStop(t.Context(), path, runbook.StopGraceful, "")
		require.ErrorIs(t, err, runbook.ErrNotRunning)
	})
}

// A request the run never reads leaves RequestStop waiting until its
// context is done.
func TestRequestStop_WaitsUntilTheContextIsDone(t *testing.T) {
	t.Parallel()

	running, release := make(chan struct{}), make(chan struct{})
	lr := startRun(t, mustPlan(t, runbook.Effect("a", blocking(running, release))),
		runbook.WithControlPoll(time.Hour))

	<-running

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	err := runbook.RequestStop(ctx, lr.path, runbook.StopGraceful, "alice")
	require.ErrorIs(t, err, context.DeadlineExceeded)

	close(release)
	require.NoError(t, lr.wait(t).err)
}

func TestRequestStop_RefusesAnUnknownMode(t *testing.T) {
	t.Parallel()

	err := runbook.RequestStop(t.Context(), journalPath(t), "pause", "")
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

	j, err := runbook.OpenJournal(path)
	require.NoError(t, err)

	defer func() { _ = j.Close() }()

	// a lets the run take the requests, and a few more looks besides, before
	// b is due to start.
	taken := runbook.StepFunc(func(context.Context, *runbook.StepContext) error {
		for {
			if entries, err := os.ReadDir(dir); err == nil && len(entries) == 0 {
				time.Sleep(50 * time.Millisecond)

				return nil
			}

			time.Sleep(time.Millisecond)
		}
	})

	var c calls

	res, err := runbook.Run(t.Context(), mustPlan(t,
		runbook.Effect("a", taken),
		runbook.Effect("b", counted(&c, "b", nil)),
	), j, fastControl)
	require.NoError(t, err)
	assert.Equal(t, runbook.OutcomePassed, res.Outcome)
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

	j, err := runbook.OpenJournal(path)
	require.NoError(t, err)

	defer func() { _ = j.Close() }()

	_, err = runbook.Run(t.Context(), mustPlan(t, runbook.Effect("a", ok())), j)
	require.ErrorContains(t, err, "is not a directory")

	events, err := j.Events()
	require.NoError(t, err)
	assert.Empty(t, events, "the run did not start")
}

// waitFor waits until rec has seen an event of the kind.
func waitFor(t *testing.T, rec *recorder, kind runbook.EventKind) {
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
	lr := startRun(t, mustPlan(t, runbook.Effect("a", blocking(running, release))), fastControl)

	<-running

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	err := runbook.RequestStop(ctx, lr.path, runbook.StopGraceful, strings.Repeat("x", 5000))
	require.ErrorContains(t, err, "too long")

	close(release)
	require.NoError(t, lr.wait(t).err)
}

func TestApprove_ReleasesAWaitingGate(t *testing.T) {
	t.Parallel()

	lr := startRun(t, mustPlan(t,
		runbook.Gate("go", runbook.GateOptions{Prompt: "Go on?"}),
		runbook.Effect("after", ok()),
	), fastControl)

	waitFor(t, lr.rec, runbook.EventGateWaiting)

	state, err := runbook.Inspect(lr.path)
	require.NoError(t, err)
	assert.True(t, state.Element("p/go").Waiting)

	require.NoError(t, runbook.Approve(t.Context(), lr.path, "p/go", "alice"))
	require.NoError(t, lr.wait(t).err)

	released := kinds(lr.rec.all(), runbook.EventGateReleased)
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
		runbook.Effect("before", blocking(running, release)),
		runbook.Gate("go", runbook.GateOptions{Approvers: []runbook.Approver{approves("test", "bob")}}),
	), fastControl)

	<-running

	for _, gate := range []string{"p/go", "p/before", "p/nothing"} {
		err := runbook.Approve(t.Context(), lr.path, gate, "alice")
		require.ErrorIs(t, err, runbook.ErrGateNotWaiting, gate)
	}

	close(release)
	require.NoError(t, lr.wait(t).err)
}

func TestApprove_ErrNotRunning(t *testing.T) {
	t.Parallel()

	err := runbook.Approve(t.Context(), journalPath(t), "p/go", "alice")
	require.ErrorIs(t, err, runbook.ErrNotRunning)

	path := journalPath(t)
	runToFile(t, path, mustPlan(t, runbook.Gate("go", runbook.GateOptions{
		Approvers: []runbook.Approver{approves("test", "bob")},
	})))

	err = runbook.Approve(t.Context(), path, "p/go", "alice")
	require.ErrorIs(t, err, runbook.ErrNotRunning)
}

// A stop ends a gate's wait without releasing it, so an approval waiting to
// be read is not answered: the gate is no longer waiting.
func TestApprove_AGateAStopInterrupted(t *testing.T) {
	t.Parallel()

	stopper := runbook.NewStopper()
	lr := startRun(t, mustPlan(t, runbook.Gate("go", runbook.GateOptions{})),
		runbook.WithControlPoll(time.Hour), runbook.WithStopper(stopper))

	waitFor(t, lr.rec, runbook.EventGateWaiting)

	done := make(chan error, 1)

	go func() { done <- runbook.Approve(t.Context(), lr.path, "p/go", "alice") }()

	// The run won't read the approval for an hour; a stop ends the wait
	// first, and the run.
	require.Eventually(t, func() bool {
		entries, err := os.ReadDir(lr.path + ".control")

		return err == nil && len(entries) == 1
	}, 10*time.Second, time.Millisecond, "the approval was written")

	stopper.Stop("bob")
	assert.Equal(t, runbook.ErrStopped, lr.wait(t).err)
	require.ErrorIs(t, <-done, runbook.ErrGateNotWaiting)
}
