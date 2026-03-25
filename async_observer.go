package pipeline

import (
	"context"
	"sync"
	"sync/atomic"
)

// AsyncObserver wraps an [Observer] and processes events in a background
// goroutine. This prevents a slow observer (e.g., one writing to a network)
// from blocking pipeline execution.
//
// If the internal buffer fills up, new events are dropped to prevent stalling
// the pipeline. The number of dropped events can be checked via [Dropped].
type AsyncObserver struct {
	next      Observer
	events    chan asyncEvent
	dropped   atomic.Int64
	done      chan struct{}
	closeOnce sync.Once
	closed    atomic.Bool
}

type asyncEvent struct {
	ctx   context.Context //nolint:containedctx // Context is required by the Observer interface.
	event Event
}

// NewAsyncObserver creates a new AsyncObserver wrapping next. The bufferSize
// determines how many events can be queued before newer events are dropped.
//
// The caller must call [Close] when the pipeline finishes to ensure all queued
// events are processed and to release the background goroutine.
func NewAsyncObserver(next Observer, bufferSize int) *AsyncObserver {
	obs := &AsyncObserver{
		next:   next,
		events: make(chan asyncEvent, bufferSize),
		done:   make(chan struct{}),
	}

	go obs.loop()

	return obs
}

// OnEvent implements [Observer]. It queues the event for background processing.
// If the queue is full, the event is dropped and the dropped counter is incremented.
func (a *AsyncObserver) OnEvent(ctx context.Context, event Event) {
	if a.closed.Load() {
		a.dropped.Add(1)

		return
	}

	select {
	case a.events <- asyncEvent{ctx: ctx, event: event}:
	default:
		a.dropped.Add(1)
	}
}

// Dropped returns the number of events that were dropped because the buffer
// was full.
func (a *AsyncObserver) Dropped() int64 {
	return a.dropped.Load()
}

// Close stops accepting new events, waits for all queued events to be
// processed by the wrapped observer, and releases the background goroutine.
// Close is safe to call multiple times.
func (a *AsyncObserver) Close() {
	a.closeOnce.Do(func() {
		a.closed.Store(true)
		close(a.events)
	})

	<-a.done
}

func (a *AsyncObserver) loop() {
	defer close(a.done)

	for e := range a.events {
		a.next.OnEvent(e.ctx, e.event)
	}
}
