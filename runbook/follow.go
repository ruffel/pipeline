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

// Follow delivers the events in the journal file named by journal to the
// observers, then keeps delivering new ones as a run writes them, like tail
// -f. It reads without locking, so it works from any process and cannot
// disturb the run. Observers get exactly the events the runner's own observers got.
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
func Follow(ctx context.Context, journal string, opts FollowOptions, observers ...Observer) error {
	f := &follower{
		reader:    journalReader{path: journal},
		opts:      opts,
		observers: observers,
		locked:    journalLocked,
	}

	return f.run(ctx)
}

// follower is one Follow call's progress through the journal.
type follower struct {
	reader    journalReader
	opts      FollowOptions
	observers []Observer
	// locked reports whether a process holds the journal at a path.
	locked func(path string) bool

	// history is where the journal ended when Follow began. Events that
	// start before it are history; the History option applies to those only.
	history int64
	// seq is the last sequence number read; open is whether the latest
	// attempt has started and not finished.
	seq  int64
	open bool
}

func (f *follower) run(ctx context.Context) error {
	poll := f.opts.Poll
	if poll <= 0 {
		poll = DefaultFollowPoll
	}

	if info, err := os.Stat(f.reader.path); err == nil {
		f.history = info.Size()
	}

	for {
		if err := f.read(ctx); err != nil {
			return err
		}

		next, err := f.afterRead(ctx)

		switch {
		case err != nil:
			return err
		case next == followAgain:
			continue
		case next == followEnd && f.open:
			return ErrRunnerGone
		case next == followEnd:
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// What to do after a read has reached the end of the journal.
const (
	followWait  = iota // wait a poll interval, then read again
	followAgain        // read again at once
	followEnd          // nothing more will come
)

// afterRead decides what follows a read that reached the end of the journal.
func (f *follower) afterRead(ctx context.Context) (int, error) {
	if f.locked(f.reader.path) {
		return followWait, nil
	}

	// Nobody held the journal at that moment. Read once more before
	// deciding: the runner may have written its last events and closed
	// between the read and the check.
	before := f.seq

	if err := f.read(ctx); err != nil {
		return followWait, err
	}

	switch {
	case f.seq != before:
		// Something was written in between, perhaps by a new runner that now
		// holds the journal. Look again from the top.
		return followAgain, nil
	case f.opts.KeepFollowing:
		return followWait, nil
	default:
		return followEnd, nil
	}
}

// read delivers the complete events written since the last read. A journal
// that does not exist yet has none. It returns the context's error as soon
// as the context is done, leaving the rest unread.
func (f *follower) read(ctx context.Context) error {
	err := f.reader.next(func(line []byte) error {
		if err := ctx.Err(); err != nil {
			return halt(err)
		}

		if f.passOver(line) {
			return nil
		}

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

		// The reader's offset is still where this line starts.
		old := f.reader.offset < f.history

		if e.Seq <= f.opts.After || (old && f.opts.History == HistoryLifecycle && !e.Kind.Durable()) {
			return nil
		}

		return f.deliver(ctx, e)
	})

	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	return ctx.Err()
}

// passOver reports whether line is old output that the viewer asked to skip,
// and if so takes its place in the sequence without decoding it. Anything
// unusual about the line is left to the decoder to report. The reader's
// offset is still where the line starts.
func (f *follower) passOver(line []byte) bool {
	if f.opts.History != HistoryLifecycle || f.reader.offset >= f.history {
		return false
	}

	env, ok := peekEvent(line)
	if !ok || !env.chatter || env.v > SchemaVersion || env.seq <= f.seq {
		return false
	}

	f.seq = env.seq

	return true
}

func (f *follower) deliver(ctx context.Context, e Event) error {
	for _, o := range f.observers {
		if v := observe(ctx, o, e); v != nil {
			// An observer's panic is not damage to the journal.
			return halt(&observerPanicError{observer: o, value: v})
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
