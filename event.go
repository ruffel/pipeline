package pipeline

import (
	"context"
	"time"
)

type Observer interface {
	OnEvent(ctx context.Context, event Event)
}

type Event interface {
	event() // Sealed interface
}

type Location struct {
	Pipeline string `json:"pipeline,omitempty"`
	Stage    string `json:"stage,omitempty"`
	Step     string `json:"step,omitempty"`
}

func NewLocation(pipeline string, stage string, step string) Location {
	return Location{
		Pipeline: pipeline,
		Stage:    stage,
		Step:     step,
	}
}

type BaseEvent struct {
	Timestamp time.Time `json:"timestamp"`
	Location
}

func NewBaseEvent(location Location) BaseEvent {
	return BaseEvent{
		Timestamp: time.Now(),
		Location:  location,
	}
}

func (b BaseEvent) event() {}

type PipelineStartedEvent struct {
	BaseEvent
}

func NewPipelineStartedEvent(pipeline string) PipelineStartedEvent {
	return PipelineStartedEvent{
		BaseEvent: NewBaseEvent(NewLocation(pipeline, "", "")),
	}
}

type PipelinePassedEvent struct {
	BaseEvent
}

func NewPipelinePassedEvent(pipeline string) PipelinePassedEvent {
	return PipelinePassedEvent{
		BaseEvent: NewBaseEvent(NewLocation(pipeline, "", "")),
	}
}

type PipelineFailedEvent struct {
	BaseEvent
	Error error `json:"error"`
}

func NewPipelineFailedEvent(pipeline string, err error) PipelineFailedEvent {
	return PipelineFailedEvent{
		BaseEvent: NewBaseEvent(NewLocation(pipeline, "", "")),
		Error:     err,
	}
}

type StageStartedEvent struct {
	BaseEvent
}

func NewStageStartedEvent(location Location) StageStartedEvent {
	return StageStartedEvent{
		BaseEvent: NewBaseEvent(location),
	}
}

type StagePassedEvent struct {
	BaseEvent
}

func NewStagePassedEvent(location Location) StagePassedEvent {
	return StagePassedEvent{
		BaseEvent: NewBaseEvent(location),
	}
}

type StageFailedEvent struct {
	BaseEvent
	Error error `json:"error"`
}

func NewStageFailedEvent(location Location, err error) StageFailedEvent {
	return StageFailedEvent{
		BaseEvent: NewBaseEvent(location),
		Error:     err,
	}
}

type StepStartedEvent struct {
	BaseEvent
}

func NewStepStartedEvent(location Location) StepStartedEvent {
	return StepStartedEvent{
		BaseEvent: NewBaseEvent(location),
	}
}

type StepPassedEvent struct {
	BaseEvent
}

func NewStepPassedEvent(location Location) StepPassedEvent {
	return StepPassedEvent{
		BaseEvent: NewBaseEvent(location),
	}
}

type StepFailedEvent struct {
	BaseEvent
	Error error `json:"error"`
}

func NewStepFailedEvent(location Location, err error) StepFailedEvent {
	return StepFailedEvent{
		BaseEvent: NewBaseEvent(location),
		Error:     err,
	}
}
