package pipeline_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ruffel/pipeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// layout is what a tool might tell a viewer about the cluster an attempt
// works on.
type layout struct {
	Controller string   `json:"controller"`
	Nodes      []string `json:"nodes"`
}

// Run data is recorded with the attempt's start, before any step reports,
// and is each attempt's own. Unlike the inputs it binds nothing: a later
// attempt is given other data, or none, and nothing refuses it.
func TestRunData_IsEachAttemptsOwn(t *testing.T) {
	t.Parallel()

	fails := true
	plan := mustPlan(t, pipeline.Effect("a", counted(&calls{}, "a", &fails)))
	inputs := pipeline.WithInputs(map[string]string{"version": "8.2"})
	j := pipeline.MemoryJournal()

	first := layout{Controller: "node1", Nodes: []string{"node1", "node2"}}
	out := resumeOn(t, j, plan, inputs, pipeline.WithRunData(first))

	require.ErrorContains(t, out.err, "a failed")
	require.Equal(t, pipeline.EventRunStarted, out.events[0].Kind)
	assert.JSONEq(t, `{"controller":"node1","nodes":["node1","node2"]}`, string(out.events[0].Data),
		"it is there before any step has reported")

	digest, fingerprint := stateOf(t, j).InputsDigest, stateOf(t, j).Fingerprint

	second := layout{Controller: "node2", Nodes: []string{"node1", "node2", "node3"}}
	out = resumeOn(t, j, plan, inputs, pipeline.WithRunData(second))
	require.ErrorContains(t, out.err, "a failed", "other data is no reason to refuse a resume")

	fails = false

	require.NoError(t, resumeOn(t, j, plan, inputs).err)
	require.NoError(t, resumeRollback(t, j, plan, inputs, pipeline.WithRunData("undoing it")).err)

	state := stateOf(t, j)
	require.Len(t, state.Attempts, 4)

	var got layout

	require.NoError(t, json.Unmarshal(state.Attempts[0].Data, &got))
	assert.Equal(t, first, got)
	require.NoError(t, json.Unmarshal(state.Attempts[1].Data, &got))
	assert.Equal(t, second, got)
	assert.Nil(t, state.Attempts[2].Data, "an attempt given none has none")
	assert.JSONEq(t, `"undoing it"`, string(state.Attempts[3].Data), "a rollback is an attempt like any other")

	assert.Equal(t, digest, state.InputsDigest, "it is no part of the inputs")
	assert.Equal(t, fingerprint, state.Fingerprint, "or of the plan")
}

// Data that can't be recorded is refused before the attempt starts: nothing
// is written, and the journal is as it was.
func TestRunData_RefusesWhatItCannotRecord(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		data any
		want string
	}{
		"it can't be encoded": {data: func() {}, want: "pipeline: run data:"},
		"it is too large":     {data: strings.Repeat("x", pipeline.MaxRunDataBytes), want: "over the 65536 limit"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			fails := true
			plan := mustPlan(t, pipeline.Effect("a", counted(&calls{}, "a", &fails)))
			j := pipeline.MemoryJournal()

			out := resumeOn(t, j, plan, pipeline.WithRunData(tc.data))

			require.ErrorContains(t, out.err, tc.want)
			assert.Nil(t, out.res, "no attempt started")
			assert.Empty(t, out.events)

			// On a journal that has an attempt, too.
			require.ErrorContains(t, resumeOn(t, j, plan).err, "a failed")

			out = resumeOn(t, j, plan, pipeline.WithRunData(tc.data))

			require.ErrorContains(t, out.err, tc.want)
			assert.Empty(t, out.events)
			assert.Len(t, stateOf(t, j).Attempts, 1)
		})
	}

	// The limit is on the encoded data, and data of that size is taken.
	out := resumeOn(t, pipeline.MemoryJournal(), mustPlan(t, pipeline.Effect("a", ok())),
		pipeline.WithRunData(strings.Repeat("x", pipeline.MaxRunDataBytes-2)))
	require.NoError(t, out.err)
}

// Nil is no data, whatever its type, and nothing is written for it.
func TestRunData_NilIsNoData(t *testing.T) {
	t.Parallel()

	var none *layout

	for name, data := range map[string]any{"nil": nil, "a nil pointer": none} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			j := pipeline.MemoryJournal()
			out := resumeOn(t, j, mustPlan(t, pipeline.Effect("a", ok())), pipeline.WithRunData(data))

			require.NoError(t, out.err)
			assert.Nil(t, out.events[0].Data)
			assert.Nil(t, stateOf(t, j).Attempts[0].Data)
		})
	}
}

// The strings in run data are redacted, as a message is, and a Secret is
// never recorded, here or among the inputs.
func TestRunData_IsRedacted(t *testing.T) {
	t.Parallel()

	type access struct {
		Host     string          `json:"host"`
		Note     string          `json:"note"`
		Password pipeline.Secret `json:"password"`
		Port     int             `json:"port"`
	}

	token := pipeline.NewSecret("s3cr3t-token")
	j := pipeline.MemoryJournal()

	out := resumeOn(t, j, mustPlan(t, pipeline.Effect("a", ok())),
		pipeline.WithInputs(struct{ Token pipeline.Secret }{Token: token}),
		pipeline.WithRedactor(func(s string) string { return strings.ReplaceAll(s, "hunter2", "***") }),
		pipeline.WithRunData(access{
			Host: "node1", Note: "password hunter2, token s3cr3t-token", Password: pipeline.NewSecret("hunter2"), Port: 22,
		}))

	require.NoError(t, out.err)

	raw := string(stateOf(t, j).Attempts[0].Data)
	assert.NotContains(t, raw, "hunter2")
	assert.NotContains(t, raw, "s3cr3t-token", "a secret among the inputs is masked here as it is anywhere")

	var got map[string]any

	require.NoError(t, json.Unmarshal([]byte(raw), &got))
	assert.Equal(t, "node1", got["host"])
	assert.Nil(t, got["password"], "a Secret is recorded as null")
	assert.InDelta(t, 22, got["port"], 0)
	assert.Contains(t, got["note"], "password ***")

	// What Run returns for data it refuses is redacted too.
	_, err := pipeline.Run(t.Context(), mustPlan(t, pipeline.Effect("a", ok())), pipeline.MemoryJournal(),
		pipeline.WithRunData(func() {}))

	var unsupported *json.UnsupportedTypeError

	require.ErrorAs(t, err, &unsupported, "and still unwraps to what went wrong")
}
