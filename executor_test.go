package pipeline_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ruffel/pipeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// -----------------------------------------------------------------------------
// Sequential happy path
// -----------------------------------------------------------------------------

func TestExecutor_HappyPath(t *testing.T) {
	t.Parallel()

	obs := &recordingObserver{}
	ex := pipeline.NewExecutor(obs)

	err := ex.Run(t.Context(), pipeline.Pipeline{
		Name: "deploy",
		Stages: []pipeline.Stage{
			{
				Name: "build",
				Steps: []pipeline.Step{
					{Name: "compile", Run: noop},
					{Name: "test", Run: noop},
				},
			},
			{
				Name: "release",
				Steps: []pipeline.Step{
					{Name: "publish", Run: noop},
				},
			},
		},
	})
	require.NoError(t, err)

	assert.Equal(t, []string{
		"pipeline.PipelineStartedEvent",
		"pipeline.StageStartedEvent",
		"pipeline.StepStartedEvent",
		"pipeline.StepPassedEvent",
		"pipeline.StepStartedEvent",
		"pipeline.StepPassedEvent",
		"pipeline.StagePassedEvent",
		"pipeline.StageStartedEvent",
		"pipeline.StepStartedEvent",
		"pipeline.StepPassedEvent",
		"pipeline.StagePassedEvent",
		"pipeline.PipelinePassedEvent",
	}, obs.eventTypes())
}

// -----------------------------------------------------------------------------
// Step failure
// -----------------------------------------------------------------------------

func TestExecutor_StepFailure(t *testing.T) {
	t.Parallel()

	obs := &recordingObserver{}
	ex := pipeline.NewExecutor(obs)
	sentinel := errors.New("build failed")

	err := ex.Run(t.Context(), pipeline.Pipeline{
		Name: "deploy",
		Stages: []pipeline.Stage{
			{
				Name: "build",
				Steps: []pipeline.Step{
					{Name: "compile", Run: func(_ context.Context) error { return sentinel }},
					{Name: "should-not-run", Run: noop},
				},
			},
		},
	})
	require.ErrorIs(t, err, sentinel)

	assert.Equal(t, []string{
		"pipeline.PipelineStartedEvent",
		"pipeline.StageStartedEvent",
		"pipeline.StepStartedEvent",
		"pipeline.StepFailedEvent",
		"pipeline.StageFailedEvent",
		"pipeline.PipelineFailedEvent",
	}, obs.eventTypes())
}

// -----------------------------------------------------------------------------
// Sentinel errors
// -----------------------------------------------------------------------------

func TestExecutor_ErrSkipPipeline(t *testing.T) {
	t.Parallel()

	obs := &recordingObserver{}
	ex := pipeline.NewExecutor(obs)

	err := ex.Run(t.Context(), pipeline.Pipeline{
		Name: "deploy",
		Stages: []pipeline.Stage{
			{
				Name: "check",
				Steps: []pipeline.Step{
					{Name: "already-running", Run: func(_ context.Context) error {
						return fmt.Errorf("cluster active: %w", pipeline.ErrSkipPipeline)
					}},
				},
			},
			{
				Name:  "should-not-run",
				Steps: []pipeline.Step{{Name: "noop", Run: noop}},
			},
		},
	})

	// ErrSkipPipeline is a clean exit — no error returned.
	require.NoError(t, err)

	// Sentinel returns produce StepPassedEvent — the step ran and resolved.
	types := obs.eventTypes()
	assert.NotContains(t, types, "pipeline.StepSkippedEvent")
	assert.NotContains(t, types, "pipeline.StepFailedEvent")
	assert.NotContains(t, types, "pipeline.StageFailedEvent")
	assert.Contains(t, types, "pipeline.PipelinePassedEvent")
}

