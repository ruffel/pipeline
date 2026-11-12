//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package runbook

import (
	"encoding/json"
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
	// The journal is read back by path, so fix the path now: a later change
	// of working directory must not point reads somewhere else.
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("runbook: open journal: %w", err)
	}

	f, err := openJournalFile(path)
	if err != nil {
		return nil, fmt.Errorf("runbook: open journal: %w", err)
	}

	if err := lockJournal(f); err != nil {
		_ = f.Close()

		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, &JournalBusyError{Path: path, PID: writerPID(path)}
		}

		// Not a competing writer: locking is unsupported here, or failed.
		return nil, fmt.Errorf("runbook: open journal: lock %s: %w", path, err)
	}

	// The file's directory entry must survive a crash too. Whoever holds the
	// lock syncs it, whether or not this call created the file: the creator
	// may have lost the race for the lock and returned busy.
	if err := syncDir(filepath.Dir(path)); err != nil {
		_ = f.Close()

		return nil, fmt.Errorf("runbook: open journal: %w", err)
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

// openJournalFile opens path for appending, creating it if need be.
func openJournalFile(path string) (*os.File, error) {
	//nolint:gosec // The journal path is the caller's to choose.
	return os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, journalFileMode)
}

// lockJournal takes the exclusive lock. While another holder has it, it
// retries briefly and then returns EWOULDBLOCK. Any other failure is returned
// at once.
func lockJournal(f *os.File) error {
	var err error

	for range lockAttempts {
		err = flock(f, syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return err
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
		v, lineSeq, err := checkLine(line)
		if err != nil {
			return err
		}

		if v > SchemaVersion {
			return fmt.Errorf("schema version %d is newer than this reader's %d", v, SchemaVersion)
		}

		if lineSeq <= seq {
			return fmt.Errorf("sequence number %d follows %d", lineSeq, seq)
		}

		seq = lineSeq

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

// checkLine validates one journal line and returns its schema version and
// sequence number. Output and other chatter is checked to be well-formed
// JSON with a proper envelope, which is several times faster than decoding
// it; everything else is decoded.
func checkLine(line []byte) (int, int64, error) {
	if env, ok := peekEvent(line); ok && env.chatter {
		if !json.Valid(line) {
			return 0, 0, errors.New("not valid JSON")
		}

		return env.v, env.seq, nil
	}

	var e Event

	// Not decodeEvent: the caller reports a newer version itself.
	if err := json.Unmarshal(line, &e); err != nil {
		return 0, 0, err
	}

	return e.V, e.Seq, nil
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
	// half-written line, at the end. The journal hands over the line and
	// doesn't use it again, so the break is appended in place.
	line = append(line, '\n')

	if _, err := s.f.Write(line); err != nil {
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

func (s *fileStore) scan(fn func(line []byte) error) error {
	_, err := scanJournal(s.path, fn)

	return err
}

func (s *fileStore) close() error {
	// Unlock explicitly. Closing would release the lock only once every copy
	// of the file is closed, and a subprocess this process is starting holds
	// one between its fork and its exec.
	_ = flock(s.f, syscall.LOCK_UN)

	return s.f.Close()
}
