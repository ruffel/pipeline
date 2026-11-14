package pipeline

import (
	"container/list"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Draining the queue releases the chatter slice, so delivered events and
// their contexts aren't kept alive by an idle observer.
func TestAsyncObserver_ReleasesDeliveredChatter(t *testing.T) {
	t.Parallel()

	a := NewAsyncObserver(ObserverFunc(func(context.Context, Event) {}), 2)

	for range 10 {
		a.OnEvent(t.Context(), Event{Kind: EventOutput, Line: "x"})
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	require.NoError(t, a.Close(ctx))

	a.mu.Lock()
	defer a.mu.Unlock()

	assert.Nil(t, a.chatter)
	assert.Zero(t, a.pending.Len())
}

func TestPopChatter_ClearsTheSlot(t *testing.T) {
	t.Parallel()

	// No delivery goroutine: this exercises the queue bookkeeping alone.
	a := &AsyncObserver{pending: list.New()}
	backing := []*list.Element{a.pending.PushBack(1), a.pending.PushBack(2)}
	a.chatter = backing
	a.popChatter()

	assert.Nil(t, backing[0], "the popped slot no longer references its element")
	assert.Len(t, a.chatter, 1)
}
