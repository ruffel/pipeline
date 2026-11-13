// Package runbooktest helps test plans and steps built with runbook: it
// simulates a crash at any point of a run, controls the clock, and renders a
// journal compactly for assertions.
//
// A crash is simulated by a journal that stops accepting writes right after
// a chosen event is stored. The run then stops at its next write, exactly as
// if the process had died there, and the test can inspect what the journal
// recorded, which is what a resumed run will read:
//
//	crash := runbooktest.CrashAfter("upgrade/node1/packages", runbooktest.Running)
//	_, err := runbook.Run(ctx, plan, crash.Journal)
//	// errors.Is(err, runbooktest.ErrCrashed), and crash.Crashed() is true.
//	state, _ := crash.Journal.State()
//	// state.Element("upgrade/node1/packages") is started, with RunCalled set.
//
//	crash.Restart()
//	_, err = runbook.Run(ctx, plan, crash.Journal) // resumes
//
// Code that runs between the crash point and the run's next write still
// runs: a crash at [Running] lets Run do its work, but nothing after it is
// recorded. That is the moment between a step changing the system and the
// journal saying so, which is the hazard resume has to handle.
package runbooktest

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ruffel/pipeline/runbook"
)

// ErrCrashed is the error every write fails with after a simulated crash.
var ErrCrashed = errors.New("runbooktest: simulated crash")

// Point is a moment in a step's life at which to crash: just after one of
// its events is stored.
type Point string

// The points every step passes through. Running is reached only if Check
// found the work not done.
const (
	// Started is just after step.started: Check has not run.
	Started Point = "step.started"
	// Running is just after step.running: Run is about to be called.
	Running Point = "step.running"
	// Finished is just after step.finished: the outcome is recorded.
	Finished Point = "step.finished"
	// GroupFinished is just after a group's group.finished. Use it with the
	// group's path.
	GroupFinished Point = "group.finished"
)

// The points a gate passes through, besides a step's.
const (
	// GateWaiting is just after gate.waiting: the gate is waiting for
	// approval.
	GateWaiting Point = "gate.waiting"
	// GateReleased is just after gate.released: the gate is released, and
	// has not finished.
	GateReleased Point = "gate.released"
)

// The points a rollback passes through for each step with an Undo.
const (
	// UndoStarted is just after undo.started: Undo is about to be called.
	UndoStarted Point = "undo.started"
	// UndoFinished is just after undo.finished: the undo's outcome is
	// recorded.
	UndoFinished Point = "undo.finished"
)

// Value is the point just after the step records key with Record.
func Value(key string) Point {
	return Point("value:" + key)
}

// Item is the point just after the step records its result for target id.
func Item(id string) Point {
	return Point("item:" + id)
}

// Crash is a memory journal that crashes at a chosen point.
type Crash struct {
	// Journal is the journal to run against, and to inspect afterwards.
	Journal *runbook.Journal

	crashed  atomic.Bool
	disarmed atomic.Bool
}

// CrashAfter returns a memory journal that crashes just after the step at
// path reaches point: that event is stored, and every later write fails
// with [ErrCrashed].
func CrashAfter(path string, point Point) *Crash {
	c := &Crash{}

	c.Journal = runbook.MemoryJournal(runbook.WithWriteHook(func(e runbook.Event) error {
		if c.crashed.Load() {
			return ErrCrashed
		}

		if !c.disarmed.Load() && e.Path == path && matches(e, point) {
			c.crashed.Store(true)
		}

		return nil
	}))

	return c
}

// Crashed reports whether the crash point was reached.
func (c *Crash) Crashed() bool {
	return c.crashed.Load()
}

// Restart simulates the process starting again after the crash: the journal
// accepts writes once more, and the crash point is disarmed. Run against the
// same journal then resumes.
func (c *Crash) Restart() {
	c.disarmed.Store(true)
	c.crashed.Store(false)
}

func matches(e runbook.Event, p Point) bool {
	switch {
	case strings.HasPrefix(string(p), "value:"):
		return e.Kind == runbook.EventValue && e.Key == strings.TrimPrefix(string(p), "value:")
	case strings.HasPrefix(string(p), "item:"):
		return e.Kind == runbook.EventItem && e.Item == strings.TrimPrefix(string(p), "item:")
	default:
		return string(e.Kind) == string(p)
	}
}

// FakeClock is a [runbook.Clock] that only moves when told to. Retry
// backoffs wait on it, so a test advances past them instead of sleeping.
// Per-try timeouts use real time, because they are context deadlines.
type FakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []waiter
}

type waiter struct {
	at time.Time
	ch chan time.Time
}

// NewFakeClock returns a FakeClock set to start.
func NewFakeClock(start time.Time) *FakeClock {
	return &FakeClock{now: start}
}

// Now implements [runbook.Clock].
func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

// After implements [runbook.Clock]. The channel receives once the clock has
// been advanced by d; at once if d is zero or less.
func (c *FakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	ch := make(chan time.Time, 1)

	if d <= 0 {
		ch <- c.now

		return ch
	}

	c.waiters = append(c.waiters, waiter{at: c.now.Add(d), ch: ch})

	return ch
}

// Advance moves the clock on by d, firing every wait that falls due.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = c.now.Add(d)

	kept := c.waiters[:0]

	for _, w := range c.waiters {
		if w.at.After(c.now) {
			kept = append(kept, w)

			continue
		}

		w.ch <- c.now
	}

	c.waiters = kept
}

// Waiters returns how many waits have not fallen due, so a test can tell
// when a step has started its backoff.
func (c *FakeClock) Waiters() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.waiters)
}

// Trace renders a journal's lifecycle events as "kind path [outcome[/reason]]"
// lines, for comparing sequences in tests. Events of kinds listed in also
// are included too.
func Trace(events []runbook.Event, also ...runbook.EventKind) []string {
	want := map[runbook.EventKind]bool{
		runbook.EventRunStarted: true, runbook.EventRunFinished: true,
		runbook.EventGroupStarted: true, runbook.EventGroupFinished: true,
		runbook.EventStepStarted: true, runbook.EventStepRunning: true, runbook.EventStepFinished: true,
	}
	for _, k := range also {
		want[k] = true
	}

	out := make([]string, 0, len(events))

	for _, e := range events {
		if !want[e.Kind] {
			continue
		}

		line := string(e.Kind) + " " + e.Path

		if e.Outcome != "" {
			line += " " + string(e.Outcome)

			if e.Reason != "" {
				line += "/" + string(e.Reason)
			}
		}

		out = append(out, line)
	}

	return out
}