func TestExecutor_ErrSkipStage(t *testing.T) {
	t.Parallel()

	obs := &recordingObserver{}
	ex := pipeline.NewExecutor(obs)

	err := ex.Run(t.Context(), pipeline.Pipeline{
		Name: "deploy",
		Stages: []pipeline.Stage{
			{
				Name: "check",
				Steps: []pipeline.Step{
					{Name: "first", Run: noop},
					{Name: "skip-rest", Run: func(_ context.Context) error { return pipeline.ErrSkipStage }},
					{Name: "should-not-run", Run: noop},
				},
			},
			{
				Name:  "next-stage",
				Steps: []pipeline.Step{{Name: "runs", Run: noop}},
			},
		},
	})
	require.NoError(t, err)

	// Sentinel returns produce StepPassedEvent — the step ran and resolved.
	types := obs.eventTypes()
	assert.NotContains(t, types, "pipeline.StepSkippedEvent")
	assert.NotContains(t, types, "pipeline.StepFailedEvent")
	assert.NotContains(t, types, "pipeline.StageFailedEvent")
	assert.Contains(t, types, "pipeline.PipelinePassedEvent")
}

func TestExecutor_StepReturnsNilEarly(t *testing.T) {
	t.Parallel()

	obs := &recordingObserver{}
	ex := pipeline.NewExecutor(obs)

	err := ex.Run(t.Context(), pipeline.Pipeline{
		Name: "deploy",
		Stages: []pipeline.Stage{
			{
				Name: "check",
				Steps: []pipeline.Step{
					{Name: "skip-me", Run: func(ctx context.Context) error {
						pipeline.EmitInfo(ctx, "already done")

						return nil
					}},
					{Name: "still-runs", Run: noop},
				},
			},
		},
	})
	require.NoError(t, err)

	assert.Equal(t, []string{
		"pipeline.PipelineStartedEvent",
		"pipeline.StageStartedEvent",
		"pipeline.StepStartedEvent",
		"pipeline.MessageEvent", // EmitInfo
		"pipeline.StepPassedEvent",
		"pipeline.StepStartedEvent",
		"pipeline.StepPassedEvent",
		"pipeline.StagePassedEvent",
		"pipeline.PipelinePassedEvent",
	}, obs.eventTypes())
}

// -----------------------------------------------------------------------------
// Conditional skip
// -----------------------------------------------------------------------------

func TestExecutor_StageConditionSkip(t *testing.T) {
	t.Parallel()

	obs := &recordingObserver{}
	ex := pipeline.NewExecutor(obs)

	err := ex.Run(t.Context(), pipeline.Pipeline{
		Name: "deploy",
		Stages: []pipeline.Stage{
			{
				Name:      "optional",
				Condition: func(_ context.Context) string { return "not needed" },
				Steps:     []pipeline.Step{{Name: "noop", Run: noop}},
			},
		},
	})
	require.NoError(t, err)

	assert.Equal(t, []string{
		"pipeline.PipelineStartedEvent",
		"pipeline.StageSkippedEvent",
		"pipeline.PipelinePassedEvent",
	}, obs.eventTypes())

	skipped, ok := obs.events[1].(pipeline.StageSkippedEvent)
	require.True(t, ok)
	assert.Equal(t, "not needed", skipped.Reason)
}

func TestExecutor_StepConditionSkip(t *testing.T) {
	t.Parallel()

	obs := &recordingObserver{}
	ex := pipeline.NewExecutor(obs)

	err := ex.Run(t.Context(), pipeline.Pipeline{
		Name: "deploy",
		Stages: []pipeline.Stage{
			{
				Name: "build",
				Steps: []pipeline.Step{
					{
						Name:      "optional-lint",
						Condition: func(_ context.Context) string { return "CI only" },
						Run:       noop,
					},
					{Name: "compile", Run: noop},
				},
			},
		},
	})
	require.NoError(t, err)

	assert.Equal(t, []string{
		"pipeline.PipelineStartedEvent",
		"pipeline.StageStartedEvent",
		"pipeline.StepSkippedEvent",
		"pipeline.StepStartedEvent",
		"pipeline.StepPassedEvent",
		"pipeline.StagePassedEvent",
		"pipeline.PipelinePassedEvent",
	}, obs.eventTypes())
}

