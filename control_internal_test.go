package pipeline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// journalLines writes events to the end of the journal file at path,
// numbering them on from seq.
func journalLines(t *testing.T, path string, seq *int64, events ...Event) {
	t.Helper()

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600) //nolint:gosec // The test's own file.
	require.NoError(t, err)

	defer func() { require.NoError(t, f.Close()) }()

	for _, e := range events {
		*seq++
		e.V, e.Seq = SchemaVersion, *seq

		line, err := json.Marshal(e)
		require.NoError(t, err)

		_, err = f.Write(append(line, '\n'))
		require.NoError(t, err)
	}
}

func newRequester(t *testing.T, mode StopMode) *stopRequester {
	t.Helper()

	path := filepath.Join(t.TempDir(), "test.journal")
	require.NoError(t, os.Mkdir(controlDir(path), controlDirMode))

	kind, err := controlFile(mode)
	require.NoError(t, err)

	return &stopRequester{
		reader: journalReader{path: path},
		dir:    controlDir(path),
		kind:   kind,
		mode:   mode,
		data:   []byte(`{"who":"alice"}`),
		locked: func(string) bool { return true },
	}
}

// requestFor returns the runs the requests in place are for, and takes
// them.
func requestFor(t *testing.T, q *stopRequester) []string {
	t.Helper()

	entries, err := os.ReadDir(q.dir)
	require.NoError(t, err)

	runs := make([]string, 0, len(entries))

	for _, e := range entries {
		req, ok := takeRequest(q.dir, e.Name())
		require.True(t, ok, e.Name())
		assert.Equal(t, q.kind, req.kind)
		assert.Equal(t, "alice", req.Who)

		runs = append(runs, req.run)
	}

	return runs
}

// A runner that finishes and starts again (a re-exec) before it sees the
// request is asked again, for its new attempt.
func TestStopRequester_AsksANewAttemptAgain(t *testing.T) {
	t.Parallel()

	q := newRequester(t, StopGraceful)

	var seq int64

	journalLines(t, q.reader.path, &seq, Event{Kind: EventRunStarted, Run: "r1"})

	done, err := q.step()
	require.NoError(t, err)
	require.False(t, done)
	assert.Equal(t, []string{"r1"}, requestFor(t, q))

	journalLines(t, q.reader.path, &seq,
		Event{Kind: EventOutput, Run: "r1", Line: "still going"},
		Event{Kind: EventRunFinished, Run: "r1", Outcome: OutcomeFailed},
		Event{Kind: EventRunStarted, Run: "r2"},
	)

	done, err = q.step()
	require.NoError(t, err)
	require.False(t, done)
	assert.Equal(t, []string{"r2"}, requestFor(t, q))

	journalLines(t, q.reader.path, &seq, Event{Kind: EventStopRequested, Run: "r2", Mode: StopGraceful})

	done, err = q.step()
	require.True(t, done)
	require.NoError(t, err)
}

// A stop recorded for an earlier attempt, or a different kind of stop, does
// not answer the request.
func TestStopRequester_OnlyItsOwnKindOfStopAnswers(t *testing.T) {
	t.Parallel()

	q := newRequester(t, StopCancel)

	var seq int64

	journalLines(t, q.reader.path, &seq,
		Event{Kind: EventRunStarted, Run: "r1"},
		Event{Kind: EventStopRequested, Run: "r1", Mode: StopCancel},
		Event{Kind: EventRunFinished, Run: "r1", Outcome: OutcomeCancelled},
		Event{Kind: EventRunStarted, Run: "r2"},
		Event{Kind: EventStopRequested, Run: "r2", Mode: StopGraceful},
	)

	done, err := q.step()
	require.NoError(t, err)
	require.False(t, done)
	assert.Equal(t, []string{"r2"}, requestFor(t, q))

	journalLines(t, q.reader.path, &seq, Event{Kind: EventStopRequested, Run: "r2", Mode: StopCancel})

	done, err = q.step()
	require.True(t, done)
	require.NoError(t, err)
}

