package runbook

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// ErrJournalClosed is returned when writing to a closed journal.
var ErrJournalClosed = errors.New("runbook: journal is closed")

// Journal is the durable record of a plan's runs. Every outcome is written to
// it before observers are told, and the state of a run is computed by
// replaying it. Use [MemoryJournal] for tests and tools that don't need
// anything to survive the process.
type Journal struct {
	mu     sync.Mutex
	store  store
	seq    int64
	closed bool
	hook   func(Event) error
}

// JournalOption configures a journal.
type JournalOption func(*Journal)

// WithWriteHook calls hook with each event, already numbered, before the
// journal stores it. If hook returns an error, the write fails with it
// exactly as a storage failure would, and the run stops. It exists for fault
// injection in tests: see the runbooktest package.
func WithWriteHook(hook func(Event) error) JournalOption {
	return func(j *Journal) {
		j.hook = hook
	}
}

// store holds the journal's encoded lines. The file store fsyncs durable
// lines; the memory store just keeps them.
type store interface {
	append(line []byte, durable bool) error
	lines() ([][]byte, error)
	close() error
}

// MemoryJournal returns a journal held in memory. It behaves like a file
// journal, including encoding every event, but nothing survives the process.
func MemoryJournal(opts ...JournalOption) *Journal {
	j := &Journal{store: &memoryStore{}}

	for _, o := range opts {
		o(j)
	}

	return j
}

// Events decodes and returns every event in the journal, in order.
func (j *Journal) Events() ([]Event, error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	lines, err := j.store.lines()
	if err != nil {
		return nil, err
	}

	events := make([]Event, 0, len(lines))

	for i, line := range lines {
		e, err := decodeEvent(line)
		if err != nil {
			return nil, fmt.Errorf("runbook: journal line %d: %w", i+1, err)
		}

		events = append(events, e)
	}

	return events, nil
}

// State replays the journal. See [Replay].
func (j *Journal) State() (*State, error) {
	events, err := j.Events()
	if err != nil {
		return nil, err
	}

	return Replay(events)
}

// Close closes the journal. Later writes fail with [ErrJournalClosed].
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()

	if j.closed {
		return nil
	}

	j.closed = true

	return j.store.close()
}

// append stamps e with the schema version and the next sequence number,
// stores it, and returns it decoded from the stored line: encoding can change
// data (invalid UTF-8 is replaced, raw JSON is compacted), and observers must
// see exactly what a replay of the journal sees. A durable event is stored
// before append returns; if that fails, the caller must stop the run.
func (j *Journal) append(e Event) (Event, error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	if j.closed {
		return Event{}, ErrJournalClosed
	}

	e.V = SchemaVersion
	e.Seq = j.seq + 1

	line, err := json.Marshal(e)
	if err != nil {
		return Event{}, fmt.Errorf("runbook: journal: encode %s event: %w", e.Kind, err)
	}

	if j.hook != nil {
		if err := j.hook(e); err != nil {
			return Event{}, fmt.Errorf("runbook: journal: write %s event: %w", e.Kind, err)
		}
	}

	stored, err := decodeEvent(line)
	if err != nil {
		return Event{}, fmt.Errorf("runbook: journal: decode %s event: %w", e.Kind, err)
	}

	if err := j.store.append(line, e.Kind.Durable()); err != nil {
		return Event{}, fmt.Errorf("runbook: journal: write %s event: %w", e.Kind, err)
	}

	j.seq = e.Seq

	return stored, nil
}

func decodeEvent(line []byte) (Event, error) {
	var e Event

	if err := json.Unmarshal(line, &e); err != nil {
		return Event{}, err
	}

	if e.V > SchemaVersion {
		return Event{}, fmt.Errorf("schema version %d is newer than this reader's %d", e.V, SchemaVersion)
	}

	return e, nil
}

type memoryStore struct {
	data [][]byte
}

func (m *memoryStore) append(line []byte, _ bool) error {
	m.data = append(m.data, line)

	return nil
}

func (m *memoryStore) lines() ([][]byte, error) {
	return append([][]byte(nil), m.data...), nil
}

func (m *memoryStore) close() error {
	return nil
}
