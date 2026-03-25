package pipeline_test

import (
	"context"
	"testing"
	"time"

	"github.com/ruffel/pipeline"
	"github.com/stretchr/testify/assert"
)

func TestBuilders(t *testing.T) {
	t.Parallel()

	runFast := func(_ context.Context) error { return nil }
	runSlow := func(_ context.Context) error { return nil }
	skipCond := func(_ context.Context) string { return "skipped" }

	p := pipeline.NewPipeline("deploy",
		pipeline.NewStage("preflight",
			pipeline.NewStep("check", runFast),
		).WithCondition(skipCond),

		pipeline.NewParallelStage("build",
			pipeline.NewStep("api", runFast),
			pipeline.NewStep("worker", runSlow).WithCondition(skipCond),
		).WithContinueOnError(true),
	)

	assert.Equal(t, "deploy", p.Name)
	assert.Len(t, p.Stages, 2)

	// Stage 1: preflight
	s1 := p.Stages[0]
	assert.Equal(t, "preflight", s1.Name)
	assert.False(t, s1.Parallel)
	assert.NotNil(t, s1.Condition)
	assert.Len(t, s1.Steps, 1)

	// Stage 2: build
	s2 := p.Stages[1]
	assert.Equal(t, "build", s2.Name)
	assert.True(t, s2.Parallel)
	assert.True(t, s2.ContinueOnError)
	assert.Nil(t, s2.Condition)
	assert.Len(t, s2.Steps, 2)

	// Verify step contents (worker)
	assert.Equal(t, "worker", s2.Steps[1].Name)
	assert.NotNil(t, s2.Steps[1].Condition)
}

func TestStep_FluentMiddleware(t *testing.T) {
	t.Parallel()

	runFast := func(_ context.Context) error { return nil }

	step := pipeline.NewStep("deploy", runFast).
		WithTimeout(30*time.Second).
		WithRetry(3, time.Second)

	assert.Equal(t, "deploy", step.Name)
	assert.NotNil(t, step.Run) // Run func is wrapped
}
