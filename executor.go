package pipeline

import (
	"context"
	"errors"
	"fmt"
)

type Executor struct {
	observers []Observer
}

func NewExecutor(observers ...Observer) *Executor {
	return &Executor{observers: observers}
}

func (e *Executor) Run(ctx context.Context, p Pipeline) error {
	if err := e.validate(p); err != nil {
		return err
	}

	return e.runPipeline(ctx, p)
}

func (e *Executor) runPipeline(ctx context.Context, p Pipeline) error {
	e.emit(ctx, NewPipelineStartedEvent(p.Name))

	for _, stage := range p.Stages {
		location := NewLocation(p.Name, stage.Name, "")

		if err := e.runStage(ctx, location, stage); err != nil {
			e.emit(ctx, NewPipelineFailedEvent(p.Name, err))

			return err
		}
	}

	e.emit(ctx, NewPipelinePassedEvent(p.Name))

	return nil
}

func (e *Executor) runStage(ctx context.Context, l Location, s Stage) error {
	e.emit(ctx, NewStageStartedEvent(l))

	for _, step := range s.Steps {
		if err := e.runStep(ctx, l, step); err != nil {
			e.emit(ctx, NewStageFailedEvent(l, err))

			return err
		}
	}

	e.emit(ctx, NewStagePassedEvent(l))

	return nil
}

func (e *Executor) runStep(ctx context.Context, l Location, s Step) error {
	location := NewLocation(l.Pipeline, l.Stage, s.Name)

	e.emit(ctx, NewStepStartedEvent(location))

	if err := s.Run(ctx); err != nil {
		e.emit(ctx, NewStepFailedEvent(location, err))

		return err
	}

	e.emit(ctx, NewStepPassedEvent(location))

	return nil
}

func (e *Executor) emit(ctx context.Context, event Event) {
	for _, o := range e.observers {
		o.OnEvent(ctx, event)
	}
}

var (
	ErrPipelineNameEmpty = errors.New("pipeline name cannot be empty")
	ErrStageNameEmpty    = errors.New("stage name cannot be empty")
	ErrStepNameEmpty     = errors.New("step name cannot be empty")
	ErrStepRunNil        = errors.New("step run cannot be nil")
)

func (e *Executor) validate(p Pipeline) error {
	if p.Name == "" {
		return ErrPipelineNameEmpty
	}

	for i, s := range p.Stages {
		if s.Name == "" {
			return fmt.Errorf("invalid stage[%d]: %w", i, ErrStageNameEmpty)
		}

		for ii, t := range s.Steps {
			if t.Name == "" {
				return fmt.Errorf("invalid step[%d]: %w", ii, ErrStepNameEmpty)
			}

			if t.Run == nil {
				return fmt.Errorf("invalid step[%d]: %w", ii, ErrStepRunNil)
			}
		}
	}

	return nil
}
