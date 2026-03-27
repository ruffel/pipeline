package pipeline

import (
	"context"
	"errors"
)

// StepFn is the function signature for step execution. Steps receive a context
// that may carry an Emitter for mid-execution event emission.
type StepFn func(ctx context.Context) error

// ConditionFn is the function signature for condition checking. A non-empty
// return value causes the executor to skip the stage or step with that reason.
type ConditionFn func(ctx context.Context) string

// Pipeline represents a sequence of stages to be executed.
type Pipeline struct {
	Name   string
	Stages []Stage
}

// Stage represents a logical grouping of steps within a pipeline.
type Stage struct {
	Name            string      // Human-readable display name.
	Steps           []Step      // The steps that make up the stage.
	Parallel        bool        // When true, steps run concurrently.
	ContinueOnError bool        // When true, all steps run even if some fail; errors are joined.
	Condition       ConditionFn // Optional; non-empty return skips the stage with that reason.
}

// Step represents a unit of work to be executed within a stage.
type Step struct {
	Name      string      // Human-readable display name.
	Run       StepFn      // The function that performs the step's work.
	Condition ConditionFn // Optional; non-empty return skips the step with that reason.
}

// Sentinel errors for flow control. Steps return these to trigger early exits
// without signalling failure. The returning step always emits a
// [StepPassedEvent] because it ran and resolved; the sentinel controls what
// happens next. Use [EmitInfo] or a Condition to communicate the reason for
// skipping.
var (
	// ErrSkipPipeline causes the executor to stop all remaining stages and
	// complete the pipeline successfully. The current stage emits a
	// [StagePassedEvent] and the pipeline emits a [PipelinePassedEvent].
	ErrSkipPipeline = errors.New("skip pipeline")

	// ErrSkipStage causes the executor to skip the remaining steps in the
	// current stage and continue with the next stage. The stage emits a
	// [StagePassedEvent].
	ErrSkipStage = errors.New("skip stage")
)