// An attempt that finishes without recording the request ends the wait.
func TestStopRequester_TheRunFinishedFirst(t *testing.T) {
	t.Parallel()

	q := newRequester(t, StopGraceful)

	var seq int64

	journalLines(t, q.reader.path, &seq, Event{Kind: EventRunStarted, Run: "r1"})

	done, err := q.step()
	require.NoError(t, err)
	require.False(t, done)

	journalLines(t, q.reader.path, &seq, Event{Kind: EventRunFinished, Run: "r1", Outcome: OutcomePassed})

	done, err = q.step()
	require.True(t, done)
	require.ErrorIs(t, err, ErrNotRunning)
}

// A runner that recorded the request and let go of the journal between two
// looks has answered it: the request is read before giving up on a runner
// that is gone.
func TestStopRequester_AnAnswerJustBeforeTheRunnerLetGo(t *testing.T) {
	t.Parallel()

	q := newRequester(t, StopGraceful)

	var seq int64

	journalLines(t, q.reader.path, &seq, Event{Kind: EventRunStarted, Run: "r1"})

	done, err := q.step()
	require.NoError(t, err)
	require.False(t, done)

	// The runner records the request and lets go just as it is looked for.
	q.locked = func(string) bool {
		if seq == 1 {
			journalLines(t, q.reader.path, &seq, Event{Kind: EventStopRequested, Run: "r1", Mode: StopGraceful})
		}

		return false
	}

	done, err = q.step()
	require.True(t, done)
	require.NoError(t, err)
}

func TestTakeRequest_LeavesNothingBehind(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(dir, "stop.r1"), []byte(`{"who":"alice"}`), 0o600))

	req, ok := takeRequest(dir, "stop.r1")
	require.True(t, ok)
	assert.Equal(t, controlRequest{Who: "alice", kind: requestStop, run: "r1", uid: req.uid}, req)

	_, ok = takeRequest(dir, "stop.r1")
	assert.False(t, ok, "a request is taken once")

	// Too big to be a request.
	big := make([]byte, maxControlRequest+1)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stop.r1"), big, 0o600))

	_, ok = takeRequest(dir, "stop.r1")
	assert.False(t, ok)

	// Not a file.
	require.NoError(t, os.Mkdir(filepath.Join(dir, "cancel.r1"), 0o700))

	_, ok = takeRequest(dir, "cancel.r1")
	assert.False(t, ok)

	// Not a kind of request.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pause.r1"), []byte(`{}`), 0o600))

	_, ok = takeRequest(dir, "pause.r1")
	assert.False(t, ok)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

// A caller that read an earlier attempt and writes late must not displace
// a request for the attempt now running.
func TestStopRequester_AStaleCallerLeavesTheCurrentRequest(t *testing.T) {
	t.Parallel()

	current := newRequester(t, StopGraceful)

	var seq int64

	journalLines(t, current.reader.path, &seq,
		Event{Kind: EventRunStarted, Run: "r1"},
		Event{Kind: EventRunFinished, Run: "r1", Outcome: OutcomeFailed},
		Event{Kind: EventRunStarted, Run: "r2"},
	)

	done, err := current.step()
	require.NoError(t, err)
	require.False(t, done)

	stale := *current
	stale.latest = "r1"
	require.NoError(t, stale.write())

	reqs := takeRequests(current.dir, "r2")
	require.Len(t, reqs, 1, "the request for the running attempt is still there")
	assert.Equal(t, requestStop, reqs[0].kind)

	entries, err := os.ReadDir(current.dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "the stale request was taken too, and thrown away")
}

// A stop recorded by an attempt that is over answers nothing.
func TestStopRequester_AStopFromAnAttemptThatIsOver(t *testing.T) {
	t.Parallel()

	t.Run("finished", func(t *testing.T) {
		t.Parallel()

		q := newRequester(t, StopGraceful)

		var seq int64

		journalLines(t, q.reader.path, &seq,
			Event{Kind: EventRunStarted, Run: "r1"},
			Event{Kind: EventStopRequested, Run: "r1", Mode: StopGraceful},
			Event{Kind: EventRunFinished, Run: "r1", Outcome: OutcomeStopped},
		)

		done, err := q.step()
		require.True(t, done)
		require.ErrorIs(t, err, ErrNotRunning)
	})

	t.Run("its runner is gone", func(t *testing.T) {
		t.Parallel()

		q := newRequester(t, StopGraceful)
		q.locked = func(string) bool { return false }

		var seq int64

		journalLines(t, q.reader.path, &seq,
			Event{Kind: EventRunStarted, Run: "r1"},
			Event{Kind: EventStopRequested, Run: "r1", Mode: StopGraceful},
		)

		done, err := q.step()
		require.True(t, done)
		require.ErrorIs(t, err, ErrNotRunning)
	})
}

