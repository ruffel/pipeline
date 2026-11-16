package pipeline_test

import (
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/ruffel/pipeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// jsonFields returns the JSON names of a struct type's fields.
func jsonFields(t reflect.Type) map[string]bool {
	names := map[string]bool{}

	for f := range t.Fields() {
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		names[name] = true
	}

	return names
}

// JOURNAL.md is the reference for anything that reads events itself, so it
// must name every kind of event and every field, and no field that doesn't
// exist.
func TestJournalDoc_MatchesTheEventModel(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("JOURNAL.md")
	require.NoError(t, err)

	doc := string(raw)

	// The table of events lies between these two.
	_, rest, found := strings.Cut(doc, "\n## Events\n")
	require.True(t, found)

	events, _, found := strings.Cut(rest, "\nWhat each one means:")
	require.True(t, found)

	kinds := []pipeline.EventKind{
		pipeline.EventRunStarted, pipeline.EventRunFinished,
		pipeline.EventGroupStarted, pipeline.EventGroupFinished, pipeline.EventGroupReset,
		pipeline.EventStepStarted, pipeline.EventStepRunning, pipeline.EventStepFinished, pipeline.EventStepRetry,
		pipeline.EventGateWaiting, pipeline.EventGateReleased,
		pipeline.EventValue, pipeline.EventItem,
		pipeline.EventMessage, pipeline.EventProgress, pipeline.EventOutput, pipeline.EventCustom,
		pipeline.EventStopRequested, pipeline.EventUndoStarted, pipeline.EventUndoFinished,
		pipeline.EventPlanChanged,
	}

	fields := jsonFields(reflect.TypeFor[pipeline.Event]())
	quoted := regexp.MustCompile("`([^`]+)`")
	rows := map[string]string{}

	for line := range strings.SplitSeq(events, "\n") {
		cells := strings.Split(line, "|")
		if len(cells) < 5 || !strings.Contains(cells[1], "`") {
			continue
		}

		kind := strings.Trim(strings.TrimSpace(cells[1]), "`")
		if kind == "kind" {
			continue // The table's heading.
		}

		rows[kind] = line

		// The Durable column agrees with the code.
		want := "no"
		if pipeline.EventKind(kind).Durable() {
			want = "yes"
		}

		assert.Equal(t, want, strings.TrimSpace(cells[2]), "whether %s is durable", kind)

		// Every field the row names exists.
		for _, m := range quoted.FindAllStringSubmatch(cells[4], -1) {
			assert.True(t, fields[m[1]], "%s lists %q, which Event has no field for", kind, m[1])
		}
	}

	for _, kind := range kinds {
		assert.Contains(t, rows, string(kind), "the Events table has no row for %s", kind)
	}

	assert.Len(t, rows, len(kinds), "the Events table has a row for a kind that doesn't exist")

	// Every field of an event, and of a plan's node, is named somewhere.
	for name := range fields {
		assert.Contains(t, doc, "`"+name+"`", "Event's %q is not in JOURNAL.md", name)
	}

	for name := range jsonFields(reflect.TypeFor[pipeline.NodeSpec]()) {
		assert.Contains(t, doc, "`"+name+"`", "NodeSpec's %q is not in JOURNAL.md", name)
	}
}
