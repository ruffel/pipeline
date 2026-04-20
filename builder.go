package pipeline

import "time"

// NewPipeline creates a new [Pipeline] with the given name and sequential stages.
func NewPipeline(name string, stages ...Stage) Pipeline {
	return Pipeline{
		Name:   name,
		Stages: stages,
	}
}

// NewStage creates a sequential [Stage] with the given name and steps.
func NewStage(name string, steps ...Step) Stage {
	return Stage{
		Name:  name,
		Steps: steps,
	}
}

// NewParallelStage creates a parallel [Stage] with the given name and steps.
func NewParallelStage(name string, steps ...Step) Stage {
	return Stage{
		Name:     name,
		Parallel: true,
		Steps:    steps,
	}
}

// WithCondition sets the conditional execution function for the stage.
func (s Stage) WithCondition(cond ConditionFn) Stage {
	s.Condition = cond

	return s
}

// WithContinueOnError sets whether parallel execution should continue if a
// step fails. Has no effect on sequential stages.
func (s Stage) WithContinueOnError(continueOnError bool) Stage {
	s.ContinueOnError = continueOnError

	return s
}

// NewStep creates a [Step] with the given name and run function.
func NewStep(name string, run StepFn) Step {
	return Step{
		Name: name,
		Run:  run,
	}
}

// WithCondition sets the conditional execution function for the step.
func (s Step) WithCondition(cond ConditionFn) Step {
	s.Condition = cond

	return s
}

// WithTimeout wraps the step's current Run function with a deadline.
// Chaining order matters: placing WithTimeout outside WithRetry caps the whole
// retry loop, while placing it before WithRetry applies the deadline to each
// retry attempt.
func (s Step) WithTimeout(d time.Duration) Step {
	s.Run = WithTimeout(d, s.Run)

	return s
}

// WithRetry wraps the step's Run function to retry on failure up to maxAttempts
// times with a fixed backoff delay between attempts.
func (s Step) WithRetry(maxAttempts int, backoff time.Duration) Step {
	s.Run = WithRetry(maxAttempts, backoff, s.Run)

	return s
}
