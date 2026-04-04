package pipeline_test

import (
	"context"
	"fmt"
	"sync"

	"github.com/ruffel/pipeline"
)

// recordingObserver captures all events for test assertions.
type recordingObserver struct {
	mu     sync.Mutex
	events []pipeline.Event
}

func (r *recordingObserver) OnEvent(_ context.Context, event pipeline.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.events = append(r.events, event)
}

func (r *recordingObserver) eventTypes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	types := make([]string, len(r.events))

	for i, e := range r.events {
		types[i] = fmt.Sprintf("%T", e)
	}

	return types
}

// noop is a step that does nothing.
func noop(_ context.Context) error { return nil }
