package json_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ruffel/pipeline"
	jsonobs "github.com/ruffel/pipeline/observers/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var fixedTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) //nolint:gochecknoglobals

func TestObserver_HappyPath(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	obs := jsonobs.New(&buf)
	ctx := t.Context()

	events := []pipeline.Event{
		pipeline.PipelineStartedEvent{
			BaseEvent:  base("deploy", "", "", fixedTime),
			Definition: pipeline.Pipeline{Name: "deploy"},
		},
		pipeline.StageStartedEvent{BaseEvent: base("deploy", "build", "", fixedTime)},
		pipeline.StepStartedEvent{BaseEvent: base("deploy", "build", "compile", fixedTime)},
		pipeline.StepPassedEvent{BaseEvent: base("deploy", "build", "compile", fixedTime.Add(time.Second))},
		pipeline.StagePassedEvent{BaseEvent: base("deploy", "build", "", fixedTime.Add(time.Second))},
		pipeline.PipelinePassedEvent{BaseEvent: base("deploy", "", "", fixedTime.Add(time.Second))},
	}

	for _, e := range events {
		obs.OnEvent(ctx, e)
	}

	lines := nonEmptyLines(buf.String())
	require.Len(t, lines, 6)

	// Verify each line is valid JSON with correct type.
	types := parseTypes(t, lines)
	assert.Equal(t, []string{
		"PipelineStarted",
		"StageStarted",
		"StepStarted",
		"StepPassed",
		"StagePassed",
		"PipelinePassed",
	}, types)
}

func TestObserver_TypeDiscriminator(t *testing.T) {
	t.Parallel()

	cases := []struct {
		event    pipeline.Event
		wantType string
	}{
		{pipeline.PipelineStartedEvent{BaseEvent: base("p", "", "", fixedTime), Definition: pipeline.Pipeline{Name: "p"}}, "PipelineStarted"},
		{pipeline.PipelinePassedEvent{BaseEvent: base("p", "", "", fixedTime)}, "PipelinePassed"},
		{pipeline.PipelineFailedEvent{BaseEvent: base("p", "", "", fixedTime), Err: errors.New("e")}, "PipelineFailed"},
		{pipeline.StageStartedEvent{BaseEvent: base("p", "s", "", fixedTime)}, "StageStarted"},
		{pipeline.StagePassedEvent{BaseEvent: base("p", "s", "", fixedTime)}, "StagePassed"},
		{pipeline.StageFailedEvent{BaseEvent: base("p", "s", "", fixedTime), Err: errors.New("e")}, "StageFailed"},
		{pipeline.StageSkippedEvent{BaseEvent: base("p", "s", "", fixedTime), Reason: "r"}, "StageSkipped"},
		{pipeline.StepStartedEvent{BaseEvent: base("p", "s", "step", fixedTime)}, "StepStarted"},
		{pipeline.StepPassedEvent{BaseEvent: base("p", "s", "step", fixedTime)}, "StepPassed"},
		{pipeline.StepFailedEvent{BaseEvent: base("p", "s", "step", fixedTime), Err: errors.New("e")}, "StepFailed"},
		{pipeline.StepSkippedEvent{BaseEvent: base("p", "s", "step", fixedTime), Reason: "r"}, "StepSkipped"},
		{pipeline.MessageEvent{BaseEvent: base("p", "s", "step", fixedTime), Level: pipeline.LevelInfo, Message: "m"}, "Message"},
		{pipeline.OutputEvent{BaseEvent: base("p", "s", "step", fixedTime), Line: "l"}, "Output"},
		{pipeline.ProgressEvent{BaseEvent: base("p", "s", "step", fixedTime), Message: "m"}, "Progress"},
		{pipeline.CustomEvent{BaseEvent: base("p", "s", "step", fixedTime), Type: "x.y"}, "Custom"},
	}

	for _, tt := range cases {
		t.Run(tt.wantType, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer

			obs := jsonobs.New(&buf)
			obs.OnEvent(t.Context(), tt.event)

			m := parseLine(t, buf.String())
			assert.Equal(t, tt.wantType, m["type"])
		})
	}
}

func TestObserver_ErrorAsString(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	obs := jsonobs.New(&buf)

	obs.OnEvent(t.Context(), pipeline.StepFailedEvent{
		BaseEvent: base("p", "s", "step", fixedTime),
		Err:       errors.New("connection refused"),
	})

	m := parseLine(t, buf.String())
	assert.Equal(t, "connection refused", m["error"])
}

