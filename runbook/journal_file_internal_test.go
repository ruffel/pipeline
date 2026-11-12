//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package runbook

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func openTemp(t *testing.T) (*Journal, *fileStore, string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "test.journal")

	j, err := OpenJournal(path)
	require.NoError(t, err)

	fs, ok := j.store.(*fileStore)
	require.True(t, ok)

	return j, fs, path
}

func TestFileJournal_SequenceContinuesAfterReopening(t *testing.T) {
	t.Parallel()

	j, _, path := openTemp(t)

	for range 2 {
		_, err := j.append(Event{Kind: EventStepStarted, Path: "p/a"})
		require.NoError(t, err)
	}

	require.NoError(t, j.Close())

	j, err := OpenJournal(path)
	require.NoError(t, err)

	e, err := j.append(Event{Kind: EventStepFinished, Path: "p/a"})
	require.NoError(t, err)
	assert.Equal(t, int64(3), e.Seq)
	require.NoError(t, j.Close())
}

func TestFileJournal_SyncsLifecycleEventsOnly(t *testing.T) {
	t.Parallel()

	j, fs, _ := openTemp(t)

	syncs := 0
	realSync := fs.sync
	fs.sync = func() error {
		syncs++

		return realSync()
	}

	want := 0

	for _, k := range []EventKind{EventRunStarted, EventOutput, EventProgress, EventValue, EventMessage, EventStepFinished} {
		_, err := j.append(Event{Kind: k})
		require.NoError(t, err)

		if k.Durable() {
			want++
		}

		assert.Equal(t, want, syncs, "after %s", k)
	}

	require.NoError(t, j.Close())
}

func TestFileJournal_StaysBrokenAfterAFailedWrite(t *testing.T) {
	t.Parallel()

	j, fs, path := openTemp(t)

	_, err := j.append(Event{Kind: EventRunStarted})
	require.NoError(t, err)

	// A sync failure after the line was written: the event may or may not be
	// durable, so nothing more may follow it.
	fs.sync = func() error { return os.ErrDeadlineExceeded }

	_, err = j.append(Event{Kind: EventStepStarted, Path: "p/a"})
	require.ErrorIs(t, err, os.ErrDeadlineExceeded)

	fs.sync = fs.f.Sync

	_, err = j.append(Event{Kind: EventOutput, Line: "must not be written"})
	require.ErrorIs(t, err, os.ErrDeadlineExceeded, "the journal stays broken")
	require.NoError(t, j.Close())

	events, err := readJournal(path)
	require.NoError(t, err)
	assert.Len(t, events, 2)

	// Opening it again makes it usable, and numbering carries on.
	j, err = OpenJournal(path)
	require.NoError(t, err)

	e, err := j.append(Event{Kind: EventStepFinished, Path: "p/a"})
	require.NoError(t, err)
	assert.Equal(t, int64(3), e.Seq)
	require.NoError(t, j.Close())
}

func TestOpenJournal_WriteHookApplies(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "test.journal")

	j, err := OpenJournal(path, WithWriteHook(func(Event) error { return os.ErrPermission }))
	require.NoError(t, err)

	_, err = j.append(Event{Kind: EventRunStarted})
	require.ErrorIs(t, err, os.ErrPermission)
	require.NoError(t, j.Close())
}

func TestOpenJournal_MissingDirectory(t *testing.T) {
	t.Parallel()

	_, err := OpenJournal(filepath.Join(t.TempDir(), "no-such-dir", "test.journal"))
	require.ErrorIs(t, err, os.ErrNotExist)
}
