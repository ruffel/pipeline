//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/ruffel/pipeline/internal/journalhook"
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

	events, _, err := ReadEvents(path)
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

	j, err := OpenJournal(path)
	require.NoError(t, err)

	journalhook.Set(j, func(Event) error { return os.ErrPermission })

	_, err = j.append(Event{Kind: EventRunStarted})
	require.ErrorIs(t, err, os.ErrPermission)
	require.NoError(t, j.Close())
}

func TestOpenJournal_MissingDirectory(t *testing.T) {
	t.Parallel()

	_, err := OpenJournal(filepath.Join(t.TempDir(), "no-such-dir", "test.journal"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

// Only a lock someone else holds means busy. Any other locking failure is
// reported as what it is.
func TestOpenJournal_OnlyAHeldLockIsBusy(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "test.journal")

	held, err := OpenJournal(path)
	require.NoError(t, err)

	f, err := openJournalFile(path)
	require.NoError(t, err)
	require.ErrorIs(t, lockJournal(f), syscall.EWOULDBLOCK, "held by another open file")
	require.NoError(t, f.Close())
	require.NoError(t, held.Close())

	// flock on a closed file fails with EBADF, at once and not as "busy".
	closed, err := openJournalFile(path)
	require.NoError(t, err)
	require.NoError(t, closed.Close())

	err = lockJournal(closed)
	require.Error(t, err)
	require.NotErrorIs(t, err, syscall.EWOULDBLOCK)
}

// The lock holder syncs the journal's directory even when it did not create
// the file: opening an existing journal must still work, and does the sync.
func TestOpenJournal_ExistingFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "test.journal")
	require.NoError(t, os.WriteFile(path, nil, journalFileMode))

	j, err := OpenJournal(path)
	require.NoError(t, err)

	fs, ok := j.store.(*fileStore)
	require.True(t, ok)
	assert.True(t, filepath.IsAbs(fs.path))
	require.NoError(t, j.Close())
}

// applies is a step that counts how often its work is applied, and checks
// first whether it has been.
type applies struct{ applied *int }

func (a applies) Check(context.Context, *StepContext) (bool, error) { return *a.applied > 0, nil }

func (a applies) Run(context.Context, *StepContext) error {
	*a.applied++

	return nil
}

// The journal fails just as a step's outcome is stored: the work is done,
// and the journal can't be relied on to say so. The run ends there, and
// nothing after it is called. A new process opens the journal again and
// resumes: the work is not done a second time, and the rest runs once.
func TestRun_AJournalFailureAtAStepsOutcomeIsResumedAfterAReopen(t *testing.T) {
	t.Parallel()

	errDisk := errors.New("input/output error")

	for name, syncFails := range map[string]bool{
		// The line reaches the file and the sync fails: it may or may not
		// be on disk.
		"the sync fails": true,
		// The line never reaches the file.
		"the write fails": false,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			applied, after := 0, 0
			plan, err := NewPlan("p",
				Effect("apply", applies{&applied}),
				Effect("after", StepFunc(func(context.Context, *StepContext) error {
					after++

					return nil
				})),
			)
			require.NoError(t, err)

			j, fs, path := openTemp(t)

			j.hook = func(e Event) error {
				switch {
				case e.Kind != EventStepFinished || e.Path != "p/apply":
					return nil
				case syncFails:
					fs.sync = func() error { return errDisk }

					return nil
				default:
					return errDisk
				}
			}

			res, err := Run(t.Context(), plan, j)
			require.ErrorIs(t, err, errDisk)
			assert.Equal(t, OutcomeFailed, res.Outcome)
			assert.Equal(t, 1, applied)
			assert.Zero(t, after, "nothing is called that the journal could not record")
			require.NoError(t, j.Close())

			j, err = OpenJournal(path)
			require.NoError(t, err)

			t.Cleanup(func() { _ = j.Close() })

			state, err := j.State()
			require.NoError(t, err)
			assert.True(t, state.Element("p/apply").RunCalled)
			assert.Equal(t, StatusNotReached, state.Element("p/after").Status)

			_, err = Run(t.Context(), plan, j)
			require.NoError(t, err)
			assert.Equal(t, 1, applied, "applied once")
			assert.Equal(t, 1, after)
		})
	}
}
