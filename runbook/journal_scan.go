package runbook

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
)

// ErrJournalCorrupt is returned when a journal file holds a complete line
// that is not a valid event. A half-written last line is not corruption: it
// is what a crash leaves, and it is ignored or removed.
var ErrJournalCorrupt = errors.New("runbook: journal is corrupt")

// scanJournal reads the journal file at path line by line, with no limit on
// line length, calling fn with each complete line. It returns the offset just
// past the last complete line: anything after it is a half-written line left
// by a crash. A complete line that fn rejects is [ErrJournalCorrupt].
func scanJournal(path string, fn func(line []byte) error) (int64, error) {
	f, err := os.Open(path) //nolint:gosec // The journal path is the caller's to choose.
	if err != nil {
		return 0, err
	}

	defer func() { _ = f.Close() }()

	var (
		r      = bufio.NewReader(f)
		offset int64
		n      int
	)

	for {
		line, err := r.ReadBytes('\n')

		switch {
		case errors.Is(err, io.EOF):
			// What is left, if anything, has no line break: a torn write.
			return offset, nil
		case err != nil:
			return offset, fmt.Errorf("runbook: journal: read %s: %w", path, err)
		}

		n++

		if err := fn(line[:len(line)-1]); err != nil {
			return offset, fmt.Errorf("%w: %s line %d: %w", ErrJournalCorrupt, path, n, err)
		}

		offset += int64(len(line))
	}
}

// readJournal decodes the events in the journal file at path, ignoring a
// half-written last line. It takes no lock, so it can read a journal that a
// run is writing.
func readJournal(path string) ([]Event, error) {
	var events []Event

	_, err := scanJournal(path, func(line []byte) error {
		e, err := decodeEvent(line)
		if err != nil {
			return err
		}

		events = append(events, e)

		return nil
	})

	return events, err
}

// Inspect replays the journal file at path without locking it, so it works
// while a run is writing the journal, from this or any other process. A
// journal that does not exist yet is an empty state.
//
// The state's Live field reports whether a process holds the journal open
// for writing: replay alone cannot tell a run that is still going from one
// that crashed.
func Inspect(path string) (*State, error) {
	events, err := readJournal(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	state, err := Replay(events)
	if err != nil {
		// Events that decode but don't replay, such as sequence numbers that
		// go backwards, are damage too. OpenJournal reports the same file the
		// same way.
		return nil, fmt.Errorf("%w: %s: %w", ErrJournalCorrupt, path, err)
	}

	state.Live = journalLocked(path)

	return state, nil
}
