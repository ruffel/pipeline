package runbook

import (
	"container/list"
	"context"
	"fmt"
	"sync"
)

// DefaultChatterLimit is how many output, message, custom and retry events an
// [AsyncObserver] holds for a slow observer before it drops the oldest.
const DefaultChatterLimit = 1024

// AsyncObserver hands events to another observer on its own goroutine, so a
// slow observer (a terminal, a socket, a pipe) never stalls the run. Put
// every observer that writes somewhere that can block behind one.
//
// While the wrapped observer is behind, events wait in a queue with a policy
// for each kind:
//
//   - Lifecycle events (run, group and step events, values and items) are
//     never dropped. Their queue is unbounded; the plan's size bounds it in
//     practice.
//   - Progress keeps only the latest event for each step and key.
//   - Output, messages, custom events and retries keep the newest
//     chatterLimit; older ones are dropped. Before the next event it
//     delivers, the observer then receives a warning message saying how many
//     were dropped. That warning is not in the journal: its Seq is 0.
//
// Order is kept, except that a newer progress event replaces an older one
// for the same step and key.
//
// A panic in the wrapped observer stops delivery; [AsyncObserver.Err] reports
// it. Call [AsyncObserver.Close] when the run has finished.
type AsyncObserver struct {
	next  Observer
	limit int

	mu       sync.Mutex
	wake     *sync.Cond
	pending  *list.List               // of queued, in delivery order
	chatter  []*list.Element          // chatter in pending, oldest first
	progress map[string]*list.Element // latest progress in pending, by step and key
	dropped  int                      // chatter dropped since the last notice
	closed   bool
	err      error

	done chan struct{}
}

type queued struct {
	ctx   context.Context //nolint:containedctx // Handed to the wrapped observer with its event.
	event Event
}

// NewAsyncObserver starts delivering events to next on a new goroutine.
// chatterLimit caps waiting output, message, custom and retry events; zero
// or less means [DefaultChatterLimit].
func NewAsyncObserver(next Observer, chatterLimit int) *AsyncObserver {
	if chatterLimit <= 0 {
		chatterLimit = DefaultChatterLimit
	}

	a := &AsyncObserver{
		next:     next,
		limit:    chatterLimit,
		pending:  list.New(),
		progress: map[string]*list.Element{},
		done:     make(chan struct{}),
	}
	a.wake = sync.NewCond(&a.mu)

	go a.loop()

	return a
}

// OnEvent implements [Observer]. It queues e and returns at once. Events
// that arrive after Close are ignored.
func (a *AsyncObserver) OnEvent(ctx context.Context, e Event) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.closed || a.err != nil {
		return
	}

	item := &queued{ctx: ctx, event: e}

	switch {
	case e.Kind == EventProgress:
		key := e.Path + "\x00" + e.Key
		if old, ok := a.progress[key]; ok {
			a.pending.Remove(old)
		}

		a.progress[key] = a.pending.PushBack(item)
	case !e.Kind.Durable():
		a.chatter = append(a.chatter, a.pending.PushBack(item))

		if len(a.chatter) > a.limit {
			a.pending.Remove(a.chatter[0])
			a.popChatter()
			a.dropped++
		}
	default:
		a.pending.PushBack(item)
	}

	a.wake.Signal()
}

// Close stops accepting events and waits until those queued have been
// delivered, or ctx is done. It returns ctx's error if the wrapped observer
// did not catch up in time; delivery then carries on in the background.
func (a *AsyncObserver) Close(ctx context.Context) error {
	a.mu.Lock()
	a.closed = true
	a.wake.Signal()
	a.mu.Unlock()

	select {
	case <-a.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Err returns the panic that stopped delivery, if the wrapped observer
// panicked.
func (a *AsyncObserver) Err() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.err
}

func (a *AsyncObserver) loop() {
	defer close(a.done)

	for {
		item, notice, ok := a.take()
		if !ok {
			return
		}

		if notice > 0 {
			warning := Event{
				Kind: EventMessage, Level: LevelWarn, TS: item.event.TS, Run: item.event.Run,
				Message: fmt.Sprintf("observer fell behind: %d output and message events were dropped", notice),
			}

			if !a.deliver(item.ctx, warning) {
				return
			}
		}

		if !a.deliver(item.ctx, item.event) {
			return
		}
	}
}

// take waits for the next event, and returns it with the number of chatter
// events dropped since the last notice. It reports false once closed and
// drained.
func (a *AsyncObserver) take() (*queued, int, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	for a.pending.Len() == 0 && !a.closed {
		a.wake.Wait()
	}

	front := a.pending.Front()
	if front == nil {
		return nil, 0, false
	}

	a.pending.Remove(front)

	item, _ := front.Value.(*queued)

	switch {
	case item.event.Kind == EventProgress:
		delete(a.progress, item.event.Path+"\x00"+item.event.Key)
	case !item.event.Kind.Durable():
		a.popChatter()
	}

	notice := a.dropped
	a.dropped = 0

	return item, notice, true
}

// popChatter drops the oldest chatter entry, clearing its slot so the
// backing array doesn't keep a delivered event alive.
func (a *AsyncObserver) popChatter() {
	a.chatter[0] = nil
	a.chatter = a.chatter[1:]

	if len(a.chatter) == 0 {
		a.chatter = nil
	}
}

// deliver calls the wrapped observer, and reports false if it panicked.
func (a *AsyncObserver) deliver(ctx context.Context, e Event) bool {
	v := observe(ctx, a.next, e)
	if v == nil {
		return true
	}

	// Format the panic before taking the lock: the value's Error or String
	// method is user code and may block. (fmt itself recovers a panic in it.)
	err := fmt.Errorf("runbook: async observer: %T panicked: %v", a.next, v)

	a.mu.Lock()
	a.err = err
	a.pending.Init()
	a.chatter, a.progress = nil, map[string]*list.Element{}
	a.mu.Unlock()

	return false
}
