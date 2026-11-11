package runbook

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// spyStore records what the journal asked it to store, and can fail.
type spyStore struct {
	memoryStore

	durable []bool
	fail    error
}

func (s *spyStore) append(line []byte, durable bool) error {
	if s.fail != nil {
		return s.fail
	}

	s.durable = append(s.durable, durable)

	return s.memoryStore.append(line, durable)
}

func TestJournal_AppendStampsVersionAndSequence(t *testing.T) {
	t.Parallel()

	j := MemoryJournal()
	ts := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)

	first, err := j.append(Event{Kind: EventRunStarted, TS: ts, Run: "r1"})
	require.NoError(t, err)

	second, err := j.append(Event{Kind: EventOutput, TS: ts, Run: "r1", Line: "hello"})
	require.NoError(t, err)

	assert.Equal(t, int64(1), first.Seq)
	assert.Equal(t, int64(2), second.Seq)
	assert.Equal(t, SchemaVersion, second.V)

	events, err := j.Events()
	require.NoError(t, err)
	assert.Equal(t, []Event{first, second}, events, "what observers get is what replay reads")
}

func TestJournal_TellsTheStoreWhichEventsAreDurable(t *testing.T) {
	t.Parallel()

	spy := &spyStore{}
	j := &Journal{store: spy}

	for _, k := range []EventKind{EventStepStarted, EventOutput, EventValue, EventProgress} {
		_, err := j.append(Event{Kind: k})
		require.NoError(t, err)
	}

	assert.Equal(t, []bool{true, false, true, false}, spy.durable)
}

func TestJournal_FailedWriteDoesNotAdvanceTheSequence(t *testing.T) {
	t.Parallel()

	spy := &spyStore{fail: errors.New("disk full")}
	j := &Journal{store: spy}

	_, err := j.append(Event{Kind: EventStepStarted, Path: "p/a"})
	require.ErrorContains(t, err, "write step.started event: disk full")

	spy.fail = nil

	e, err := j.append(Event{Kind: EventStepStarted, Path: "p/a"})
	require.NoError(t, err)
	assert.Equal(t, int64(1), e.Seq)
}

func TestJournal_UnencodableEventIsAnError(t *testing.T) {
	t.Parallel()

	j := MemoryJournal()

	_, err := j.append(Event{Kind: EventCustom, Data: json.RawMessage(`{not json`)})
	require.ErrorContains(t, err, "encode custom event")

	events, err := j.Events()
	require.NoError(t, err)
	assert.Empty(t, events)
}

func TestJournal_ClosedJournalRefusesWrites(t *testing.T) {
	t.Parallel()

	j := MemoryJournal()
	require.NoError(t, j.Close())
	require.NoError(t, j.Close(), "closing twice is fine")

	_, err := j.append(Event{Kind: EventRunStarted})
	require.ErrorIs(t, err, ErrJournalClosed)
}

func TestJournal_RefusesANewerSchemaVersion(t *testing.T) {
	t.Parallel()

	j := MemoryJournal()
	_, err := j.append(Event{Kind: EventRunStarted})
	require.NoError(t, err)

	mem, ok := j.store.(*memoryStore)
	require.True(t, ok)

	mem.data = append(mem.data, []byte(`{"v":99,"seq":2,"kind":"run.finished"}`))

	_, err = j.Events()
	require.ErrorContains(t, err, "journal line 2: schema version 99 is newer than this reader's 1")
}

func TestJournal_StateReplaysWhatWasAppended(t *testing.T) {
	t.Parallel()

	j := MemoryJournal()

	for _, e := range []Event{
		{Kind: EventRunStarted, Run: "r1", Attempt: 1, Fingerprint: "sha256:x"},
		{Kind: EventStepStarted, Run: "r1", Path: "p/a"},
		{Kind: EventStepFinished, Run: "r1", Path: "p/a", Outcome: OutcomePassed},
	} {
		_, err := j.append(e)
		require.NoError(t, err)
	}

	s, err := j.State()
	require.NoError(t, err)
	assert.Equal(t, StatusPassed, s.Element("p/a").Status)
	assert.Equal(t, int64(3), s.LastSeq)
	assert.Equal(t, "sha256:x", s.Fingerprint)
}
