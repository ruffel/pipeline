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
	Group           string      // Optional; UI only grouping.
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

// Sentinel errors for flow control. Steps return these (optionally wrapped
// with additional context via fmt.Errorf) to trigger early exits without
// signalling failure.
var (
	// ErrSkipPipeline causes the executor to stop all remaining stages and
	// complete the pipeline successfully.
	ErrSkipPipeline = errors.New("skip pipeline")

	// ErrSkipStage causes the executor to skip the remaining steps in the
	// current stage and continue with the next stage.
	ErrSkipStage = errors.New("skip stage")

	// ErrSkipStep causes the executor to mark the current step as skipped
	// and continue with the next step.
	ErrSkipStep = errors.New("skip step")
)
