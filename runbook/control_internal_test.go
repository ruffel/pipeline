package runbook

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

	name, err := controlFile(mode)
	require.NoError(t, err)

	return &stopRequester{
		reader: journalReader{path: path},
		dir:    controlDir(path),
		name:   name,
		mode:   mode,
		who:    "alice",
		locked: func(string) bool { return true },
	}
}

// requestFor returns the run the request in place is for.
func requestFor(t *testing.T, q *stopRequester) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(q.dir, q.name))
	require.NoError(t, err)

	var req controlRequest
	require.NoError(t, json.Unmarshal(data, &req))
	assert.Equal(t, "alice", req.Who)

	return req.Run
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
	assert.Equal(t, "r1", requestFor(t, q))

	journalLines(t, q.reader.path, &seq,
		Event{Kind: EventOutput, Run: "r1", Line: "still going"},
		Event{Kind: EventRunFinished, Run: "r1", Outcome: OutcomeFailed},
		Event{Kind: EventRunStarted, Run: "r2"},
	)

	done, err = q.step()
	require.NoError(t, err)
	require.False(t, done)
	assert.Equal(t, "r2", requestFor(t, q))

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
	assert.Equal(t, "r2", requestFor(t, q))

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

	require.NoError(t, os.WriteFile(filepath.Join(dir, "stop"), []byte(`{"run":"r1","who":"alice"}`), 0o600))

	req, ok := takeRequest(dir, "stop")
	require.True(t, ok)
	assert.Equal(t, "r1", req.Run)
	assert.Equal(t, "alice", req.Who)

	_, ok = takeRequest(dir, "stop")
	assert.False(t, ok, "a request is taken once")

	// Too big to be a request.
	big := make([]byte, maxControlRequest+1)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stop"), big, 0o600))

	_, ok = takeRequest(dir, "stop")
	assert.False(t, ok)

	// Not a file.
	require.NoError(t, os.Mkdir(filepath.Join(dir, "cancel"), 0o700))

	_, ok = takeRequest(dir, "cancel")
	assert.False(t, ok)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries)
}
