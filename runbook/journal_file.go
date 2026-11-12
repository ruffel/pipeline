//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package runbook

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// journalFileMode keeps journals private: they record what was done to a
// system, and its output.
const journalFileMode = 0o600

// How long OpenJournal keeps trying for the lock. [Inspect] holds it for an
// instant while it checks whether a writer is live, and must not make a
// writer fail to open.
const (
	lockAttempts = 10
	lockInterval = 20 * time.Millisecond
)

// OpenJournal opens the journal file at path for writing, creating it if it
// does not exist, and locks it: a journal has one writer at a time. A second
// OpenJournal, from this or any other process, fails with a
// [JournalBusyError], which matches [ErrJournalBusy]. The lock is released by
// Close, or by the kernel if the process dies.
//
// A crash can leave half a line at the end of the file. OpenJournal removes
// it, so the next event starts on a line of its own. A complete line that is
// not a valid event is [ErrJournalCorrupt].
//
// Lifecycle events are synced to disk before the write returns; output,
// progress and messages are not. If a write or sync fails, every later write
// fails too, until the journal is opened again.
func OpenJournal(path string, opts ...JournalOption) (*Journal, error) {
	f, created, err := openJournalFile(path)
	if err != nil {
		return nil, fmt.Errorf("runbook: open journal: %w", err)
	}

	if err := lockJournal(f); err != nil {
		_ = f.Close()

		return nil, &JournalBusyError{Path: path, PID: writerPID(path)}
	}

	if created {
		// The file's directory entry must survive a crash too.
		if err := syncDir(filepath.Dir(path)); err != nil {
			_ = f.Close()

			return nil, fmt.Errorf("runbook: open journal: %w", err)
		}
	}

	seq, err := repairJournal(path, f)
	if err != nil {
		_ = f.Close()

		return nil, err
	}

	j := &Journal{store: &fileStore{path: path, f: f, sync: f.Sync}, seq: seq}

	for _, o := range opts {
		o(j)
	}

	return j, nil
}

// openJournalFile opens path for appending, and reports whether it created
// the file.
func openJournalFile(path string) (*os.File, bool, error) {
	const flags = os.O_WRONLY | os.O_APPEND

	//nolint:gosec // The journal path is the caller's to choose.
	f, err := os.OpenFile(path, flags|os.O_CREATE|os.O_EXCL, journalFileMode)
	if err == nil {
		return f, true, nil
	}

	if !errors.Is(err, os.ErrExist) {
		return nil, false, err
	}

	f, err = os.OpenFile(path, flags, journalFileMode) //nolint:gosec // As above.

	return f, false, err
}

// lockJournal takes the exclusive lock, retrying briefly.
func lockJournal(f *os.File) error {
	var err error

	for range lockAttempts {
		if err = flock(f, syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return nil
		}

		time.Sleep(lockInterval)
	}

	return err
}

func flock(f *os.File, how int) error {
	for {
		err := syscall.Flock(int(f.Fd()), how)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

// repairJournal validates the file, removes a half-written last line, and
// returns the last sequence number. The caller holds the lock.
func repairJournal(path string, f *os.File) (int64, error) {
	var seq int64

	valid, err := scanJournal(path, func(line []byte) error {
		e, err := decodeEvent(line)
		if err != nil {
			return err
		}

		if e.Seq <= seq {
			return fmt.Errorf("sequence number %d follows %d", e.Seq, seq)
		}

		seq = e.Seq

		return nil
	})
	if err != nil {
		return 0, err
	}

	info, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("runbook: open journal: %w", err)
	}

	if info.Size() == valid {
		return seq, nil
	}

	if err := f.Truncate(valid); err != nil {
		return 0, fmt.Errorf("runbook: open journal: remove half-written line: %w", err)
	}

	if err := f.Sync(); err != nil {
		return 0, fmt.Errorf("runbook: open journal: %w", err)
	}

	return seq, nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // The journal's own directory.
	if err != nil {
		return err
	}

	defer func() { _ = d.Close() }()

	return d.Sync()
}

// journalLocked reports whether a writer holds the journal at path. It takes
// a shared lock for an instant: that fails only while a writer has the
// exclusive one.
func journalLocked(path string) bool {
	f, err := os.Open(path) //nolint:gosec // The journal path is the caller's to choose.
	if err != nil {
		return false
	}

	defer func() { _ = f.Close() }()

	if err := flock(f, syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		return errors.Is(err, syscall.EWOULDBLOCK)
	}

	_ = flock(f, syscall.LOCK_UN)

	return false
}

// fileStore appends lines to the journal file.
type fileStore struct {
	path string
	f    *os.File
	sync func() error

	// broken is the first write or sync failure. After it the file may end
	// in a half-written line, so nothing more is written until the journal is
	// opened again, which repairs it.
	broken error
}

func (s *fileStore) append(line []byte, durable bool) error {
	if s.broken != nil {
		return s.broken
	}

	// One write per line, break included, so a crash leaves at most one
	// half-written line, at the end.
	buf := make([]byte, 0, len(line)+1)
	buf = append(append(buf, line...), '\n')

	if _, err := s.f.Write(buf); err != nil {
		s.broken = err

		return err
	}

	if durable {
		if err := s.sync(); err != nil {
			s.broken = err

			return err
		}
	}

	return nil
}

func (s *fileStore) lines() ([][]byte, error) {
	var lines [][]byte

	_, err := scanJournal(s.path, func(line []byte) error {
		lines = append(lines, line)

		return nil
	})

	return lines, err
}

func (s *fileStore) close() error {
	// Closing the file releases the lock.
	return s.f.Close()
}
