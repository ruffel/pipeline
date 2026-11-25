package pipeline

import (
	"context"
	"fmt"
)

// emit stores e in the journal, then gives it to the observers. If the
// journal can't be written the run is cancelled, and every later emit
// returns the same error.
func (r *runner) emit(e Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.emitLocked(e)
}

func (r *runner) emitLocked(e Event) error {
	if r.fatal != nil {
		return r.fatal
	}

	e.Run, e.V = r.id, r.version
	if e.TS.IsZero() {
		e.TS = r.clock.Now().UTC()
	}

	stored, err := r.journal.append(r.redact.event(e))
	if err != nil {
		r.fatal = err
		r.cancel(r.fatal)

		return r.fatal
	}

	r.deliver(stored)

	// Delivering may have written warnings about panicking observers; if one
	// of those writes failed, the journal is broken and the caller must stop.
	return r.fatal
}

// deliver hands e to every observer, dropping any that panic and recording
// a warning for each.
func (r *runner) deliver(e Event) {
	var panicked []string

	kept := make([]Observer, 0, len(r.observers))

	for _, o := range r.observers {
		if v := observe(r.obsCtx, o, e); v != nil {
			panicked = append(panicked, fmt.Sprintf("observer %T panicked and was removed: %v", o, v))

			continue
		}

		kept = append(kept, o)
	}

	r.observers = kept

	for _, msg := range panicked {
		_ = r.emitLocked(Event{Kind: EventMessage, Level: LevelWarn, Message: msg})
	}
}

// observe calls o and returns what it panicked with, if anything.
func observe(ctx context.Context, o Observer, e Event) any {
	var recovered any

	func() {
		defer func() { recovered = recover() }()

		o.OnEvent(ctx, e)
	}()

	return recovered
}
