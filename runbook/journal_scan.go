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
	r := journalReader{path: path}
	err := r.next(fn)

	return r.offset, err
}

// journalReader reads a journal file in stages, so a viewer can come back
// for the lines written since it last looked.
type journalReader struct {
	path string
	// offset is just past the last complete line read, and line is how many
	// have been read.
	offset int64
	line   int
}

// next calls fn with each complete line after the last one read. A
// half-written line at the end is left for the next call.
func (r *journalReader) next(fn func(line []byte) error) error {
	f, err := os.Open(r.path)
	if err != nil {
		return err
	}

	defer func() { _ = f.Close() }()

	if _, err := f.Seek(r.offset, io.SeekStart); err != nil {
		return fmt.Errorf("runbook: journal: read %s: %w", r.path, err)
	}

	br := bufio.NewReader(f)

	for {
		line, err := br.ReadBytes('\n')

		switch {
		case errors.Is(err, io.EOF):
			// What is left, if anything, has no line break: a torn write, or
			// one still in progress.
			return nil
		case err != nil:
			return fmt.Errorf("runbook: journal: read %s: %w", r.path, err)
		}

		if err := fn(line[:len(line)-1]); err != nil {
			if h, ok := errors.AsType[*haltError](err); ok {
				return h.err
			}

			return fmt.Errorf("%w: %s line %d: %w", ErrJournalCorrupt, r.path, r.line+1, err)
		}

		r.line++
		r.offset += int64(len(line))
	}
}

// haltError lets a reader's callback stop the read for a reason that is not
// the line's fault, such as a cancelled context. The line is not consumed,
// and the reason is returned as it is, not as [ErrJournalCorrupt].
type haltError struct {
	err error
}

func (h *haltError) Error() string {
	return h.err.Error()
}

func halt(err error) error {
	return &haltError{err: err}
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
