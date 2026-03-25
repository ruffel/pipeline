// Package plain provides a plain-text observer that writes pipeline
// execution progress to an [io.Writer].
//
// Output is designed to be human-readable in a terminal emulator. Each event
// is rendered as a single line with indentation reflecting hierarchy:
//
//	▶ Pipeline: deploy
//	  ▶ Stage: build
//	    ✓ compile (1.2s)
//	    ✗ lint — exit status 1 (0.4s)
//	  ✗ Stage: build — exit status 1 (1.6s)
//	✗ Pipeline: deploy — exit status 1 (1.6s)
package plain

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/ruffel/pipeline"
)

// Observer writes plain-text event output to an [io.Writer].
type Observer struct {
	w io.Writer

	// starts tracks event timestamps for duration calculation.
	starts map[pipeline.Location]time.Time
}

// New creates a terminal observer that writes to w.
func New(w io.Writer) *Observer {
	return &Observer{
		w:      w,
		starts: make(map[pipeline.Location]time.Time),
	}
}

// OnEvent implements [pipeline.Observer].
//
//nolint:cyclop
func (o *Observer) OnEvent(_ context.Context, event pipeline.Event) {
	switch e := event.(type) {
	// -------------------------------------------------------------------------
	// Pipeline
	// -------------------------------------------------------------------------
	case pipeline.PipelineStartedEvent:
		clear(o.starts)
		o.starts[e.Location] = e.Timestamp
		o.writef("▶ Pipeline: %s\n", e.Pipeline)

	case pipeline.PipelinePassedEvent:
		o.writef("✓ Pipeline: %s%s\n", e.Pipeline, o.elapsed(e.Location, e.Timestamp))

	case pipeline.PipelineFailedEvent:
		o.writef("✗ Pipeline: %s — %s%s\n", e.Pipeline, e.Err, o.elapsed(e.Location, e.Timestamp))

	// -------------------------------------------------------------------------
	// Stage
	// -------------------------------------------------------------------------
	case pipeline.StageStartedEvent:
		o.starts[e.Location] = e.Timestamp
		o.writef("  ▶ Stage: %s\n", e.Stage)

	case pipeline.StagePassedEvent:
		o.writef("  ✓ Stage: %s%s\n", e.Stage, o.elapsed(e.Location, e.Timestamp))

	case pipeline.StageFailedEvent:
		o.writef("  ✗ Stage: %s — %s%s\n", e.Stage, e.Err, o.elapsed(e.Location, e.Timestamp))

	case pipeline.StageSkippedEvent:
		o.writef("  ⊘ Stage: %s — %s\n", e.Stage, e.Reason)

	// -------------------------------------------------------------------------
	// Step
	// -------------------------------------------------------------------------
	case pipeline.StepStartedEvent:
		o.starts[e.Location] = e.Timestamp

	case pipeline.StepPassedEvent:
		o.writef("    ✓ %s%s\n", e.Step, o.elapsed(e.Location, e.Timestamp))

	case pipeline.StepFailedEvent:
		o.writef("    ✗ %s — %s%s\n", e.Step, e.Err, o.elapsed(e.Location, e.Timestamp))

	case pipeline.StepSkippedEvent:
		o.writef("    ⊘ %s — %s\n", e.Step, e.Reason)

	// -------------------------------------------------------------------------
	// In-flight
	// -------------------------------------------------------------------------
	case pipeline.MessageEvent:
		o.writef("    │ [%s] %s\n", levelLabel(e.Level), e.Message)

	case pipeline.OutputEvent:
		o.writef("    │ %s\n", e.Line)

	case pipeline.ProgressEvent:
		if e.Total > 0 {
			o.writef("    │ %s (%d/%d)\n", e.Message, e.Current, e.Total)
		} else {
			o.writef("    │ %s\n", e.Message)
		}

	case pipeline.CustomEvent:
		o.writef("    │ [%s] %v\n", e.Type, e.Data)
	}
}

func (o *Observer) writef(format string, args ...any) {
	_, _ = fmt.Fprintf(o.w, format, args...)
}

func (o *Observer) elapsed(loc pipeline.Location, now time.Time) string {
	start, ok := o.starts[loc]
	if !ok {
		return ""
	}

	delete(o.starts, loc)

	return fmt.Sprintf(" (%s)", now.Sub(start).Truncate(time.Millisecond))
}

func levelLabel(l pipeline.MessageLevel) string {
	switch l {
	case pipeline.LevelInfo:
		return "INFO"
	case pipeline.LevelWarn:
		return "WARN"
	case pipeline.LevelDebug:
		return "DBUG"
	default:
		return "INFO"
	}
}
