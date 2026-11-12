package runbook

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every kind of event, as the journal writes it, must be recognised from its
// envelope, and classed as chatter exactly when it is not durable. If the
// Event struct's leading fields are reordered, this fails: replay would still
// be correct, but as slow as a full decode.
func TestPeekEvent_AgreesWithTheJournalsOwnEncoding(t *testing.T) {
	t.Parallel()

	kinds := []EventKind{
		EventRunStarted, EventRunFinished, EventGroupStarted, EventGroupFinished,
		EventStepStarted, EventStepRunning, EventStepFinished, EventValue, EventItem,
		EventOutput, EventProgress, EventMessage, EventCustom, EventStepRetry,
	}

	for _, kind := range kinds {
		line, err := json.Marshal(Event{
			V: SchemaVersion, Seq: 1234567, TS: time.Date(2026, 11, 11, 9, 0, 0, 123, time.UTC),
			Kind: kind, Path: `p/"quoted"`, Line: `tricky "kind":"run.started" text`,
		})
		require.NoError(t, err)

		env, ok := peekEvent(line)
		require.True(t, ok, "%s: %s", kind, line)
		assert.Equal(t, SchemaVersion, env.v, kind)
		assert.Equal(t, int64(1234567), env.seq, kind)
		assert.Equal(t, !kind.Durable(), env.chatter, kind)
	}
}

// Anything that isn't the journal's exact layout is left to the decoder.
func TestPeekEvent_DeclinesWhatItDoesNotRecognise(t *testing.T) {
	t.Parallel()

	for name, line := range map[string]string{
		"empty":             ``,
		"not json":          `this is not json`,
		"no timestamp":      `{"v":1,"seq":2,"kind":"output","line":"x"}`,
		"reordered":         `{"seq":2,"v":1,"ts":"2026-11-11T09:00:00Z","kind":"output"}`,
		"leading space":     ` {"v":1,"seq":2,"ts":"2026-11-11T09:00:00Z","kind":"output"}`,
		"spaced":            `{"v": 1, "seq": 2, "ts": "2026-11-11T09:00:00Z", "kind": "output"}`,
		"cut off":           `{"v":1,"seq":2,"ts":"2026-11-11T09:00:00Z","kind":"output","line":"x`,
		"no seq digits":     `{"v":1,"seq":,"ts":"2026-11-11T09:00:00Z","kind":"output"}`,
		"negative seq":      `{"v":1,"seq":-2,"ts":"2026-11-11T09:00:00Z","kind":"output"}`,
		"seq too long":      `{"v":1,"seq":12345678901234567890,"ts":"2026-11-11T09:00:00Z","kind":"output"}`,
		"escape in ts":      `{"v":1,"seq":2,"ts":"x\\","kind":"output","line":"x","kind":"run.finished"}`,
		"unterminated ts":   `{"v":1,"seq":2,"ts":"2026-11-11T09:00:00Z}`,
		"unterminated kind": `{"v":1,"seq":2,"ts":"2026-11-11T09:00:00Z","kind":"output}`,
	} {
		_, ok := peekEvent([]byte(line))
		assert.False(t, ok, name)
	}
}

func TestPeekEvent_AnUnknownKindIsNotChatter(t *testing.T) {
	t.Parallel()

	env, ok := peekEvent([]byte(`{"v":1,"seq":2,"ts":"2026-11-11T09:00:00Z","kind":"gate.waiting"}`))
	require.True(t, ok)
	assert.False(t, env.chatter, "a kind this reader doesn't know is decoded in full")
}
