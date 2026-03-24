package pipeline

import (
	"context"
	"time"
)

// Observer receives events emitted during pipeline execution. OnEvent is called
// synchronously from within the executor.
type Observer interface {
	OnEvent(ctx context.Context, event Event)
}

// Event is a sealed interface. All events embed BaseEvent and are emitted by
// the executor or by steps via the Emitter helpers.
type Event interface {
	event() // unexported method seals the interface to this package
}

// Location identifies where in the pipeline hierarchy an event was emitted.
type Location struct {
	Pipeline string `json:"pipeline,omitempty"`
	Stage    string `json:"stage,omitempty"`
	Step     string `json:"step,omitempty"`
}

// WithStep returns a copy of l with the Step field set.
func (l Location) WithStep(name string) Location {
	l.Step = name

	return l
}

// BaseEvent carries the fields common to every event.
type BaseEvent struct {
	Location

	Timestamp time.Time `json:"timestamp"`
}

// newBaseEvent constructs a BaseEvent using the provided clock. Passing
// time.Now() at the call site keeps the clock injectable for tests.
func newBaseEvent(loc Location, now time.Time) BaseEvent {
	return BaseEvent{Location: loc, Timestamp: now}
}

// event implements the sealed Event interface.
func (b BaseEvent) event() {}
