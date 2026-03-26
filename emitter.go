package pipeline

import (
	"context"
	"fmt"
	"time"
)

// Emitter allows steps to emit events during execution without direct access
// to the executor. An Emitter is stored in the step's context by the executor
// and accessed via the EmitX package-level helpers.
type Emitter struct {
	broadcast func(context.Context, Event)
	loc       Location
}

// NewEmitter constructs an Emitter that delivers events through the given
// broadcast function at the given location. The broadcast function is
// responsible for any synchronisation required by the underlying observers.
//
// Inside the executor, the broadcast function is [Executor.emit] which already
// serialises observer calls. For standalone use (e.g. CLI glue), any function
// will work.
func NewEmitter(broadcast func(context.Context, Event), loc Location) *Emitter {
	if broadcast == nil {
		broadcast = func(context.Context, Event) {}
	}

	return &Emitter{broadcast: broadcast, loc: loc}
}

type emitterKey struct{}

// WithEmitter returns a new context carrying the given Emitter.
func WithEmitter(ctx context.Context, e *Emitter) context.Context {
	return context.WithValue(ctx, emitterKey{}, e)
}

// EmitterFrom retrieves the Emitter from ctx, or nil if none is present.
func EmitterFrom(ctx context.Context) *Emitter {
	e, _ := ctx.Value(emitterKey{}).(*Emitter)

	return e
}

func (e *Emitter) emit(ctx context.Context, event Event) {
	e.broadcast(ctx, event)
}

// -----------------------------------------------------------------------------
// EmitX helpers — package-level functions called from step Run functions.
// All are safe no-ops when ctx carries no Emitter.
// -----------------------------------------------------------------------------

// EmitInfo emits an informational message from the current step.
func EmitInfo(ctx context.Context, message string) {
	emitMessage(ctx, LevelInfo, message, nil)
}

// EmitInfof emits a formatted informational message.
func EmitInfof(ctx context.Context, format string, args ...any) {
	EmitInfo(ctx, fmt.Sprintf(format, args...))
}

// EmitWarn emits a warning message from the current step.
func EmitWarn(ctx context.Context, message string) {
	emitMessage(ctx, LevelWarn, message, nil)
}

// EmitWarnf emits a formatted warning message.
func EmitWarnf(ctx context.Context, format string, args ...any) {
	EmitWarn(ctx, fmt.Sprintf(format, args...))
}

// EmitDebug emits a debug-level message from the current step.
func EmitDebug(ctx context.Context, message string) {
	emitMessage(ctx, LevelDebug, message, nil)
}

// EmitDebugf emits a formatted debug-level message.
func EmitDebugf(ctx context.Context, format string, args ...any) {
	EmitDebug(ctx, fmt.Sprintf(format, args...))
}

// EmitProgress reports incremental progress. When current and total are both
// zero the progress is indeterminate.
func EmitProgress(ctx context.Context, key, message string, current, total int) {
	e := EmitterFrom(ctx)
	if e == nil {
		return
	}

	e.emit(ctx, NewProgressEvent(e.loc, key, message, current, total, time.Now()))
}

// EmitIndeterminateProgress reports progress without a known total. This is
// equivalent to calling [EmitProgress] with zero current and total values.
func EmitIndeterminateProgress(ctx context.Context, key, message string) {
	e := EmitterFrom(ctx)
	if e == nil {
		return
	}

	e.emit(ctx, NewProgressEvent(e.loc, key, message, 0, 0, time.Now()))
}

// EmitOutput emits a single line of raw output from the current step.
func EmitOutput(ctx context.Context, stream Stream, line string) {
	e := EmitterFrom(ctx)
	if e == nil {
		return
	}

	e.emit(ctx, NewOutputEvent(e.loc, stream, line, time.Now()))
}

// EmitCustom emits a domain-specific event. The eventType should be a
// dot-namespaced discriminator (e.g. "deploy.endpoint-ready").
func EmitCustom(ctx context.Context, eventType string, data any) {
	e := EmitterFrom(ctx)
	if e == nil {
		return
	}

	e.emit(ctx, NewCustomEvent(e.loc, eventType, data, time.Now()))
}

func emitMessage(ctx context.Context, level MessageLevel, message string, attrs map[string]string) {
	e := EmitterFrom(ctx)
	if e == nil {
		return
	}

	e.emit(ctx, NewMessageEvent(e.loc, level, message, attrs, time.Now()))
}
