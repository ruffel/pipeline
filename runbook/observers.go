package runbook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// JSONObserver writes every event as one line of JSON in the journal's
// format, so the file it writes can be read like a journal.
//
// The first write error stops it; [JSONObserver.Err] reports it. Observers
// don't fail runs: the journal is the record.
type JSONObserver struct {
	mu  sync.Mutex
	enc *json.Encoder
	err error
}

// NewJSONObserver returns a JSONObserver writing to w.
func NewJSONObserver(w io.Writer) *JSONObserver {
	return &JSONObserver{enc: json.NewEncoder(w)}
}

// OnEvent implements [Observer].
func (o *JSONObserver) OnEvent(_ context.Context, e Event) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.err != nil {
		return
	}

	if err := o.enc.Encode(e); err != nil {
		o.err = fmt.Errorf("runbook: json observer: %w", err)
	}
}

// Err returns the first write error, or nil.
func (o *JSONObserver) Err() error {
	o.mu.Lock()
	defer o.mu.Unlock()

	return o.err
}

// PlainObserver writes a run as indented plain text, one line per event that
// matters to a reader:
//
//	▶ upgrade
//	  ▶ prechecks
//	    ✓ ring (1.8s)
//	  ✓ prechecks (1.9s)
//	  ⊘ backup — done
//	  ✗ apply — exit status 1 (0.4s)
//	    │ E: dpkg was interrupted
//	✗ upgrade failed (2.3s)
//
// Groups and the run show when they start and finish, steps when they
// finish. Output, messages, progress, items, custom events and retries are
// shown under their step, marked │. Write errors are ignored.
type PlainObserver struct {
	mu           sync.Mutex
	w            io.Writer
	descriptions map[string]string
}

// NewPlainObserver returns a PlainObserver writing to w.
func NewPlainObserver(w io.Writer) *PlainObserver {
	return &PlainObserver{w: w, descriptions: map[string]string{}}
}

// OnEvent implements [Observer].
func (o *PlainObserver) OnEvent(_ context.Context, e Event) {
	o.mu.Lock()
	defer o.mu.Unlock()

	indent := strings.Repeat("  ", strings.Count(e.Path, "/"))

	switch e.Kind {
	case EventRunStarted:
		o.capture(e.Plan)
		o.printf("▶ %s%s\n", e.Path, o.description(e.Path))
	case EventRunFinished:
		o.printf("%s %s %s%s%s\n", mark(e.Outcome), e.Path, e.Outcome, errorText(e.Error), elapsed(e.DurationMs))
	case EventGroupStarted:
		o.printf("%s▶ %s%s\n", indent, name(e.Path), o.description(e.Path))
	case EventStepStarted:
		if d := o.description(e.Path); d != "" {
			o.printf("%s· %s%s\n", indent, name(e.Path), d)
		}
	case EventGroupFinished, EventStepFinished:
		o.printf("%s%s %s%s%s%s\n", indent, mark(e.Outcome), name(e.Path), reasonText(e.Reason), errorText(e.Error), elapsed(e.DurationMs))
	case EventOutput:
		o.printf("%s  │ %s\n", indent, e.Line)
	case EventMessage:
		o.printf("%s  │ [%s] %s\n", indent, strings.ToUpper(string(e.Level)), e.Message)
	case EventProgress:
		if e.Total > 0 {
			o.printf("%s  │ %s (%d/%d)\n", indent, e.Message, e.Current, e.Total)
		} else {
			o.printf("%s  │ %s\n", indent, e.Message)
		}
	case EventItem:
		o.printf("%s  │ %s: %s%s\n", indent, e.Item, e.Status, dash(e.Message))
	case EventCustom:
		o.printf("%s  │ [%s] %s\n", indent, e.Type, e.Data)
	case EventStepRetry:
		o.printf("%s  │ try %d failed%s, retrying\n", indent, e.Try, errorText(e.Error))
	case EventStepRunning, EventValue:
		// Bookkeeping for resume and rollback; nothing for a reader.
	}
}

func (o *PlainObserver) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(o.w, format, args...)
}

// capture keeps the plan's descriptions, by path.
func (o *PlainObserver) capture(spec *NodeSpec) {
	clear(o.descriptions)

	var walk func(NodeSpec)

	walk = func(n NodeSpec) {
		if n.Description != "" {
			o.descriptions[n.Path] = n.Description
		}

		for _, c := range n.Children {
			walk(c)
		}
	}

	if spec != nil {
		walk(*spec)
	}
}

func (o *PlainObserver) description(path string) string {
	return dash(o.descriptions[path])
}

func mark(o Outcome) string {
	switch o {
	case OutcomePassed:
		return "✓"
	case OutcomeSkipped:
		return "⊘"
	case OutcomeFailed, OutcomeCancelled, OutcomeStopped:
		return "✗"
	default:
		return "?"
	}
}

// name returns the last segment of a path.
func name(path string) string {
	return path[strings.LastIndexByte(path, '/')+1:]
}

func dash(s string) string {
	if s == "" {
		return ""
	}

	return " — " + s
}

func reasonText(r Reason) string {
	return dash(string(r))
}

func errorText(e *ErrorInfo) string {
	if e == nil {
		return ""
	}

	return dash(e.Message)
}

func elapsed(ms int64) string {
	if ms <= 0 {
		return ""
	}

	return fmt.Sprintf(" (%s)", time.Duration(ms)*time.Millisecond)
}
