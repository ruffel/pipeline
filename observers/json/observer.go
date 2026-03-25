// Package json provides an observer that writes pipeline events as JSON Lines
// (one JSON object per line) to an [io.Writer].
//
// Each line includes a "type" discriminator for easy filtering:
//
//	{"type":"PipelineStarted","pipeline":"deploy","timestamp":"..."}
//	{"type":"StepPassed","pipeline":"deploy","stage":"build","step":"compile","timestamp":"...","durationMs":1200}
//
// Error fields are serialised as strings (Go's json.Marshal produces "{}" for
// the error interface). The observer handles this transparently.
package json

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"time"

	"github.com/ruffel/pipeline"
)

// Observer writes JSON Lines event output to an [io.Writer].
//
// If a write fails, the observer records the first error and stops writing.
// Call [Observer.Err] after the pipeline completes to check for write failures.
type Observer struct {
	enc *json.Encoder
	err error

	// starts tracks event timestamps for duration calculation.
	starts map[pipeline.Location]time.Time
}

// New creates a JSON observer that writes to w.
func New(w io.Writer) *Observer {
	return &Observer{
		enc:    json.NewEncoder(w),
		starts: make(map[pipeline.Location]time.Time),
	}
}

// OnEvent implements [pipeline.Observer].
func (o *Observer) OnEvent(_ context.Context, event pipeline.Event) { //nolint:cyclop,funlen
	switch e := event.(type) {
	case pipeline.PipelineStartedEvent:
		clear(o.starts)
		o.err = nil
		o.starts[e.Location] = e.Timestamp
		o.write("PipelineStarted", e.Location, e.Timestamp, 0, fields{
			"definition": e.Definition,
		})

	case pipeline.PipelinePassedEvent:
		o.write("PipelinePassed", e.Location, e.Timestamp, o.duration(e.Location, e.Timestamp), nil)

	case pipeline.PipelineFailedEvent:
		o.write("PipelineFailed", e.Location, e.Timestamp, o.duration(e.Location, e.Timestamp), fields{
			"error": e.Err.Error(),
		})

	case pipeline.StageStartedEvent:
		o.starts[e.Location] = e.Timestamp
		o.write("StageStarted", e.Location, e.Timestamp, 0, nil)

	case pipeline.StagePassedEvent:
		o.write("StagePassed", e.Location, e.Timestamp, o.duration(e.Location, e.Timestamp), nil)

	case pipeline.StageFailedEvent:
		o.write("StageFailed", e.Location, e.Timestamp, o.duration(e.Location, e.Timestamp), fields{
			"error": e.Err.Error(),
		})

	case pipeline.StageSkippedEvent:
		o.write("StageSkipped", e.Location, e.Timestamp, 0, fields{
			"reason": e.Reason,
		})

	case pipeline.StepStartedEvent:
		o.starts[e.Location] = e.Timestamp
		o.write("StepStarted", e.Location, e.Timestamp, 0, nil)

	case pipeline.StepPassedEvent:
		o.write("StepPassed", e.Location, e.Timestamp, o.duration(e.Location, e.Timestamp), nil)

	case pipeline.StepFailedEvent:
		o.write("StepFailed", e.Location, e.Timestamp, o.duration(e.Location, e.Timestamp), fields{
			"error": e.Err.Error(),
		})

	case pipeline.StepSkippedEvent:
		o.write("StepSkipped", e.Location, e.Timestamp, 0, fields{
			"reason": e.Reason,
		})

	case pipeline.MessageEvent:
		f := fields{
			"level":   levelString(e.Level),
			"message": e.Message,
		}

		if len(e.Attrs) > 0 {
			f["attrs"] = e.Attrs
		}

		o.write("Message", e.Location, e.Timestamp, 0, f)

	case pipeline.OutputEvent:
		o.write("Output", e.Location, e.Timestamp, 0, fields{
			"stream": streamString(e.Stream),
			"line":   e.Line,
		})

	case pipeline.ProgressEvent:
		o.write("Progress", e.Location, e.Timestamp, 0, fields{
			"key":     e.Key,
			"message": e.Message,
			"current": e.Current,
			"total":   e.Total,
		})

	case pipeline.CustomEvent:
		o.write("Custom", e.Location, e.Timestamp, 0, fields{
			"eventType": e.Type,
			"data":      e.Data,
		})
	}
}

// fields is a shorthand for ad-hoc key-value pairs merged into the envelope.
type fields map[string]any

// envelope is the JSON structure written for each event.
type envelope struct {
	Type       string `json:"type"`
	Pipeline   string `json:"pipeline,omitempty"`
	Stage      string `json:"stage,omitempty"`
	Step       string `json:"step,omitempty"`
	Timestamp  string `json:"timestamp"`
	DurationMs int64  `json:"durationMs,omitempty"`

	// Extra holds event-specific fields, flattened into the top level.
	Extra fields `json:"-"`
}

// MarshalJSON flattens Extra into the top-level object.
func (e envelope) MarshalJSON() ([]byte, error) {
	m := map[string]any{
		"type":      e.Type,
		"timestamp": e.Timestamp,
	}

	if e.Pipeline != "" {
		m["pipeline"] = e.Pipeline
	}

	if e.Stage != "" {
		m["stage"] = e.Stage
	}

	if e.Step != "" {
		m["step"] = e.Step
	}

	if e.DurationMs > 0 {
		m["durationMs"] = e.DurationMs
	}

	maps.Copy(m, e.Extra)

	return json.Marshal(m)
}

// Err returns the first write error encountered, or nil.
func (o *Observer) Err() error { return o.err }

func (o *Observer) write(typ string, loc pipeline.Location, ts time.Time, dur time.Duration, extra fields) {
	if o.err != nil {
		return
	}

	o.err = o.enc.Encode(envelope{
		Type:       typ,
		Pipeline:   loc.Pipeline,
		Stage:      loc.Stage,
		Step:       loc.Step,
		Timestamp:  ts.Format(time.RFC3339Nano),
		DurationMs: dur.Milliseconds(),
		Extra:      extra,
	})
}

func (o *Observer) duration(loc pipeline.Location, now time.Time) time.Duration {
	start, ok := o.starts[loc]
	if !ok {
		return 0
	}

	delete(o.starts, loc)

	return now.Sub(start)
}

func levelString(l pipeline.MessageLevel) string {
	switch l {
	case pipeline.LevelInfo:
		return "info"
	case pipeline.LevelWarn:
		return "warn"
	case pipeline.LevelDebug:
		return "debug"
	default:
		return "info"
	}
}

func streamString(s pipeline.Stream) string {
	switch s {
	case pipeline.Stdout:
		return "stdout"
	case pipeline.Stderr:
		return "stderr"
	default:
		return "stdout"
	}
}
