package pipeline_test

import (
	"context"
	"fmt"

	"github.com/ruffel/pipeline"
)

// recordingObserver captures all events for test assertions.
type recordingObserver struct {
	events []pipeline.Event
}

func (r *recordingObserver) OnEvent(_ context.Context, event pipeline.Event) {
	r.events = append(r.events, event)
}

func (r *recordingObserver) eventTypes() []string {
	types := make([]string, len(r.events))

	for i, e := range r.events {
		types[i] = fmt.Sprintf("%T", e)
	}

	return types
}

// noop is a step that does nothing.
func noop(_ context.Context) error { return nil }
