package pipeline

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
)

// Executor runs a Pipeline, emitting events to its registered observers.
type Executor struct {
	mu        sync.Mutex
	observers []Observer
	runID     string
}

// NewExecutor returns an Executor that broadcasts events to the given observers.
func NewExecutor(observers ...Observer) *Executor {
	return NewExecutorWithOptions(WithObservers(observers...))
}

// Run validates and executes the pipeline. It returns nil on success, including
// when ErrSkipPipeline is returned by a step. Any other error from a step is
// wrapped with its location context for diagnostics.
func (e *Executor) Run(ctx context.Context, p Pipeline) error {
	if err := e.validate(p); err != nil {
		return err
	}

	if e.runID != "" {
		ctx = withRunID(ctx, e.runID)
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

	if errors.Is(err, ErrSkipPipeline) {
		e.emit(ctx, newStagePassedEvent(loc, time.Now()))

		return ErrSkipPipeline
	}

	if err != nil {
		e.emit(ctx, newStageFailedEvent(loc, err, time.Now()))

		return err
	}

	e.emit(ctx, newStagePassedEvent(loc, time.Now()))

	return nil
}

func (e *Executor) runStepsSequential(ctx context.Context, loc Location, s Stage) error {
	for i, step := range s.Steps {
		// Mirror the between-stages check: once the run context is cancelled,
		// remaining steps must not start.
		if err := ctx.Err(); err != nil {
			return err
		}

		if err := e.runStep(e.stepCtx(ctx, loc, step), loc.WithStep(step.Name), step); err != nil {
			if errors.Is(err, ErrSkipStage) {
				e.emitBypassedSteps(ctx, loc, s.Steps[i+1:], step.Name)

				return nil
			}

			return err
		}
	}

	return nil
}

// emitBypassedSteps emits a [StepSkippedEvent] for each step bypassed when an
// earlier step ends its stage via [ErrSkipStage], so observers that pre-render
// the pipeline definition can resolve every step.
func (e *Executor) emitBypassedSteps(ctx context.Context, loc Location, steps []Step, cause string) {
	reason := fmt.Sprintf("%q ended the stage early", cause)

	for _, step := range steps {
		e.emit(ctx, newStepSkippedEvent(loc.WithStep(step.Name), reason, time.Now()))
	}
}

func (e *Executor) runStepsParallel(ctx context.Context, loc Location, s Stage) error {
	if s.ContinueOnError {
		return e.runStepsBestEffort(ctx, loc, s)
	}

	return e.runStepsFailFast(ctx, loc, s)
}

func (e *Executor) runStepsFailFast(ctx context.Context, loc Location, s Stage) error {
	g, gctx := errgroup.WithContext(ctx)

	if s.MaxParallel > 0 {
		g.SetLimit(s.MaxParallel)
	}

	var (
		mu           sync.Mutex
		skipPipeline bool
		unstarted    atomic.Bool
	)

	for _, step := range s.Steps {
		stepCtx := e.stepCtx(gctx, loc, step)
		stepLoc := loc.WithStep(step.Name)

		g.Go(func() error {
			// Once the group context is done — a peer failed, or the run
			// was cancelled — a step that has not started must not start.
			// It emits nothing, like steps the sequential path never
			// reaches. This check sits inside the goroutine because a
			// MaxParallel limit admits steps one at a time, so the context
			// may have ended while this step waited for its slot.
			if gctx.Err() != nil {
				unstarted.Store(true)

				return nil
			}

			err := e.runStep(stepCtx, stepLoc, step)
			if err == nil {
				return nil
			}

			mu.Lock()
			defer mu.Unlock()

			// Absorb sentinels so they don't cancel peers via errgroup.
			switch {
			case errors.Is(err, ErrSkipStage):
				return nil
			case errors.Is(err, ErrSkipPipeline):
				skipPipeline = true

				return nil
			default:
				return err
			}
		})
	}

	if err := g.Wait(); err != nil {
		return err
	}

	// No step failed, yet some never started: only the run context can have
	// ended the group, so report its cancellation rather than passing.
	if unstarted.Load() {
		if err := ctx.Err(); err != nil {
			return err
		}
	}

	if skipPipeline {
		return ErrSkipPipeline
	}

	return nil
}

func (e *Executor) runStepsBestEffort(ctx context.Context, loc Location, s Stage) error {
	var (
		wg           sync.WaitGroup
		mu           sync.Mutex
		errs         []error
		skipPipeline bool
		unstarted    atomic.Bool
	)

	// Optional counting semaphore bounding concurrent steps.
	var sem chan struct{}
	if s.MaxParallel > 0 {
		sem = make(chan struct{}, s.MaxParallel)
	}

	for _, step := range s.Steps {
		wg.Add(1)

		stepCtx := e.stepCtx(ctx, loc, step)
		stepLoc := loc.WithStep(step.Name)

		go func() {
			defer wg.Done()

			release, ok := acquire(ctx, sem)
			if !ok {
				// Cancelled while waiting for a slot: never start.
				unstarted.Store(true)

				return
			}

			defer release()

			// A slot can free up only after the run was cancelled: a step
			// that has not started must not start. It emits nothing, like
			// steps the sequential path never reaches.
			if ctx.Err() != nil {
				unstarted.Store(true)

				return
			}

			if err := e.runStep(stepCtx, stepLoc, step); err != nil {
				mu.Lock()

				switch {
				case errors.Is(err, ErrSkipPipeline):
					skipPipeline = true
				case errors.Is(err, ErrSkipStage):
					// Absorbed — stage continues with remaining steps.
				default:
					errs = append(errs, err)
				}

				mu.Unlock()
			}
		}()
	}

	wg.Wait()

	// Real failures take priority over skip signals and cancellation.
	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	// Steps left unstarted means the run context ended: report it rather
	// than passing a stage that did not run to completion.
	if unstarted.Load() {
		if err := ctx.Err(); err != nil {
			return err
		}
	}

	// No real failures — propagate skip if requested.
	if skipPipeline {
		return ErrSkipPipeline
	}

	return nil
}

// acquire takes a slot from sem, returning the matching release function. It
// reports false if ctx ends before a slot frees. A nil sem means unbounded
// concurrency: acquire succeeds at once and release is a no-op.
func acquire(ctx context.Context, sem chan struct{}) (func(), bool) {
	if sem == nil {
		return func() {}, true
	}

	select {
	case sem <- struct{}{}:
		return func() { <-sem }, true
	case <-ctx.Done():
		return nil, false
	}
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
			// A panicking step is a step failure like any other: the event
			// carries the PanicError and the returned error wraps it with
			// the step's location, exactly as the error path below does.
			perr := &PanicError{Value: r, Stack: debug.Stack()}
			e.emit(ctx, newStepFailedEvent(loc, perr, time.Now()))
			stepErr = wrapStepErr(loc, perr)
		}
	}()

	if err := s.Run(ctx); err != nil {
		if errors.Is(err, ErrSkipStage) || errors.Is(err, ErrSkipPipeline) {
			e.emit(ctx, newStepPassedEvent(loc, time.Now()))

			return err
		}

		e.emit(ctx, newStepFailedEvent(loc, err, time.Now()))

		return wrapStepErr(loc, err)
	}

	e.emit(ctx, newStepPassedEvent(loc, time.Now()))

	return nil
}

