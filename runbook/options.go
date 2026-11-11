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

type config struct {
	observers []Observer
	labels    L
	clock     Clock

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
