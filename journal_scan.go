package pipeline

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
)

var (
	// ErrJournalCorrupt is returned when a journal file holds a complete line
	// that is not a valid event. A half-written last line is not corruption:
	// it is what a crash leaves, and it is ignored or removed.
	ErrJournalCorrupt = errors.New("pipeline: journal is corrupt")

	// ErrJournalTooNew is returned when a journal holds an event written by
	// a newer release of this package, in a schema version this one does not
	// know: see [SchemaVersion]. The journal is not damaged, and nothing is
	// changed in it. The program reading it needs upgrading, as everything
	// that reads a journal must be before a tool uses a feature that raises
	// the version.
	ErrJournalTooNew = errors.New("pipeline: the journal was written by a newer release")
)

// tooNewError is an event in a schema version newer than this reader's. It
// matches [ErrJournalTooNew].
type tooNewError struct {
	v int
}

func (e *tooNewError) Error() string {
	return fmt.Sprintf("schema version %d is newer than this reader's %d", e.v, SchemaVersion)
}

func (e *tooNewError) Unwrap() error {
	return ErrJournalTooNew
}

// scanJournal reads the journal file at path line by line, with no limit on
// line length, calling fn with each complete line. It returns the offset just
// past the last complete line: anything after it is a half-written line left
// by a crash. A complete line that fn rejects is [ErrJournalCorrupt], or
// [ErrJournalTooNew] for one a newer release wrote.
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
		return fmt.Errorf("pipeline: journal: read %s: %w", r.path, err)
	}

	br := bufio.NewReaderSize(f, scanBuffer)

	for {
		// The line is only valid until the next read: fn must not keep it.
		line, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			line, err = longLine(br, line)
		}

		switch {
		case errors.Is(err, io.EOF):
			// What is left, if anything, has no line break: a torn write, or
			// one still in progress.
			return nil
		case err != nil:
			return fmt.Errorf("pipeline: journal: read %s: %w", r.path, err)
		}

		if err := fn(line[:len(line)-1]); err != nil {
			if h, ok := errors.AsType[*haltError](err); ok {
				return h.err
			}

			// A newer release's journal is not a damaged one.
			if errors.Is(err, ErrJournalTooNew) {
				return fmt.Errorf("%w: %s line %d: %w", ErrJournalTooNew, r.path, r.line+1, err)
			}

			return fmt.Errorf("%w: %s line %d: %w", ErrJournalCorrupt, r.path, r.line+1, err)
		}

		r.line++
		r.offset += int64(len(line))
	}
}

// scanBuffer is the reader's buffer. Lines that fit are handed to the
// callback without being copied, and nearly all do: output is cut to
// MaxOutputLine.
const scanBuffer = 64 << 10

// longLine reads the rest of a line that did not fit the reader's buffer.
func longLine(br *bufio.Reader, start []byte) ([]byte, error) {
	line := bytes.Clone(start)

	for {
		more, err := br.ReadSlice('\n')
		line = append(line, more...)

		if !errors.Is(err, bufio.ErrBufferFull) {
			return line, err
		}
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

// Inspect replays the journal file named by journal without locking it, so it
// works while a run is writing the journal, from this or any other process. A
// journal that does not exist yet is an empty state.
//
// The state's Live field reports whether a process holds the journal open
// for writing: replay alone cannot tell a run that is still going from one
// that crashed.
//
// Inspect is quick on a long journal. Output, progress and messages are most
// of one and change no state, so it reads only the start of those lines and
// never decodes them; it does not notice damage inside one. OpenJournal
// checks every line.
func Inspect(journal string) (*State, error) {
	state := newState()

	// The journal is replayed as it is read, never held in memory: a long
	// run's journal is mostly output, which can be far larger than the state.
	// A line that doesn't decode or doesn't follow the last one is damage,
	// reported as OpenJournal reports it.
	_, err := scanJournal(journal, state.addLine)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	state.Live = journalLocked(journal)

	return state, nil
}

// ReadEvents reads the journal file named by journal without locking it, as
// [Inspect] does, and returns every event in it with the state they replay
// to. The two come from one reading, so they agree: the state's LastSeq is
// the last event's, which an Inspect and a separate read of a journal that a
// run is writing can't promise. A journal that does not exist yet has no
// events, and an empty state.
//
// It is for a tool that wants a run's events as data: a report, or a status
// that shows what the state does not keep, such as messages and output. The
// events of one attempt share its Run. A half-written last line is left
// out, as it is by Inspect.
//
// It decodes every line and holds every event in memory, and a long run's
// output can be large: Inspect is the quick way to the state alone, and
// [Follow] gives events one at a time, with those still to come.
func ReadEvents(journal string) ([]Event, *State, error) {
	var events []Event

	state := newState()

	_, err := scanJournal(journal, func(line []byte) error {
		e, err := decodeEvent(line)
		if err != nil {
			return err
		}

		if err := state.add(e); err != nil {
			return err
		}

		events = append(events, e)

		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}

	state.Live = journalLocked(journal)

	return events, state, nil
}
