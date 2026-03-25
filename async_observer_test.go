package pipeline_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ruffel/pipeline"
	"github.com/stretchr/testify/assert"
)

func TestAsyncObserver_DeliversEvents(t *testing.T) {
	t.Parallel()

	var received []pipeline.Event

	next := pipeline.ObserverFunc(func(_ context.Context, e pipeline.Event) {
		received = append(received, e)
	})

	async := pipeline.NewAsyncObserver(next, 10)

	expected := []pipeline.Event{
		pipeline.PipelineStartedEvent{},
		pipeline.PipelinePassedEvent{},
	}

	for _, e := range expected {
		async.OnEvent(t.Context(), e)
	}

	async.Close()

	assert.Equal(t, expected, received)
	assert.Equal(t, int64(0), async.Dropped())
}

func TestAsyncObserver_DropsEventsWhenFull(t *testing.T) {
	t.Parallel()

	block := make(chan struct{})

	var received atomic.Int32

	next := pipeline.ObserverFunc(func(_ context.Context, _ pipeline.Event) {
		<-block // Block the consumer goroutine immediately.
		received.Add(1)
	})

	// Buffer size 2.
	async := pipeline.NewAsyncObserver(next, 2)

	// Send 5 events.
	// 1st event is picked up by the blocked goroutine.
	// 2nd and 3rd events fill the buffer.
	// 4th and 5th events should be dropped.
	for range 5 {
		async.OnEvent(t.Context(), pipeline.PipelineStartedEvent{})
		// Slight yield to ensure the goroutine picks up the first event.
		time.Sleep(time.Millisecond)
	}

	assert.Equal(t, int64(2), async.Dropped())

	// Unblock to let remaining events process.
	close(block)
	async.Close()

	// 1 in flight + 2 in buffer = 3 processed.
	assert.Equal(t, int32(3), received.Load())
}

func TestAsyncObserver_CloseWaitsForDrain(t *testing.T) {
	t.Parallel()

	var received atomic.Int32

	next := pipeline.ObserverFunc(func(_ context.Context, _ pipeline.Event) {
		time.Sleep(10 * time.Millisecond)
		received.Add(1)
	})

	async := pipeline.NewAsyncObserver(next, 10)

	for range 3 {
		async.OnEvent(t.Context(), pipeline.PipelineStartedEvent{})
	}

	// Close should block until all 3 slow events are processed.
	async.Close()

	assert.Equal(t, int32(3), received.Load())
}
