package runbook

import (
	"context"
	"maps"
	"time"
)

// Observer receives every event after the journal has stored it. Calls are
// serialised, in journal order, from the goroutine that wrote the event, so
// an observer that blocks stalls the run: put anything that writes to a
// terminal, socket or pipe behind an asynchronous wrapper.
//
// A panic in OnEvent is recovered. That observer receives nothing more, and a
// warning is recorded.
type Observer interface {
	OnEvent(ctx context.Context, e Event)
}

// ObserverFunc adapts a function to [Observer].
type ObserverFunc func(ctx context.Context, e Event)

// OnEvent implements [Observer] by calling f.
func (f ObserverFunc) OnEvent(ctx context.Context, e Event) {
	f(ctx, e)
}

// Clock tells the time and waits. The default is the system clock; tests can
// supply their own.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// Option configures [Run].
type Option func(*config)

// ResumeMode says how a resumed run treats an Effect step that an earlier
// attempt finished.
type ResumeMode int

// The resume modes.
const (
	// ResumeVerify runs the step's Check again, and its Run if the work is
	// no longer done. A step with no Check is skipped. It is the default.
	ResumeVerify ResumeMode = iota
	// ResumeTrust skips the step without calling Check.
	ResumeTrust
)

type config struct {
	observers []Observer
	labels    L
	clock     Clock
	resume    ResumeMode
	force     bool

	// outputBudget is MaxStepOutput, except in tests.
	outputBudget int
}

// WithObservers adds observers to the run. Nil observers are ignored.
func WithObservers(observers ...Observer) Option {
	return func(c *config) {
		for _, o := range observers {
			if o != nil {
				c.observers = append(c.observers, o)
			}
		}
	}
}

// WithRunLabels sets labels on the run, such as {"operation": "u-42"}. They
// are recorded in the run.started event.
func WithRunLabels(l L) Option {
	return func(c *config) {
		c.labels = maps.Clone(l)
	}
}

// WithResume sets how a resumed run treats Effect steps that an earlier
// attempt finished. The default is [ResumeVerify].
func WithResume(mode ResumeMode) Option {
	return func(c *config) {
		c.resume = mode
	}
}

// Force runs the whole plan again, whatever the journal says: nothing is
// skipped because an earlier attempt did it, and a journal whose latest
// attempt passed is accepted. Check still decides whether each Run is
// needed, and values recorded by earlier attempts stay readable. The attempt
// is recorded as forced.
func Force() Option {
	return func(c *config) {
		c.force = true
	}
}

// WithClock replaces the system clock, for tests.
func WithClock(clock Clock) Option {
	return func(c *config) {
		if clock != nil {
			c.clock = clock
		}
	}
}

type systemClock struct{}

func (systemClock) Now() time.Time {
	return time.Now()
}

func (systemClock) After(d time.Duration) <-chan time.Time {
	return time.After(d)
}
