package pipeline_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ruffel/pipeline"
	"github.com/ruffel/pipeline/pipelinetest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hostKey is what a step might say about one target of a check, in fields
// of its own.
type hostKey struct {
	Node        string `json:"node"`
	Check       string `json:"check"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

// ItemWith keeps data of the step's own with an item: in the event, in the
// state, and for a later attempt of the step to read back.
func TestItemWith_KeepsTheStepsDataWithTheItem(t *testing.T) {
	t.Parallel()

	var (
		fails    = true
		previous map[string]pipeline.ItemState
	)

	plan := mustPlan(t, pipeline.Effect("keys", pipeline.StepFunc(func(_ context.Context, s *pipeline.StepContext) error {
		previous = s.PreviousItems()

		err := errors.Join(
			s.ItemWith("ssh/node7", pipeline.ItemPassed, "recorded", hostKey{Node: "node7", Check: "ssh", Fingerprint: "SHA256:abc"}),
			s.Item("ssh/node8", pipeline.ItemPassed, "known"),
			s.ItemWith("ssh/node9", pipeline.ItemFailed, "", nil),
		)
		if err != nil || !fails {
			return err
		}

		return errors.New("boom")
	})))
	j := pipeline.MemoryJournal()

	out := resumeOn(t, j, plan)
	require.ErrorContains(t, out.err, "boom")
	assert.Empty(t, previous)

	items := kinds(out.events, pipeline.EventItem)
	require.Len(t, items, 3)
	assert.JSONEq(t, `{"node":"node7","check":"ssh","fingerprint":"SHA256:abc"}`, string(items[0].Data))
	assert.Nil(t, items[1].Data, "an item with no data says nothing of it")
	assert.Nil(t, items[2].Data, "and nil is no data")

	state := stateOf(t, j).Items("p/keys")
	assert.Equal(t, "recorded", state["ssh/node7"].Message)

	var got hostKey

	require.NoError(t, json.Unmarshal(state["ssh/node7"].Data, &got))
	assert.Equal(t, hostKey{Node: "node7", Check: "ssh", Fingerprint: "SHA256:abc"}, got)
	assert.Nil(t, state["ssh/node8"].Data)

	fails = false

	require.NoError(t, resumeOn(t, j, plan).err)
	assert.Equal(t, state, previous, "a later attempt reads the data back with the item")
}

// The latest result for a target replaces the one before, data and all.
func TestItemWith_TheNextResultReplacesTheData(t *testing.T) {
	t.Parallel()

	out := runPlan(t, t.Context(), mustPlan(t, pipeline.Effect("a", pipeline.StepFunc(
		func(_ context.Context, s *pipeline.StepContext) error {
			return errors.Join(
				s.ItemWith("node7", pipeline.ItemFailed, "", hostKey{Node: "node7"}),
				s.ItemWith("node8", pipeline.ItemFailed, "", hostKey{Node: "node8"}),
				s.ItemWith("node7", pipeline.ItemPassed, "", hostKey{Node: "node7", Check: "again"}),
				s.Item("node8", pipeline.ItemPassed, ""),
			)
		}))))

	require.NoError(t, out.err)

	items := out.state.Items("p/a")
	assert.JSONEq(t, `{"node":"node7","check":"again"}`, string(items["node7"].Data))
	assert.Nil(t, items["node8"].Data, "a result with no data leaves none behind")
}

// Data that can't be encoded, or is too large, is refused, and the item is
// not recorded.
func TestItemWith_RefusesDataItCannotKeep(t *testing.T) {
	t.Parallel()

	var unencodable, large, fits error

	out := runPlan(t, t.Context(), mustPlan(t, pipeline.Effect("a", pipeline.StepFunc(
		func(_ context.Context, s *pipeline.StepContext) error {
			unencodable = s.ItemWith("fn", pipeline.ItemPassed, "", func() {})
			large = s.ItemWith("large", pipeline.ItemPassed, "", strings.Repeat("x", pipeline.MaxItemDataBytes))
			fits = s.ItemWith("fits", pipeline.ItemPassed, "", strings.Repeat("x", pipeline.MaxItemDataBytes-2))

			return nil
		}))))

	require.NoError(t, out.err)
	require.ErrorContains(t, unencodable, `pipeline: item "fn"`)
	require.ErrorContains(t, large, "over the 4096 limit")
	require.NoError(t, fits, "the limit is on the encoded data")
	assert.Equal(t, []string{"fits"}, itemIDs(out.state.Items("p/a")))
}

func itemIDs(items map[string]pipeline.ItemState) []string {
	ids := make([]string, 0, len(items))
	for id := range items {
		ids = append(ids, id)
	}

	return ids
}

// The strings in an item's data are redacted, as its message is: keys,
// numbers and shape are kept.
func TestItemWith_ItsDataIsRedacted(t *testing.T) {
	t.Parallel()

	redact := pipeline.WithRedactor(func(s string) string { return strings.ReplaceAll(s, "hunter2", "***") })

	out := runPlan(t, t.Context(), mustPlan(t, pipeline.Effect("a", pipeline.StepFunc(
		func(_ context.Context, s *pipeline.StepContext) error {
			return s.ItemWith("node7", pipeline.ItemPassed, "password hunter2", map[string]any{"password": "hunter2", "port": 22})
		}))), redact)

	require.NoError(t, out.err)

	item := out.state.Items("p/a")["node7"]
	assert.Equal(t, "password ***", item.Message)
	assert.JSONEq(t, `{"password":"***","port":22}`, string(item.Data))
}

// Every report of a task says when the step first reported on it, so a
// viewer that sees only the latest can say how long the task has run. Each
// task has its own start, and so has each attempt.
func TestProgress_SaysWhenTheTaskWasFirstReported(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	clock := pipelinetest.NewFakeClock(start)
	fails := true

	plan := mustPlan(t, pipeline.Effect("a", pipeline.StepFunc(func(_ context.Context, s *pipeline.StepContext) error {
		s.Progress("copy", "copying", 1, 3)
		clock.Advance(40 * time.Second)
		s.Progress("verify", "still running", 0, 0)
		clock.Advance(20 * time.Second)
		s.Progress("copy", "copying", 3, 3)
		s.Progress("verify", "still running", 0, 0)

		if fails {
			return errors.New("boom")
		}

		return nil
	})))
	j := pipeline.MemoryJournal()

	out := resumeOn(t, j, plan, pipeline.WithClock(clock))
	require.ErrorContains(t, out.err, "boom")

	reports := kinds(out.events, pipeline.EventProgress)
	require.Len(t, reports, 4)

	type report struct {
		key     string
		started time.Time
		elapsed time.Duration
	}

	seen := make([]report, 0, len(reports))
	for _, e := range reports {
		seen = append(seen, report{key: e.Key, started: e.Started, elapsed: e.TS.Sub(e.Started)})
	}

	assert.Equal(t, []report{
		{key: "copy", started: start, elapsed: 0},
		{key: "verify", started: start.Add(40 * time.Second), elapsed: 0},
		{key: "copy", started: start, elapsed: time.Minute},
		{key: "verify", started: start.Add(40 * time.Second), elapsed: 20 * time.Second},
	}, seen)

	// A later attempt's task starts when that attempt first reports on it.
	fails = false
	out = resumeOn(t, j, plan, pipeline.WithClock(clock))

	require.NoError(t, out.err)
	assert.Equal(t, start.Add(time.Minute), kinds(out.events, pipeline.EventProgress)[0].Started)
}
