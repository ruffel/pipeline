package pipeline

import "time"

// Stream identifies stdout or stderr for OutputEvent.
type Stream int

const (
	Stdout Stream = iota
	Stderr
)

// MessageLevel classifies the severity of a MessageEvent.
type MessageLevel int

const (
	LevelInfo MessageLevel = iota
	LevelWarn
	LevelDebug
)

// -----------------------------------------------------------------------------
// In-flight events — emitted by steps via the Emitter helpers.
//
// Constructors are exported so callers can build events outside the EmitX
// convenience layer when needed.
// -----------------------------------------------------------------------------

// ProgressEvent reports incremental progress within a step. When both Current
// and Total are zero the progress is indeterminate, i.e. blocking on some
// external signal.
type ProgressEvent struct {
	BaseEvent

	Key     string `json:"key,omitempty"`
	Message string `json:"message"`
	Current int    `json:"current"`
	Total   int    `json:"total"`
}

// NewProgressEvent constructs a ProgressEvent with the given fields.
func NewProgressEvent(loc Location, key, message string, current, total int, now time.Time) ProgressEvent {
	return ProgressEvent{
		BaseEvent: newBaseEvent(loc, now),
		Key:       key,
		Message:   message,
		Current:   current,
		Total:     total,
	}
}

// OutputEvent carries a single line of raw process output from a step.
type OutputEvent struct {
	BaseEvent

	Stream Stream `json:"stream"`
	Line   string `json:"line"`
}

// NewOutputEvent constructs an OutputEvent with the given fields.
func NewOutputEvent(loc Location, stream Stream, line string, now time.Time) OutputEvent {
	return OutputEvent{
		BaseEvent: newBaseEvent(loc, now),
		Stream:    stream,
		Line:      line,
	}
}

// MessageEvent carries a structured log-like message emitted by a step.
type MessageEvent struct {
	BaseEvent

	Level   MessageLevel      `json:"level"`
	Message string            `json:"message"`
	Attrs   map[string]string `json:"attrs,omitempty"`
}

// NewMessageEvent constructs a MessageEvent with the given fields.
func NewMessageEvent(loc Location, level MessageLevel, message string, attrs map[string]string, now time.Time) MessageEvent {
	return MessageEvent{
		BaseEvent: newBaseEvent(loc, now),
		Level:     level,
		Message:   message,
		Attrs:     attrs,
	}
}

// CustomEvent is for domain-specific signals. The Type field is a dot-namespaced
// (e.g. "deploy.endpoint-ready"). Observers switch on Type to handle known
// custom events and ignore unknown ones.
type CustomEvent struct {
	BaseEvent

	Type string `json:"type"`
	Data any    `json:"data,omitempty"`
}

// NewCustomEvent constructs a CustomEvent with the given fields.
func NewCustomEvent(loc Location, eventType string, data any, now time.Time) CustomEvent {
	return CustomEvent{
		BaseEvent: newBaseEvent(loc, now),
		Type:      eventType,
		Data:      data,
	}
}
