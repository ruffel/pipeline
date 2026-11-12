package runbook

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

// ErrRunnerGone is returned by [Follow] when the journal's latest attempt
// never finished and no process holds the journal: the runner crashed or was
// killed. The run can be resumed.
var ErrRunnerGone = errors.New("runbook: the run did not finish, and its runner is gone")

// DefaultFollowPoll is how often [Follow] looks for new events.
const DefaultFollowPoll = 250 * time.Millisecond

// History says which of the events already in the journal [Follow] replays
// before it follows new ones.
type History int

// The history choices.
const (
	// HistoryAll replays every event.
	HistoryAll History = iota
	// HistoryLifecycle replays only lifecycle events: runs, groups, steps,
	// values and items. A viewer attaching late gets the plan and where
	// everything stands, without hours of old output.
	HistoryLifecycle
)

// FollowOptions configures [Follow].
type FollowOptions struct {
	// After skips events up to and including this sequence number. A viewer
	// that has a [State] follows on from its LastSeq.
	After int64
	// History chooses what is replayed of the events already in the journal
	// when Follow is called. Events written later are always delivered.
	History History
	// KeepFollowing makes Follow wait for further attempts instead of
	// returning when the run has ended, or its runner has gone. It then
	// returns only when its context is done.
	KeepFollowing bool
	// Poll is how often to look for new events. Zero means
	// [DefaultFollowPoll].
	Poll time.Duration
}

// Follow delivers the events in the journal file at path to the observers,
// then keeps delivering new ones as a run writes them, like tail -f. It
// reads without locking, so it works from any process and cannot disturb the
// run. Observers get exactly the events the runner's own observers got.
//
// Follow returns:
//
//   - nil, when it has delivered the end of the latest attempt and no process
//     holds the journal. A journal with no attempts, or none yet, counts.
//   - [ErrRunnerGone], when the latest attempt never finished and no process
//     holds the journal. Everything written has been delivered.
//   - the context's error, when ctx is done.
//   - [ErrJournalCorrupt], for a journal that is damaged.
//
// With KeepFollowing set, the first two cases wait for another attempt
// instead.
//
// A panic in an observer ends Follow with an error.
func Follow(ctx context.Context, path string, opts FollowOptions, observers ...Observer) error {
	f := &follower{
		reader:    journalReader{path: path},
		opts:      opts,
		observers: observers,
		history:   true,
	}

	poll := opts.Poll
	if poll <= 0 {
		poll = DefaultFollowPoll
	}

	for {
		if err := f.read(ctx); err != nil {
			return err
		}

		// Everything in the journal when Follow was called has been read.
		f.history = false

		if !journalLocked(path) {
			// The runner may have written its last events and closed between
			// that read and this check, so read once more before deciding.
			if err := f.read(ctx); err != nil {
				return err
			}

			if !opts.KeepFollowing {
				if f.open {
					return ErrRunnerGone
				}

				return nil
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// follower is one Follow call's progress through the journal.
type follower struct {
	reader    journalReader
	opts      FollowOptions
	observers []Observer

	// history is true while reading what the journal held when Follow began.
	history bool
	// seq is the last sequence number read; open is whether the latest
	// attempt has started and not finished.
	seq  int64
	open bool
}

// read delivers the complete events written since the last read. A journal
// that does not exist yet has none.
func (f *follower) read(ctx context.Context) error {
	err := f.reader.next(func(line []byte) error {
		e, err := decodeEvent(line)
		if err != nil {
			return err
		}

		if e.Seq <= f.seq {
			return fmt.Errorf("sequence number %d follows %d", e.Seq, f.seq)
		}

		f.seq = e.Seq

		switch e.Kind { //nolint:exhaustive // Only these two say whether an attempt is open.
		case EventRunStarted:
			f.open = true
		case EventRunFinished:
			f.open = false
		}

		if e.Seq <= f.opts.After || (f.history && f.opts.History == HistoryLifecycle && !e.Kind.Durable()) {
			return nil
		}

		return f.deliver(ctx, e)
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if p, ok := errors.AsType[*observerPanicError](err); ok {
		// An observer's panic is not damage to the journal.
		return p
	}

	return err
}

func (f *follower) deliver(ctx context.Context, e Event) error {
	for _, o := range f.observers {
		if v := observe(ctx, o, e); v != nil {
			return &observerPanicError{observer: o, value: v}
		}
	}

	return nil
}

// observerPanicError is a panic recovered from an observer during Follow.
type observerPanicError struct {
	observer Observer
	value    any
}

func (e *observerPanicError) Error() string {
	return fmt.Sprintf("runbook: follow: observer %T panicked: %v", e.observer, e.value)
}
