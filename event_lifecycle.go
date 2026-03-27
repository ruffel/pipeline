package pipeline

import "time"

// -----------------------------------------------------------------------------
// Pipeline lifecycle events
// -----------------------------------------------------------------------------

// PipelineStartedEvent is emitted before any stage runs. It carries the full
// Pipeline definition so observers can inspect the structure upfront if for
// example they want to render the full pipeline to UI and update incrementally.
type PipelineStartedEvent struct {
	BaseEvent

	Definition Pipeline `json:"definition"`
}

func newPipelineStartedEvent(p Pipeline, now time.Time) PipelineStartedEvent {
	return PipelineStartedEvent{
		BaseEvent:  newBaseEvent(Location{Pipeline: p.Name}, now),
		Definition: p,
	}
}

// PipelinePassedEvent is emitted when all stages complete without error.
type PipelinePassedEvent struct {
	BaseEvent
}

func newPipelinePassedEvent(name string, now time.Time) PipelinePassedEvent {
	return PipelinePassedEvent{
		BaseEvent: newBaseEvent(Location{Pipeline: name}, now),
	}
}

// PipelineFailedEvent is emitted when the pipeline terminates with an error.
type PipelineFailedEvent struct {
	BaseEvent

	Err error `json:"-"`
}

func newPipelineFailedEvent(name string, err error, now time.Time) PipelineFailedEvent {
	return PipelineFailedEvent{
		BaseEvent: newBaseEvent(Location{Pipeline: name}, now),
		Err:       err,
	}
}

// -----------------------------------------------------------------------------
// Stage lifecycle events
// -----------------------------------------------------------------------------

// StageStartedEvent is emitted before a stage's steps are executed.
type StageStartedEvent struct {
	BaseEvent
}

func newStageStartedEvent(loc Location, now time.Time) StageStartedEvent {
	return StageStartedEvent{BaseEvent: newBaseEvent(loc, now)}
}

// StagePassedEvent is emitted when all steps in a stage complete without error.
type StagePassedEvent struct {
	BaseEvent
}

func newStagePassedEvent(loc Location, now time.Time) StagePassedEvent {
	return StagePassedEvent{BaseEvent: newBaseEvent(loc, now)}
}

// StageFailedEvent is emitted when a stage terminates with an error.
type StageFailedEvent struct {
	BaseEvent

	Err error `json:"-"`
}

func newStageFailedEvent(loc Location, err error, now time.Time) StageFailedEvent {
	return StageFailedEvent{BaseEvent: newBaseEvent(loc, now), Err: err}
}

// StageSkippedEvent is emitted when a stage's Condition returns a non-empty
// reason. It is always standalone — no [StageStartedEvent] precedes it.
type StageSkippedEvent struct {
	BaseEvent

	Reason string `json:"reason"`
}

func newStageSkippedEvent(loc Location, reason string, now time.Time) StageSkippedEvent {
	return StageSkippedEvent{BaseEvent: newBaseEvent(loc, now), Reason: reason}
}

// -----------------------------------------------------------------------------
// Step lifecycle events
// -----------------------------------------------------------------------------

// StepStartedEvent is emitted before a step's Run function is called.
type StepStartedEvent struct {
	BaseEvent
}

func newStepStartedEvent(loc Location, now time.Time) StepStartedEvent {
	return StepStartedEvent{BaseEvent: newBaseEvent(loc, now)}
}

// StepPassedEvent is emitted when a step's Run function returns nil.
type StepPassedEvent struct {
	BaseEvent
}

func newStepPassedEvent(loc Location, now time.Time) StepPassedEvent {
	return StepPassedEvent{BaseEvent: newBaseEvent(loc, now)}
}

// StepFailedEvent is emitted when a step's Run function returns a non-nil,
// non-sentinel error.
type StepFailedEvent struct {
	BaseEvent

	Err error `json:"-"`
}

func newStepFailedEvent(loc Location, err error, now time.Time) StepFailedEvent {
	return StepFailedEvent{BaseEvent: newBaseEvent(loc, now), Err: err}
}

// StepSkippedEvent is emitted when a step's Condition returns a non-empty
// reason. It is always standalone — no [StepStartedEvent] precedes it.
type StepSkippedEvent struct {
	BaseEvent

	Reason string `json:"reason"`
}

func newStepSkippedEvent(loc Location, reason string, now time.Time) StepSkippedEvent {
	return StepSkippedEvent{BaseEvent: newBaseEvent(loc, now), Reason: reason}
}
