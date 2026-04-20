package pipeline

import (
	"context"
	"sync"
	"sync/atomic"
)

// DefaultAsyncObserverBuffer is the recommended queue size for
// [NewAsyncObserver] apropos nothing.
const DefaultAsyncObserverBuffer = 256

// AsyncObserver wraps an [Observer] and processes events in a background
// goroutine. This prevents a slow observer (e.g., one writing to a network)
// from blocking pipeline execution.
//
// AsyncObserver is caller-owned. After construction, the caller is responsible
// for calling [Close] once the pipeline run is finished so queued events are
// drained and the background goroutine exits.
//
// If the internal buffer fills up, new events are dropped to prevent stalling
// the pipeline. This is a drop-newest policy: already accepted events stay in
// the queue, and the event that cannot be enqueued is dropped. The number of
// dropped events can be checked via [Dropped].
type AsyncObserver struct {
	next      Observer
	events    chan asyncEvent
	dropped   atomic.Int64
	done      chan struct{}
	closeOnce sync.Once

	// mu guards the closed flag and the channel send in OnEvent against
	// a concurrent Close. OnEvent takes an RLock (concurrent senders don't
	// block each other), Close takes a full Lock before closing the channel.
	mu     sync.RWMutex
	closed bool
}

type asyncEvent struct {
	ctx   context.Context //nolint:containedctx // Context is required by the Observer interface.
	event Event
}

// NewAsyncObserver creates a new AsyncObserver wrapping next. The bufferSize
// determines how many events can be queued before newer events are dropped.
//
// next must be non-nil and bufferSize must be at least 1. NewAsyncObserver
// panics if either requirement is not met.
//
// The caller must call [Close] when the pipeline finishes to ensure all queued
// events are processed and to release the background goroutine.
func NewAsyncObserver(next Observer, bufferSize int) *AsyncObserver {
	if next == nil {
		panic("pipeline: NewAsyncObserver next observer cannot be nil")
	}

	if bufferSize < 1 {
		panic("pipeline: NewAsyncObserver bufferSize must be >= 1")
	}

	obs := &AsyncObserver{
		next:   next,
		events: make(chan asyncEvent, bufferSize),
		done:   make(chan struct{}),
	}

	go obs.loop()

	return obs
}

// OnEvent implements [Observer]. It queues the event for background processing.
// If the queue is full or the observer has been closed, the event is dropped
// and the dropped counter is incremented.
func (a *AsyncObserver) OnEvent(ctx context.Context, event Event) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	if a.closed {
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
// was full or because the observer was already closed.
func (a *AsyncObserver) Dropped() int64 {
	return a.dropped.Load()
}

// Close stops accepting new events, waits for all queued events to be
// processed by the wrapped observer, and releases the background goroutine.
// Close is safe to call multiple times.
func (a *AsyncObserver) Close() {
	a.closeOnce.Do(func() {
		a.mu.Lock()
		a.closed = true
		a.mu.Unlock()

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
