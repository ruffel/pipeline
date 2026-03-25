package pipeline

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

// Executor runs a Pipeline, emitting events to its registered observers.
type Executor struct {
	mu        sync.Mutex
	observers []Observer
}

// NewExecutor returns an Executor that broadcasts events to the given observers.
func NewExecutor(observers ...Observer) *Executor {
	return &Executor{observers: observers}
}

// Run validates and executes the pipeline. It returns nil on success, including
// when ErrSkipPipeline is returned by a step. Any other error from a step is
// wrapped with its location context for diagnostics.
func (e *Executor) Run(ctx context.Context, p Pipeline) error {
	if err := e.validate(p); err != nil {
		return err
	}

	return e.runPipeline(ctx, p)
}

func (e *Executor) runPipeline(ctx context.Context, p Pipeline) error {
	e.emit(ctx, newPipelineStartedEvent(p, time.Now()))

	if err := e.executeStages(ctx, p); err != nil {
		e.emit(ctx, newPipelineFailedEvent(p.Name, err, time.Now()))

		return err
	}

	e.emit(ctx, newPipelinePassedEvent(p.Name, time.Now()))

	return nil
}

func (e *Executor) executeStages(ctx context.Context, p Pipeline) error {
	for _, stage := range p.Stages {
		if err := ctx.Err(); err != nil {
			return err
		}

		loc := Location{Pipeline: p.Name, Stage: stage.Name}

		if err := e.runStage(ctx, loc, stage); err != nil {
			if errors.Is(err, ErrSkipPipeline) {
				return nil
			}

			return err
		}
	}

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

	var err error
	if s.Parallel {
		err = e.runStepsParallel(ctx, loc, s)
	} else {
		err = e.runStepsSequential(ctx, loc, s)
	}

	if err != nil {
		e.emit(ctx, newStageFailedEvent(loc, err, time.Now()))

		return err
	}

	e.emit(ctx, newStagePassedEvent(loc, time.Now()))

	return nil
}

func (e *Executor) runStepsSequential(ctx context.Context, loc Location, s Stage) error {
	for _, step := range s.Steps {
		if err := e.runStep(e.stepCtx(ctx, loc, step), loc.WithStep(step.Name), step); err != nil {
			if errors.Is(err, ErrSkipStage) {
				return nil
			}

			return err
		}
	}

	return nil
}

func (e *Executor) runStepsParallel(ctx context.Context, loc Location, s Stage) error {
	if s.ContinueOnError {
		return e.runStepsBestEffort(ctx, loc, s)
	}

	return e.runStepsFailFast(ctx, loc, s)
}

func (e *Executor) runStepsFailFast(ctx context.Context, loc Location, s Stage) error {
	g, gctx := errgroup.WithContext(ctx)

	for _, step := range s.Steps {
		stepCtx := e.stepCtx(gctx, loc, step)
		stepLoc := loc.WithStep(step.Name)

		g.Go(func() error {
			return e.runStep(stepCtx, stepLoc, step)
		})
	}

	return g.Wait()
}

func (e *Executor) runStepsBestEffort(ctx context.Context, loc Location, s Stage) error {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)

	for _, step := range s.Steps {
		wg.Add(1)

		stepCtx := e.stepCtx(ctx, loc, step)
		stepLoc := loc.WithStep(step.Name)

		go func() {
			defer wg.Done()

			if err := e.runStep(stepCtx, stepLoc, step); err != nil {
				mu.Lock()

				errs = append(errs, err)

				mu.Unlock()
			}
		}()
	}

	wg.Wait()

	return errors.Join(errs...)
}

func (e *Executor) runStep(ctx context.Context, loc Location, s Step) (stepErr error) {
	if s.Condition != nil {
		if reason := s.Condition(ctx); reason != "" {
			e.emit(ctx, newStepSkippedEvent(loc, reason, time.Now()))

			return nil
		}
	}

	e.emit(ctx, newStepStartedEvent(loc, time.Now()))

	defer func() {
		if r := recover(); r != nil {
			stack := string(debug.Stack())
			stepErr = fmt.Errorf("panic in step %q: %v\n%s", s.Name, r, stack)
			e.emit(ctx, newStepFailedEvent(loc, stepErr, time.Now()))
		}
	}()

	if err := s.Run(ctx); err != nil {
		if errors.Is(err, ErrSkipStep) {
			e.emit(ctx, newStepSkippedEvent(loc, err.Error(), time.Now()))

			return nil
		}

		e.emit(ctx, newStepFailedEvent(loc, err, time.Now()))

		return fmt.Errorf("stage %q step %q: %w", loc.Stage, loc.Step, err)
	}

	e.emit(ctx, newStepPassedEvent(loc, time.Now()))

	return nil
}

// stepCtx creates a child context with the emitter wired for the given step.
func (e *Executor) stepCtx(ctx context.Context, loc Location, s Step) context.Context {
	return WithEmitter(ctx, NewEmitter(e.observers, loc.WithStep(s.Name)))
}

func (e *Executor) emit(ctx context.Context, event Event) {
	e.mu.Lock()
	defer e.mu.Unlock()

	for _, o := range e.observers {
		o.OnEvent(ctx, event)
	}
}

func (e *Executor) validate(p Pipeline) error {
	var errs []error

	if p.Name == "" {
		errs = append(errs, errors.New("pipeline name cannot be empty"))
	}

	if len(p.Stages) == 0 {
		errs = append(errs, errors.New("pipeline must have at least one stage"))
	}

	stageNames := make(map[string]bool, len(p.Stages))

	for i, s := range p.Stages {
		if s.Name == "" {
			errs = append(errs, fmt.Errorf("stage[%d]: name cannot be empty", i))
		} else if stageNames[s.Name] {
			errs = append(errs, fmt.Errorf("stage[%d]: duplicate stage name %q", i, s.Name))
		} else {
			stageNames[s.Name] = true
		}

		if len(s.Steps) == 0 {
			errs = append(errs, fmt.Errorf("stage[%d] %q: must have at least one step", i, s.Name))
		}

		stepNames := make(map[string]bool, len(s.Steps))

		for j, t := range s.Steps {
			if t.Name == "" {
				errs = append(errs, fmt.Errorf("stage[%d] step[%d]: name cannot be empty", i, j))
			} else if stepNames[t.Name] {
				errs = append(errs, fmt.Errorf("stage[%d] step[%d]: duplicate step name %q", i, j, t.Name))
			} else {
				stepNames[t.Name] = true
			}

			if t.Run == nil {
				errs = append(errs, fmt.Errorf("stage[%d] step[%d] %q: run function cannot be nil", i, j, t.Name))
			}
		}
	}

	return errors.Join(errs...)
}