func TestExecutor_ConditionReturnsEmpty_StageRuns(t *testing.T) {
	t.Parallel()

	obs := &recordingObserver{}
	ex := pipeline.NewExecutor(obs)

	err := ex.Run(t.Context(), pipeline.Pipeline{
		Name: "deploy",
		Stages: []pipeline.Stage{
			{
				Name:      "conditional",
				Condition: func(_ context.Context) string { return "" },
				Steps:     []pipeline.Step{{Name: "runs", Run: noop}},
			},
		},
	})
	require.NoError(t, err)

	types := obs.eventTypes()
	assert.Contains(t, types, "pipeline.StageStartedEvent")
	assert.Contains(t, types, "pipeline.StepPassedEvent")
}

// -----------------------------------------------------------------------------
// Context cancellation
// -----------------------------------------------------------------------------

func TestExecutor_ContextCancelled(t *testing.T) {
	t.Parallel()

	obs := &recordingObserver{}
	ex := pipeline.NewExecutor(obs)

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // Cancel before run.

	err := ex.Run(ctx, pipeline.Pipeline{
		Name: "deploy",
		Stages: []pipeline.Stage{
			{
				Name:  "should-not-run",
				Steps: []pipeline.Step{{Name: "noop", Run: noop}},
			},
		},
	})

	require.ErrorIs(t, err, context.Canceled)
}

// -----------------------------------------------------------------------------
// Panic recovery
// -----------------------------------------------------------------------------

