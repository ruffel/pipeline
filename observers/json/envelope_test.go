package json

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEnvelopeMarshalJSON_ExtraCannotOverrideStructural is a regression test
// for the MarshalJSON ordering bug.
func TestEnvelopeMarshalJSON_ExtraCannotOverrideStructural(t *testing.T) {
	t.Parallel()

	e := envelope{
		Type:      "StepPassed",
		Pipeline:  "deploy",
		Timestamp: "2026-01-01T00:00:00Z",
		Extra: fields{
			// Attempt to overwrite reserved structural keys via Extra.
			"type":      "INJECTED",
			"timestamp": "INJECTED",
			"pipeline":  "INJECTED",
			// Non-conflicting key should still appear.
			"custom": "value",
		},
	}

	b, err := json.Marshal(e)
	require.NoError(t, err)

	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m))

	assert.Equal(t, "StepPassed", m["type"], "structural 'type' must not be overridden by Extra")
	assert.Equal(t, "2026-01-01T00:00:00Z", m["timestamp"], "structural 'timestamp' must not be overridden by Extra")
	assert.Equal(t, "deploy", m["pipeline"], "structural 'pipeline' must not be overridden by Extra")
	assert.Equal(t, "value", m["custom"], "non-conflicting Extra key must appear in output")
}
