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
//	    │ apply: E: dpkg was interrupted
//	  ✗ apply — exit status 1 (0.4s)
//	✗ upgrade failed (2.3s)
//
// A rollback shows what it undid:
//
//	▶ upgrade (rollback)
//	  ⊘ apply not undone — no-undo
//	  ↶ backup undone (12s)
//	✓ upgrade passed (12.1s)
//
// Groups and the run show when they start and finish, steps when they
// finish. Output, messages, progress, items, custom events and retries are
// marked │ and named by their step's path below the plan, since steps in a
// parallel group interleave. Write errors are ignored.
type PlainObserver struct {
	mu           sync.Mutex
	w            io.Writer
	root         string
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
		o.started(e)
	case EventRunFinished:
		o.printf("%s %s %s%s%s\n", mark(e.Outcome), e.Path, e.Outcome, errorText(e.Error), elapsed(e.DurationMs))
	case EventGroupStarted:
		o.printf("%s▶ %s%s\n", indent, name(e.Path), o.description(e.Path))
	case EventGroupReset:
		o.printf("%s↺ %s reset: a resume runs it again from the start\n", indent, name(e.Path))
	case EventStepStarted, EventUndoStarted:
		if d := o.description(e.Path); d != "" {
			o.printf("%s· %s%s\n", indent, name(e.Path), d)
		}
	case EventUndoFinished:
		o.printf("%s%s %s %s%s%s%s\n", indent, mark(e.Outcome), name(e.Path), undoText(e.Outcome),
			reasonText(e.Reason), errorText(e.Error), elapsed(e.DurationMs))
	case EventGroupFinished, EventStepFinished:
		o.printf("%s%s %s%s%s%s\n", indent, mark(e.Outcome), name(e.Path), reasonText(e.Reason), errorText(e.Error), elapsed(e.DurationMs))
	case EventOutput, EventMessage, EventProgress, EventItem, EventCustom, EventStepRetry:
		o.detail(indent, e.Path, detailText(e))
	case EventStopRequested:
		by := ""
		if e.Who != "" {
			by = " by " + e.Who
		}

		if e.Mode == StopCancel {
			o.printf("■ cancel requested%s; running steps are cancelled\n", by)
		} else {
			o.printf("■ stop requested%s; running steps will finish\n", by)
		}
	case EventGateWaiting:
		o.printf("%s⏸ %s%s%s\n", indent, name(e.Path), dash(e.Prompt), deadline(e.Deadline))
	case EventGateReleased:
		o.printf("%s▶ %s %s\n", indent, name(e.Path), releasedText(e))
	case EventStepRunning, EventValue:
		// Bookkeeping for resume and rollback; nothing for a reader.
	}
}

// started writes a run's first line, and keeps its plan's descriptions.
func (o *PlainObserver) started(e Event) {
	o.root = e.Path
	o.capture(e.Plan)

	rollback := ""
	if e.Rollback {
		rollback = " (rollback)"
	}

	o.printf("▶ %s%s%s\n", e.Path, rollback, o.description(e.Path))
}

// deadline says when a gate times out, if it does.
func deadline(t time.Time) string {
	if t.IsZero() {
		return ""
	}

	return " (waiting until " + t.Format(time.RFC3339) + ")"
}

// releasedText says what released a gate.
func releasedText(e Event) string {
	if e.Policy != "" {
		return fmt.Sprintf("timed out: %s", e.Policy)
	}

	if e.Who != "" {
		return fmt.Sprintf("approved by %s (%s)", e.Who, e.By)
	}

	return "approved (" + e.By + ")"
}

// detailText is what a step's output, message, progress, item, custom event
// or retry shows.
func detailText(e Event) string {
	switch e.Kind { //nolint:exhaustive // Only the kinds shown under a step.
	case EventMessage:
		return fmt.Sprintf("[%s] %s", strings.ToUpper(string(e.Level)), e.Message)
	case EventProgress:
		if e.Total > 0 {
			return fmt.Sprintf("%s (%d/%d)", e.Message, e.Current, e.Total)
		}

		return e.Message
	case EventItem:
		return fmt.Sprintf("%s %s%s", e.Item, e.Status, dash(e.Message))
	case EventCustom:
		return fmt.Sprintf("[%s] %s", e.Type, e.Data)
	case EventStepRetry:
		return fmt.Sprintf("try %d failed%s, retrying", e.Try, errorText(e.Error))
	default:
		return e.Line
	}
}

// detail writes a line under a step, named by the step's path below the plan.
func (o *PlainObserver) detail(indent, path, text string) {
	step := strings.TrimPrefix(path, o.root+"/")
	if step == "" || step == o.root {
		o.printf("%s  │ %s\n", indent, text)

		return
	}

	o.printf("%s  │ %s: %s\n", indent, step, text)
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

		if n.OnFailure != nil {
			walk(*n.OnFailure)
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
	case OutcomeStopped:
		return "■"
	case OutcomeFailed, OutcomeCancelled:
		return "✗"
	case OutcomeUndone:
		return "↶"
	default:
		return "?"
	}
}

// undoText says what a rollback did with a step.
func undoText(o Outcome) string {
	switch o { //nolint:exhaustive // An undo is undone, skipped or failed.
	case OutcomeUndone:
		return "undone"
	case OutcomeSkipped:
		return "not undone"
	default:
		return "undo failed"
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