func TestExecutor_PanicRecovery(t *testing.T) {
	t.Parallel()

	obs := &recordingObserver{}
	ex := pipeline.NewExecutor(obs)

	err := ex.Run(t.Context(), pipeline.Pipeline{
		Name: "deploy",
		Stages: []pipeline.Stage{
			{
				Name: "build",
				Steps: []pipeline.Step{
					{Name: "panic-step", Run: func(_ context.Context) error {
						panic("unexpected nil pointer")
					}},
				},
			},
		},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "panic in step")
	assert.Contains(t, err.Error(), "unexpected nil pointer")

	last := obs.eventTypes()[len(obs.eventTypes())-1]
	assert.Equal(t, "pipeline.PipelineFailedEvent", last)
}

// -----------------------------------------------------------------------------
// Parallel execution
// -----------------------------------------------------------------------------

func TestExecutor_Parallel(t *testing.T) {
	t.Parallel()

	obs := &recordingObserver{}
	ex := pipeline.NewExecutor(obs)

	// Use a barrier to prove steps run concurrently.
	var ready sync.WaitGroup

	ready.Add(2)

	step := func(_ context.Context) error {
		ready.Done() // Signal this step has started.
		ready.Wait() // Wait for the other to start too.

		return nil
	}

	err := ex.Run(t.Context(), pipeline.Pipeline{
		Name: "deploy",
		Stages: []pipeline.Stage{
			{
				Name:     "parallel-stage",
				Parallel: true,
				Steps: []pipeline.Step{
					{Name: "a", Run: step},
					{Name: "b", Run: step},
				},
			},
		},
	})
	require.NoError(t, err)
}

func TestExecutor_ParallelFailFast(t *testing.T) {
	t.Parallel()

	obs := &recordingObserver{}
	ex := pipeline.NewExecutor(obs)
	sentinel := errors.New("step failed")

	err := ex.Run(t.Context(), pipeline.Pipeline{
		Name: "deploy",
		Stages: []pipeline.Stage{
			{
				Name:     "parallel-stage",
				Parallel: true,
				Steps: []pipeline.Step{
					{Name: "fail", Run: func(_ context.Context) error { return sentinel }},
					{Name: "noop", Run: noop},
				},
			},
		},
	})

	require.ErrorIs(t, err, sentinel)
}

func TestExecutor_ParallelFailFast_SkipStageAbsorbed(t *testing.T) {
	t.Parallel()

	obs := &recordingObserver{}
	ex := pipeline.NewExecutor(obs)

	err := ex.Run(t.Context(), pipeline.Pipeline{
		Name: "deploy",
		Stages: []pipeline.Stage{
			{
				Name:     "build",
				Parallel: true,
				Steps: []pipeline.Step{
					{Name: "skip-stage", Run: func(_ context.Context) error { return pipeline.ErrSkipStage }},
					{Name: "slow-peer", Run: func(ctx context.Context) error {
						// A sibling that takes longer than the sentinel.
						// Without the fix, this would get context.Canceled
						// and emit StepFailedEvent.
						select {
						case <-time.After(50 * time.Millisecond):
							return nil
						case <-ctx.Done():
							return ctx.Err()
						}
					}},
				},
			},
		},
	})

	require.NoError(t, err)

	types := obs.eventTypes()
	assert.Contains(t, types, "pipeline.StagePassedEvent")
	assert.NotContains(t, types, "pipeline.StepFailedEvent")
}

func TestExecutor_ParallelFailFast_SkipPipelinePropagates(t *testing.T) {
	t.Parallel()

	obs := &recordingObserver{}
	ex := pipeline.NewExecutor(obs)

	err := ex.Run(t.Context(), pipeline.Pipeline{
		Name: "deploy",
		Stages: []pipeline.Stage{
			{
				Name:     "check",
				Parallel: true,
				Steps: []pipeline.Step{
					{Name: "skip-pipe", Run: func(_ context.Context) error { return pipeline.ErrSkipPipeline }},
				},
			},
			{
				Name:  "should-not-run",
				Steps: []pipeline.Step{{Name: "x", Run: noop}},
			},
		},
	})

	// Pipeline passes cleanly — skip is honoured.
	require.NoError(t, err)

	for _, ev := range obs.events {
		if e, ok := ev.(pipeline.StageStartedEvent); ok {
			assert.NotEqual(t, "should-not-run", e.Stage, "skipped stage should not have started")
		}
	}
}

// -----------------------------------------------------------------------------
// ContinueOnError (best-effort parallel)
// -----------------------------------------------------------------------------

func TestExecutor_ContinueOnError(t *testing.T) {
	t.Parallel()

	obs := &recordingObserver{}
	ex := pipeline.NewExecutor(obs)

	errA := errors.New("error a")
	errB := errors.New("error b")

	var ran atomic.Bool

	err := ex.Run(t.Context(), pipeline.Pipeline{
		Name: "deploy",
		Stages: []pipeline.Stage{
			{
				Name:            "cleanup",
				Parallel:        true,
				ContinueOnError: true,
				Steps: []pipeline.Step{
					{Name: "fail-a", Run: func(_ context.Context) error { return errA }},
					{Name: "fail-b", Run: func(_ context.Context) error { return errB }},
					{Name: "success", Run: func(_ context.Context) error {
						ran.Store(true)

						return nil
					}},
				},
			},
		},
	})

	// All steps ran — even the success step.
	assert.True(t, ran.Load(), "success step should have run despite other failures")

	// Both errors are joined.
	require.Error(t, err)
	require.ErrorIs(t, err, errA)
	require.ErrorIs(t, err, errB)
}

func TestExecutor_ContinueOnError_PreservesRealErrorsWithSkipPipeline(t *testing.T) {
	t.Parallel()

	obs := &recordingObserver{}
	ex := pipeline.NewExecutor(obs)
	realErr := errors.New("deploy failed")

	err := ex.Run(t.Context(), pipeline.Pipeline{
		Name: "deploy",
		Stages: []pipeline.Stage{
			{
				Name:            "cleanup",
				Parallel:        true,
				ContinueOnError: true,
				Steps: []pipeline.Step{
					{Name: "real-fail", Run: func(_ context.Context) error { return realErr }},
					{Name: "skip-pipe", Run: func(_ context.Context) error { return pipeline.ErrSkipPipeline }},
				},
			},
		},
	})

	// Real failure must NOT be silently discarded.
	require.Error(t, err)
	require.ErrorIs(t, err, realErr)
	assert.NotErrorIs(t, err, pipeline.ErrSkipPipeline, "skip should be filtered out")
}

func TestExecutor_ContinueOnError_SkipPipelineAloneIsClean(t *testing.T) {
	t.Parallel()

	obs := &recordingObserver{}
	ex := pipeline.NewExecutor(obs)

	err := ex.Run(t.Context(), pipeline.Pipeline{
		Name: "deploy",
		Stages: []pipeline.Stage{
			{
				Name:            "check",
				Parallel:        true,
				ContinueOnError: true,
				Steps: []pipeline.Step{
					{Name: "skip-pipe", Run: func(_ context.Context) error { return pipeline.ErrSkipPipeline }},
					{Name: "noop", Run: func(_ context.Context) error { return nil }},
				},
			},
			{
				Name:  "should-not-run",
				Steps: []pipeline.Step{{Name: "x", Run: func(_ context.Context) error { return nil }}},
			},
		},
	})

	// Pipeline should pass cleanly — no real failures, skip is honoured.
	require.NoError(t, err)

	for _, ev := range obs.events {
		if e, ok := ev.(pipeline.StageStartedEvent); ok {
			assert.NotEqual(t, "should-not-run", e.Stage, "skipped stage should not have started")
		}
	}

	types := obs.eventTypes()
	assert.Contains(t, types, "pipeline.PipelinePassedEvent")
}

func TestExecutor_ContinueOnError_SkipStageAbsorbed(t *testing.T) {
	t.Parallel()

	obs := &recordingObserver{}
	ex := pipeline.NewExecutor(obs)

	err := ex.Run(t.Context(), pipeline.Pipeline{
		Name: "deploy",
		Stages: []pipeline.Stage{
			{
				Name:            "cleanup",
				Parallel:        true,
				ContinueOnError: true,
				Steps: []pipeline.Step{
					{Name: "skip-stage", Run: func(_ context.Context) error { return pipeline.ErrSkipStage }},
					{Name: "noop", Run: func(_ context.Context) error { return nil }},
				},
			},
		},
	})

	// ErrSkipStage should be absorbed — stage passes.
	require.NoError(t, err)

	types := obs.eventTypes()
	assert.Contains(t, types, "pipeline.PipelinePassedEvent")
}

// -----------------------------------------------------------------------------
// Emitter wiring
// -----------------------------------------------------------------------------

func TestExecutor_EmitterInStepContext(t *testing.T) {
	t.Parallel()

	obs := &recordingObserver{}
	ex := pipeline.NewExecutor(obs)

	err := ex.Run(t.Context(), pipeline.Pipeline{
		Name: "deploy",
		Stages: []pipeline.Stage{
			{
				Name: "build",
				Steps: []pipeline.Step{
					{Name: "compile", Run: func(ctx context.Context) error {
						pipeline.EmitInfo(ctx, "compiling")

						return nil
					}},
				},
			},
		},
	})
	require.NoError(t, err)

	// Should see a MessageEvent from EmitInfo between StepStarted and StepPassed.
	types := obs.eventTypes()
	assert.Contains(t, types, "pipeline.MessageEvent")
}

func TestExecutor_EmitterLocationMatchesStep(t *testing.T) {
	t.Parallel()

	obs := &recordingObserver{}
	ex := pipeline.NewExecutor(obs)

	err := ex.Run(t.Context(), pipeline.Pipeline{
		Name: "deploy",
		Stages: []pipeline.Stage{
			{
				Name: "build",
				Steps: []pipeline.Step{
					{Name: "compile", Run: func(ctx context.Context) error {
						pipeline.EmitInfo(ctx, "compiling")

						return nil
					}},
				},
			},
		},
	})
	require.NoError(t, err)

	for _, ev := range obs.events {
		if msg, ok := ev.(pipeline.MessageEvent); ok {
			assert.Equal(t, "deploy", msg.Pipeline)
			assert.Equal(t, "build", msg.Stage)
			assert.Equal(t, "compile", msg.Step)
		}
	}
}

// -----------------------------------------------------------------------------
// Validation
// -----------------------------------------------------------------------------

func TestExecutor_Validation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		p       pipeline.Pipeline
		wantErr string
	}{
		{
			name:    "empty pipeline name",
			p:       pipeline.Pipeline{},
			wantErr: "pipeline name cannot be empty",
		},
		{
			name:    "no stages",
			p:       pipeline.Pipeline{Name: "p"},
			wantErr: "pipeline must have at least one stage",
		},
		{
			name: "empty stage name",
			p: pipeline.Pipeline{
				Name:   "p",
				Stages: []pipeline.Stage{{Steps: []pipeline.Step{{Name: "s", Run: noop}}}},
			},
			wantErr: "stage[0]: name cannot be empty",
		},
		{
			name: "empty step name",
			p: pipeline.Pipeline{
				Name:   "p",
				Stages: []pipeline.Stage{{Name: "s", Steps: []pipeline.Step{{Run: noop}}}},
			},
			wantErr: "step[0]: name cannot be empty",
		},
		{
			name: "nil step run",
			p: pipeline.Pipeline{
				Name:   "p",
				Stages: []pipeline.Stage{{Name: "s", Steps: []pipeline.Step{{Name: "step"}}}},
			},
			wantErr: "run function cannot be nil",
		},
		{
			name: "no steps",
			p: pipeline.Pipeline{
				Name:   "p",
				Stages: []pipeline.Stage{{Name: "empty"}},
			},
			wantErr: "must have at least one step",
		},
		{
			name: "duplicate stage name",
			p: pipeline.Pipeline{
				Name: "p",
				Stages: []pipeline.Stage{
					{Name: "build", Steps: []pipeline.Step{{Name: "a", Run: noop}}},
					{Name: "build", Steps: []pipeline.Step{{Name: "b", Run: noop}}},
				},
			},
			wantErr: `duplicate stage name "build"`,
		},
		{
			name: "duplicate step name",
			p: pipeline.Pipeline{
				Name: "p",
				Stages: []pipeline.Stage{
					{Name: "s", Steps: []pipeline.Step{
						{Name: "compile", Run: noop},
						{Name: "compile", Run: noop},
					}},
				},
			},
			wantErr: `duplicate step name "compile"`,
		},
		{
			name: "multiple errors aggregated",
			p: pipeline.Pipeline{
				Name: "p",
				Stages: []pipeline.Stage{
					{Steps: []pipeline.Step{{Run: noop}}},
				},
			},
			wantErr: "name cannot be empty",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ex := pipeline.NewExecutor()
			err := ex.Run(t.Context(), tt.p)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// -----------------------------------------------------------------------------
// Panic in parallel
// -----------------------------------------------------------------------------

func TestExecutor_PanicInParallel(t *testing.T) {
	t.Parallel()

	ex := pipeline.NewExecutor()

	err := ex.Run(t.Context(), pipeline.Pipeline{
		Name: "deploy",
		Stages: []pipeline.Stage{
			{
				Name:     "parallel",
				Parallel: true,
				Steps: []pipeline.Step{
					{Name: "panic-step", Run: func(_ context.Context) error {
						panic("boom")
					}},
					{Name: "noop", Run: noop},
				},
			},
		},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "panic in step")
}
