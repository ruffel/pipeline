package plain_test

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/ruffel/pipeline"
	"github.com/ruffel/pipeline/observers/plain"
	"github.com/stretchr/testify/assert"
)

var fixedTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) //nolint:gochecknoglobals

func TestObserver_HappyPath(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	obs := plain.New(&buf)
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

	output := buf.String()
	assert.Contains(t, output, "▶ Pipeline: deploy")
	assert.Contains(t, output, "▶ Stage: build")
	assert.Contains(t, output, "✓ compile (1s)")
	assert.Contains(t, output, "✓ Stage: build (1s)")
	assert.Contains(t, output, "✓ Pipeline: deploy (1s)")
}

func TestObserver_StepFailure(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	obs := plain.New(&buf)
	ctx := t.Context()
	sentinel := errors.New("exit status 1")

	events := []pipeline.Event{
		pipeline.PipelineStartedEvent{
			BaseEvent:  base("deploy", "", "", fixedTime),
			Definition: pipeline.Pipeline{Name: "deploy"},
		},
		pipeline.StageStartedEvent{BaseEvent: base("deploy", "build", "", fixedTime)},
		pipeline.StepStartedEvent{BaseEvent: base("deploy", "build", "lint", fixedTime)},
		pipeline.StepFailedEvent{BaseEvent: base("deploy", "build", "lint", fixedTime.Add(time.Second)), Err: sentinel},
		pipeline.StageFailedEvent{BaseEvent: base("deploy", "build", "", fixedTime.Add(time.Second)), Err: sentinel},
		pipeline.PipelineFailedEvent{BaseEvent: base("deploy", "", "", fixedTime.Add(time.Second)), Err: sentinel},
	}

	for _, e := range events {
		obs.OnEvent(ctx, e)
	}

	output := buf.String()
	assert.Contains(t, output, "✗ lint — exit status 1")
	assert.Contains(t, output, "✗ Stage: build — exit status 1")
	assert.Contains(t, output, "✗ Pipeline: deploy — exit status 1")
}

func TestObserver_StageSkip(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	obs := plain.New(&buf)

	obs.OnEvent(t.Context(), pipeline.StageSkippedEvent{
		BaseEvent: base("deploy", "optional", "", fixedTime),
		Reason:    "not needed",
	})

	assert.Contains(t, buf.String(), "⊘ Stage: optional — not needed")
}

func TestObserver_StepSkip(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	obs := plain.New(&buf)

	obs.OnEvent(t.Context(), pipeline.StepSkippedEvent{
		BaseEvent: base("deploy", "build", "lint", fixedTime),
		Reason:    "CI only",
	})

	assert.Contains(t, buf.String(), "⊘ lint — CI only")
}

func TestObserver_MessageLevels(t *testing.T) {
	t.Parallel()

	cases := []struct {
		level pipeline.MessageLevel
		label string
	}{
		{pipeline.LevelInfo, "INFO"},
		{pipeline.LevelWarn, "WARN"},
		{pipeline.LevelDebug, "DBUG"},
	}

	for _, tt := range cases {
		var buf bytes.Buffer

		obs := plain.New(&buf)

		obs.OnEvent(t.Context(), pipeline.MessageEvent{
			BaseEvent: base("p", "s", "step", fixedTime),
			Level:     tt.level,
			Message:   "hello",
		})

		assert.Contains(t, buf.String(), "["+tt.label+"] hello")
	}
}

func TestObserver_Output(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	obs := plain.New(&buf)

	obs.OnEvent(t.Context(), pipeline.OutputEvent{
		BaseEvent: base("p", "s", "step", fixedTime),
		Stream:    pipeline.Stdout,
		Line:      "building...",
	})

	assert.Contains(t, buf.String(), "│ building...")
}

func TestObserver_Progress(t *testing.T) {
	t.Parallel()

	t.Run("determinate", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer

		obs := plain.New(&buf)

		obs.OnEvent(t.Context(), pipeline.ProgressEvent{
			BaseEvent: base("p", "s", "step", fixedTime),
			Message:   "uploading",
			Current:   3,
			Total:     10,
		})

		assert.Contains(t, buf.String(), "uploading (3/10)")
	})

	t.Run("indeterminate", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer

		obs := plain.New(&buf)

		obs.OnEvent(t.Context(), pipeline.ProgressEvent{
			BaseEvent: base("p", "s", "step", fixedTime),
			Message:   "waiting",
		})

		output := buf.String()
		assert.Contains(t, output, "waiting")
		assert.NotContains(t, output, "(0/0)")
	})
}

func TestObserver_Custom(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	obs := plain.New(&buf)

	obs.OnEvent(t.Context(), pipeline.CustomEvent{
		BaseEvent: base("p", "s", "step", fixedTime),
		Type:      "deploy.url",
		Data:      "https://example.com",
	})

	assert.Contains(t, buf.String(), "[deploy.url] https://example.com")
}

// base is a helper that constructs a BaseEvent with the given location and time.
func base(p, s, step string, t time.Time) pipeline.BaseEvent {
	return pipeline.BaseEvent{
		Location:  pipeline.Location{Pipeline: p, Stage: s, Step: step},
		Timestamp: t,
	}
}

// Verify the observer satisfies the interface at compile time.
var _ pipeline.Observer = (*plain.Observer)(nil)
