package terminal_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/ruffel/pipeline"
	termobs "github.com/ruffel/pipeline/observers/terminal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestObserver_Integration(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	obs := termobs.New(&buf)
	ex := pipeline.NewExecutor(obs)

	p := pipeline.NewPipeline("deploy",
		pipeline.NewStage("build",
			pipeline.NewStep("api", func(_ context.Context) error { return nil }),
		),
	)

	err := ex.Run(t.Context(), p)
	require.NoError(t, err)

	out := buf.String()

	// Just checking for some known styled outputs rather than exact byte matching
	// because lipgloss outputs raw ANSI escapes which can vary slightly by env.
	assert.Contains(t, out, "Pipeline: deploy")
	assert.Contains(t, out, "Stage: build")
	assert.Contains(t, out, "api") // step name
	assert.Contains(t, out, "Pipeline Complete")
}

func TestObserver_StageSkipped(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	obs := termobs.New(&buf)
	ex := pipeline.NewExecutor(obs)

	p := pipeline.NewPipeline("deploy",
		pipeline.NewStage("optional",
			pipeline.NewStep("noop", func(_ context.Context) error { return nil }),
		).WithCondition(func(_ context.Context) string { return "not needed" }),
		pipeline.NewStage("build",
			pipeline.NewStep("compile", func(_ context.Context) error { return nil }),
		),
	)

	err := ex.Run(t.Context(), p)
	require.NoError(t, err)

	out := buf.String()

	assert.Contains(t, out, "Stage: optional")
	assert.Contains(t, out, "not needed")
	assert.Contains(t, out, "Pipeline Complete")
}

// TestObserver_SameStepNameInDifferentStages is a regression test for the
// stepTimes collision bug: two steps with the same name in different stages
// must not share timer state.
func TestObserver_SameStepNameInDifferentStages(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	obs := termobs.New(&buf)
	ex := pipeline.NewExecutor(obs)

	p := pipeline.NewPipeline("deploy",
		pipeline.NewStage("stage-a",
			// Same step name as stage-b — passes so stage-b runs.
			pipeline.NewStep("validate", func(_ context.Context) error { return nil }),
		),
		pipeline.NewStage("stage-b",
			pipeline.NewStep("validate", func(_ context.Context) error { return errors.New("failure") }),
		),
	)

	_ = ex.Run(t.Context(), p)

	out := buf.String()

	// The failed-steps summary must include the stage prefix so the two
	// "validate" entries (one per stage) are distinguishable.
	assert.Contains(t, out, "[stage-b] validate - failure")

	// stage-a's step must have rendered with a duration, proving its timer
	// was not overwritten by stage-b's StepStartedEvent.
	assert.Contains(t, out, "✓ validate (")
}

func TestObserver_CustomEvent_DefaultNoOp(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	obs := termobs.New(&buf)

	obs.OnEvent(t.Context(), pipeline.PipelineStartedEvent{
		Definition: pipeline.Pipeline{Name: "p"},
	})

	before := buf.Len()

	obs.OnEvent(t.Context(), pipeline.CustomEvent{
		Type: "deploy.url-ready",
		Data: "https://example.com",
	})

	// Default formatter is a no-op — no new output should be written.
	assert.Equal(t, before, buf.Len())
}

func TestObserver_CustomEvent_WithOverride(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	opts := termobs.DefaultOptions()
	opts.FormatCustom = func(_ context.Context, e pipeline.CustomEvent, _ termobs.State, _ termobs.Palette) string {
		return "[" + e.Type + "] " + e.Data.(string)
	}

	obs := termobs.NewWithOptions(&buf, opts)

	obs.OnEvent(t.Context(), pipeline.PipelineStartedEvent{
		Definition: pipeline.Pipeline{Name: "p"},
	})
	obs.OnEvent(t.Context(), pipeline.CustomEvent{
		Type: "deploy.url-ready",
		Data: "https://example.com",
	})

	out := buf.String()
	assert.Contains(t, out, "[deploy.url-ready] https://example.com")
}