func TestTakeRequests_CancelsFirstAndOnlyForTheRun(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	for _, name := range []string{"stop.r2", "cancel.r2", "stop.r1", ".stop.r2-123"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(`{"who":"`+name+`"}`), 0o600))
	}

	reqs := takeRequests(dir, "r2")
	require.Len(t, reqs, 2)
	assert.Equal(t, "cancel.r2", reqs[0].Who)
	assert.Equal(t, "stop.r2", reqs[1].Who)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, ".stop.r2-123", entries[0].Name(), "a request still being written is left alone")
}

func TestRequestName_RefusesARunThatIsNotAName(t *testing.T) {
	t.Parallel()

	for _, run := range []string{"", "../r1", "a/b", "a\\b", "a\x00b", "r1.r2"} {
		_, err := requestName(requestStop, run, "")
		assert.Error(t, err, run)
	}
}

// A control directory another user made stays theirs whatever its mode, so
// the run refuses it.
func TestCheckControlDir_RefusesAnotherUsersDirectory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	info, err := os.Lstat(dir)
	require.NoError(t, err)

	uid := fileUID(info)
	if uid == nil {
		t.Skip("files have no owning user here")
	}

	require.NoError(t, checkControlDir(dir, info, *uid))
	require.ErrorContains(t, checkControlDir(dir, info, *uid+1), "belongs to user")
}

func newApprover(t *testing.T) *approveRequester {
	t.Helper()

	path := filepath.Join(t.TempDir(), "test.journal")
	require.NoError(t, os.Mkdir(controlDir(path), controlDirMode))

	return &approveRequester{
		reader: journalReader{path: path},
		dir:    controlDir(path),
		gate:   "p/go",
		data:   []byte(`{"who":"alice","gate":"p/go"}`),
		locked: func(string) bool { return true },
	}
}

// approvalsFor takes the approvals in place, and returns the runs they are
// for.
func approvalsFor(t *testing.T, q *approveRequester) []string {
	t.Helper()

	entries, err := os.ReadDir(q.dir)
	require.NoError(t, err)

	runs := make([]string, 0, len(entries))

	for _, e := range entries {
		req, ok := takeRequest(q.dir, e.Name())
		require.True(t, ok, e.Name())
		assert.Equal(t, requestApprove, req.kind)
		assert.Equal(t, "p/go", req.Gate)

		runs = append(runs, req.run)
	}

	return runs
}

// A re-exec before the gate is approved asks again, once the new attempt's
// gate is waiting.
func TestApproveRequester_AsksANewAttemptAgain(t *testing.T) {
	t.Parallel()

	q := newApprover(t)

	var seq int64

	journalLines(t, q.reader.path, &seq,
		Event{Kind: EventRunStarted, Run: "r1"},
		Event{Kind: EventGateWaiting, Run: "r1", Path: "p/go"},
	)

	done, err := q.step()
	require.NoError(t, err)
	require.False(t, done)
	assert.Equal(t, []string{"r1"}, approvalsFor(t, q))

	journalLines(t, q.reader.path, &seq,
		Event{Kind: EventStepFinished, Run: "r1", Path: "p/go", Outcome: OutcomeStopped},
		Event{Kind: EventRunFinished, Run: "r1", Outcome: OutcomeStopped},
		Event{Kind: EventRunStarted, Run: "r2"},
	)

	done, err = q.step()
	require.NoError(t, err)
	require.False(t, done, "the new attempt hasn't reached the gate yet")
	assert.Empty(t, approvalsFor(t, q))

	journalLines(t, q.reader.path, &seq, Event{Kind: EventGateWaiting, Run: "r2", Path: "p/go"})

	done, err = q.step()
	require.NoError(t, err)
	require.False(t, done)
	assert.Equal(t, []string{"r2"}, approvalsFor(t, q))

	journalLines(t, q.reader.path, &seq,
		Event{Kind: EventGateReleased, Run: "r2", Path: "p/go", By: "control", Who: "alice"},
	)

	done, err = q.step()
	require.True(t, done)
	require.NoError(t, err)
}