// wrapStepErr adds the step's location to an error returned by its Run
// function so the failure is diagnosable from the error alone.
func wrapStepErr(loc Location, err error) error {
	return fmt.Errorf("stage %q step %q: %w", loc.Stage, loc.Step, err)
}

// stepCtx creates a child context with the emitter wired for the given step.
func (e *Executor) stepCtx(ctx context.Context, loc Location, s Step) context.Context {
	return WithEmitter(ctx, NewEmitter(e.emit, loc.WithStep(s.Name)))
}

func (e *Executor) emit(ctx context.Context, event Event) {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Mask the emitter so observers cannot call back into the executor
	// and cause a deadlock.
	ctx = WithoutEmitter(ctx)

	// Strip cancellation only for terminal lifecycle events if the context
	// is cancelled.
	if ctx.Err() != nil && isTerminalEvent(event) {
		ctx = context.WithoutCancel(ctx)
	}

	for _, o := range e.observers {
		o.OnEvent(ctx, event)
	}
}

func isTerminalEvent(e Event) bool {
	switch e.(type) {
	case PipelinePassedEvent, PipelineFailedEvent,
		StagePassedEvent, StageFailedEvent, StageSkippedEvent,
		StepPassedEvent, StepFailedEvent, StepSkippedEvent:
		return true
	default:
		return false
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
		switch {
		case s.Name == "":
			errs = append(errs, fmt.Errorf("stage[%d]: name cannot be empty", i))
		case stageNames[s.Name]:
			errs = append(errs, fmt.Errorf("stage[%d]: duplicate stage name %q", i, s.Name))
		default:
			stageNames[s.Name] = true
		}

		errs = append(errs, validateStage(i, s)...)
	}

	return errors.Join(errs...)
}

func validateStage(i int, s Stage) []error {
	var errs []error

	if len(s.Steps) == 0 {
		errs = append(errs, fmt.Errorf("stage[%d] %q: must have at least one step", i, s.Name))
	}

	if s.ContinueOnError && !s.Parallel {
		errs = append(errs, fmt.Errorf("stage[%d] %q: ContinueOnError requires Parallel", i, s.Name))
	}

	if s.MaxParallel < 0 {
		errs = append(errs, fmt.Errorf("stage[%d] %q: MaxParallel cannot be negative", i, s.Name))
	}

	if s.MaxParallel > 0 && !s.Parallel {
		errs = append(errs, fmt.Errorf("stage[%d] %q: MaxParallel requires Parallel", i, s.Name))
	}

	stepNames := make(map[string]bool, len(s.Steps))

	for j, t := range s.Steps {
		switch {
		case t.Name == "":
			errs = append(errs, fmt.Errorf("stage[%d] step[%d]: name cannot be empty", i, j))
		case stepNames[t.Name]:
			errs = append(errs, fmt.Errorf("stage[%d] step[%d]: duplicate step name %q", i, j, t.Name))
		default:
			stepNames[t.Name] = true
		}

		if t.Run == nil {
			errs = append(errs, fmt.Errorf("stage[%d] step[%d] %q: run function cannot be nil", i, j, t.Name))
		}
	}

	return errs
}
