package pipeline

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Executor runs a Pipeline, emitting events to its registered observers.
// Observer calls are serialised with a mutex so observers do not need their
// own synchronisation.
type Executor struct {
	observers []Observer
}

// NewExecutor returns an Executor that broadcasts events to the given observers.
func NewExecutor(observers ...Observer) *Executor {
	return &Executor{observers: observers}
}

// Run validates and executes the pipeline. It returns the first error
// encountered, or nil if all stages pass.
func (e *Executor) Run(ctx context.Context, p Pipeline) error {
	if err := e.validate(p); err != nil {
		return err
	}

	return e.runPipeline(ctx, p)
}

func (e *Executor) runPipeline(ctx context.Context, p Pipeline) error {
	now := time.Now()

	e.emit(ctx, newPipelineStartedEvent(p, now))

	for _, stage := range p.Stages {
		loc := Location{Pipeline: p.Name, Stage: stage.Name}

		if err := e.runStage(ctx, loc, stage); err != nil {
			e.emit(ctx, newPipelineFailedEvent(p.Name, err, time.Now()))

			return err
		}
	}

	e.emit(ctx, newPipelinePassedEvent(p.Name, time.Now()))

	return nil
}

func (e *Executor) runStage(ctx context.Context, loc Location, s Stage) error {
	if s.Condition != nil {
		if reason := s.Condition(ctx); reason != "" {
			e.emit(ctx, newStageSkippedEvent(loc, reason, time.Now()))

			return nil
		}
	}

	e.emit(ctx, newStageStartedEvent(loc, time.Now()))

	for _, step := range s.Steps {
		stepLoc := Location{Pipeline: loc.Pipeline, Stage: loc.Stage, Step: step.Name}

		if err := e.runStep(ctx, stepLoc, step); err != nil {
			e.emit(ctx, newStageFailedEvent(loc, err, time.Now()))

			return err
		}
	}

	e.emit(ctx, newStagePassedEvent(loc, time.Now()))

	return nil
}

func (e *Executor) runStep(ctx context.Context, loc Location, s Step) error {
	if s.Condition != nil {
		if reason := s.Condition(ctx); reason != "" {
			e.emit(ctx, newStepSkippedEvent(loc, reason, time.Now()))

			return nil
		}
	}

	e.emit(ctx, newStepStartedEvent(loc, time.Now()))

	if err := s.Run(ctx); err != nil {
		e.emit(ctx, newStepFailedEvent(loc, err, time.Now()))

		return err
	}

	e.emit(ctx, newStepPassedEvent(loc, time.Now()))

	return nil
}

func (e *Executor) emit(ctx context.Context, event Event) {
	for _, o := range e.observers {
		o.OnEvent(ctx, event)
	}
}

var (
	errPipelineNameEmpty = errors.New("pipeline name cannot be empty")
	errStageNameEmpty    = errors.New("stage name cannot be empty")
	errStepNameEmpty     = errors.New("step name cannot be empty")
	errStepRunNil        = errors.New("step run cannot be nil")
)

func (e *Executor) validate(p Pipeline) error {
	if p.Name == "" {
		return errPipelineNameEmpty
	}

	for i, s := range p.Stages {
		if s.Name == "" {
			return fmt.Errorf("stage[%d]: %w", i, errStageNameEmpty)
		}

		for ii, t := range s.Steps {
			if t.Name == "" {
				return fmt.Errorf("stage %q step[%d]: %w", s.Name, ii, errStepNameEmpty)
			}

			if t.Run == nil {
				return fmt.Errorf("stage %q step %q: %w", s.Name, t.Name, errStepRunNil)
			}
		}
	}

	return nil
}