func TestObserver_Duration(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	obs := jsonobs.New(&buf)
	ctx := t.Context()

	obs.OnEvent(ctx, pipeline.StepStartedEvent{BaseEvent: base("p", "s", "step", fixedTime)})
	obs.OnEvent(ctx, pipeline.StepPassedEvent{BaseEvent: base("p", "s", "step", fixedTime.Add(1500*time.Millisecond))})

	lines := nonEmptyLines(buf.String())
	require.Len(t, lines, 2)

	passed := parseLine(t, lines[1])
	assert.InDelta(t, float64(1500), passed["durationMs"], 0.1)
}

func TestObserver_SkipEvent(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	obs := jsonobs.New(&buf)

	obs.OnEvent(t.Context(), pipeline.StageSkippedEvent{
		BaseEvent: base("p", "optional", "", fixedTime),
		Reason:    "not needed",
	})

	m := parseLine(t, buf.String())
	assert.Equal(t, "StageSkipped", m["type"])
	assert.Equal(t, "not needed", m["reason"])
}

func TestObserver_MessageFields(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	obs := jsonobs.New(&buf)

	obs.OnEvent(t.Context(), pipeline.MessageEvent{
		BaseEvent: base("p", "s", "step", fixedTime),
		Level:     pipeline.LevelWarn,
		Message:   "slow query",
		Attrs:     map[string]string{"host": "prod-1"},
	})

	m := parseLine(t, buf.String())
	assert.Equal(t, "warn", m["level"])
	assert.Equal(t, "slow query", m["message"])
}

func TestObserver_OutputFields(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	obs := jsonobs.New(&buf)

	obs.OnEvent(t.Context(), pipeline.OutputEvent{
		BaseEvent: base("p", "s", "step", fixedTime),
		Stream:    pipeline.Stderr,
		Line:      "error: not found",
	})

	m := parseLine(t, buf.String())
	assert.Equal(t, "stderr", m["stream"])
	assert.Equal(t, "error: not found", m["line"])
}

// -------------------------------------------------------------------------
// Err (sticky error)
// -------------------------------------------------------------------------

type failWriter struct{}

func (failWriter) Write(_ []byte) (int, error) {
	return 0, errors.New("disk full")
}

func TestObserver_ErrNilOnSuccess(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	obs := jsonobs.New(&buf)
	obs.OnEvent(t.Context(), pipeline.StepStartedEvent{BaseEvent: base("p", "s", "step", fixedTime)})

	assert.NoError(t, obs.Err())
}

func TestObserver_ErrCapturesWriteFailure(t *testing.T) {
	t.Parallel()

	obs := jsonobs.New(failWriter{})
	obs.OnEvent(t.Context(), pipeline.StepStartedEvent{BaseEvent: base("p", "s", "step", fixedTime)})

	require.Error(t, obs.Err())
	assert.Contains(t, obs.Err().Error(), "disk full")
}

func TestObserver_ErrStopsWritingAfterFailure(t *testing.T) {
	t.Parallel()

	obs := jsonobs.New(failWriter{})
	ctx := t.Context()

	obs.OnEvent(ctx, pipeline.StepStartedEvent{BaseEvent: base("p", "s", "step", fixedTime)})
	obs.OnEvent(ctx, pipeline.StepPassedEvent{BaseEvent: base("p", "s", "step", fixedTime)})

	// Error is sticky — still the first one.
	require.Error(t, obs.Err())
	assert.Contains(t, obs.Err().Error(), "disk full")
}

// -------------------------------------------------------------------------
// Helpers
// -------------------------------------------------------------------------

func base(p, s, step string, t time.Time) pipeline.BaseEvent {
	return pipeline.BaseEvent{
		Location:  pipeline.Location{Pipeline: p, Stage: s, Step: step},
		Timestamp: t,
	}
}

func parseLine(t *testing.T, raw string) map[string]any {
	t.Helper()

	line := strings.TrimSpace(raw)

	var m map[string]any

	require.NoError(t, json.Unmarshal([]byte(line), &m))

	return m
}

func parseTypes(t *testing.T, lines []string) []string {
	t.Helper()

	types := make([]string, len(lines))

	for i, line := range lines {
		m := parseLine(t, line)
		typ, ok := m["type"].(string)
		require.True(t, ok, "missing type field in line %d", i)

		types[i] = typ
	}

	return types
}

func nonEmptyLines(s string) []string {
	var out []string

	for line := range strings.SplitSeq(s, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}

	return out
}

// Compile-time interface check.
var _ pipeline.Observer = (*jsonobs.Observer)(nil)