// A gate released by its timeout was never approved.
func TestApproveRequester_TheGateTimedOutFirst(t *testing.T) {
	t.Parallel()

	q := newApprover(t)

	var seq int64

	journalLines(t, q.reader.path, &seq,
		Event{Kind: EventRunStarted, Run: "r1"},
		Event{Kind: EventGateWaiting, Run: "r1", Path: "p/go"},
	)

	done, err := q.step()
	require.NoError(t, err)
	require.False(t, done)

	journalLines(t, q.reader.path, &seq,
		Event{Kind: EventGateReleased, Run: "r1", Path: "p/go", By: "timeout", Policy: TimeoutContinue},
	)

	done, err = q.step()
	require.True(t, done)
	require.ErrorIs(t, err, ErrGateNotWaiting)
}

// Another approver releasing the gate answers the approval: the gate is
// approved.
func TestApproveRequester_AnotherApproverWasFirst(t *testing.T) {
	t.Parallel()

	q := newApprover(t)

	var seq int64

	journalLines(t, q.reader.path, &seq,
		Event{Kind: EventRunStarted, Run: "r1"},
		Event{Kind: EventGateWaiting, Run: "r1", Path: "p/go"},
	)

	done, err := q.step()
	require.NoError(t, err)
	require.False(t, done)

	journalLines(t, q.reader.path, &seq,
		Event{Kind: EventGateReleased, Run: "r1", Path: "p/go", By: "stdin", Who: "bob"},
		Event{Kind: EventRunFinished, Run: "r1", Outcome: OutcomePassed},
	)

	done, err = q.step()
	require.True(t, done)
	require.NoError(t, err)
}

func TestTakeRequest_AnApprovalMustNameItsGateTwice(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	key := gateKey("p/go")

	write := func(name, body string) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600))
	}

	write("approve.r1."+key, `{"who":"alice","gate":"p/go"}`)

	req, ok := takeRequest(dir, "approve.r1."+key)
	require.True(t, ok)
	assert.Equal(t, controlRequest{Who: "alice", Gate: "p/go", kind: requestApprove, run: "r1", uid: req.uid}, req)

	for name, body := range map[string]string{
		"approve.r1." + key:            `{"who":"alice","gate":"p/other"}`,
		"approve.r1." + gateKey("p/x"): `{"who":"alice","gate":"p/go"}`,
		"approve.r1":                   `{"who":"alice","gate":"p/go"}`,
	} {
		write(name, body)

		_, ok := takeRequest(dir, name)
		assert.False(t, ok, name)
	}
}

// A gate that finishes without being released, while its run goes on,
// answers the approval: the gate is no longer waiting.
func TestApproveRequester_TheGateFinishedUnreleased(t *testing.T) {
	t.Parallel()

	q := newApprover(t)

	var seq int64

	journalLines(t, q.reader.path, &seq,
		Event{Kind: EventRunStarted, Run: "r1"},
		Event{Kind: EventGateWaiting, Run: "r1", Path: "p/go"},
	)

	done, err := q.step()
	require.NoError(t, err)
	require.False(t, done)
	assert.Equal(t, []string{"r1"}, approvalsFor(t, q))

	journalLines(t, q.reader.path, &seq,
		Event{Kind: EventStopRequested, Run: "r1", Path: "p", Mode: StopGraceful},
		Event{Kind: EventStepFinished, Run: "r1", Path: "p/go", Outcome: OutcomeStopped},
	)

	done, err = q.step()
	require.True(t, done)
	require.ErrorIs(t, err, ErrGateNotWaiting)
}
