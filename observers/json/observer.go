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
//
// A value that can't be encoded (a func, a channel, NaN) is dropped from its
// line, and a "marshalError" field names it and gives the reason. The rest of
// the line, and every later line, is still written.
package json

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/ruffel/pipeline"
)

// Observer writes JSON Lines event output to an [io.Writer].
//
// If a write fails, the observer records the first error and stops writing.
// Call [Observer.Err] after the pipeline completes to check for write failures.
// A value that can't be encoded is not a write failure (see the package doc).
type Observer struct {
	w   io.Writer
	err error

	// runID carries the current event's run identifier, read from the event
	// context on each OnEvent call.
	runID string

	// starts tracks event timestamps for duration calculation.
	starts map[pipeline.Location]time.Time

	// descriptions holds stage/step descriptions captured from the pipeline
	// definition, keyed by location.
	descriptions map[pipeline.Location]string
}

// New creates a JSON observer that writes to w.
func New(w io.Writer) *Observer {
	return &Observer{
		w:            w,
		starts:       make(map[pipeline.Location]time.Time),
		descriptions: make(map[pipeline.Location]string),
	}
}

// OnEvent implements [pipeline.Observer].
func (o *Observer) OnEvent(ctx context.Context, event pipeline.Event) { //nolint:cyclop,funlen
	o.runID = pipeline.RunIDFrom(ctx)

	switch e := event.(type) {
	case pipeline.PipelineStartedEvent:
		clear(o.starts)
		o.err = nil
		o.captureDescriptions(e.Definition)
		o.starts[e.Location] = e.Timestamp
		o.write("PipelineStarted", e.Location, e.Timestamp, 0, nil)

	case pipeline.PipelinePassedEvent:
		o.write("PipelinePassed", e.Location, e.Timestamp, o.duration(e.Location, e.Timestamp), nil)

	case pipeline.PipelineFailedEvent:
		o.write("PipelineFailed", e.Location, e.Timestamp, o.duration(e.Location, e.Timestamp), fields{
			"error": e.Err.Error(),
		})

	case pipeline.StageStartedEvent:
		o.starts[e.Location] = e.Timestamp
		o.write("StageStarted", e.Location, e.Timestamp, 0, o.descriptionFields(e.Location))

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
		o.write("StepStarted", e.Location, e.Timestamp, 0, o.descriptionFields(e.Location))

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
			"level":   e.Level.String(),
			"message": e.Message,
		}

		if len(e.Attrs) > 0 {
			f["attrs"] = e.Attrs
		}

		o.write("Message", e.Location, e.Timestamp, 0, f)

	case pipeline.OutputEvent:
		o.write("Output", e.Location, e.Timestamp, 0, fields{
			"stream": e.Stream.String(),
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
	RunID      string `json:"runId,omitempty"`
	Pipeline   string `json:"pipeline,omitempty"`
	Stage      string `json:"stage,omitempty"`
	Step       string `json:"step,omitempty"`
	Timestamp  string `json:"timestamp"`
	DurationMs int64  `json:"durationMs,omitempty"`

	// Extra holds event-specific fields, flattened into the top level.
	Extra fields `json:"-"`
}

// MarshalJSON flattens Extra into the top-level object. Structural fields are
// written after Extra so that event-specific data can never overwrite reserved
// keys like "type" or "timestamp".
func (e envelope) MarshalJSON() ([]byte, error) {
	m := map[string]any{}

	maps.Copy(m, e.Extra)

	// Structural fields are set after Extra so they always take precedence.
	m["type"] = e.Type
	m["timestamp"] = e.Timestamp

	if e.RunID != "" {
		m["runId"] = e.RunID
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

	return json.Marshal(m)
}

// Err returns the first write error encountered, or nil. Values that could not
// be encoded are reported in the output instead, and don't set Err.
func (o *Observer) Err() error { return o.err }

func (o *Observer) write(typ string, loc pipeline.Location, ts time.Time, dur time.Duration, extra fields) {
	if o.err != nil {
		return
	}

	env := envelope{
		Type:       typ,
		RunID:      o.runID,
		Pipeline:   loc.Pipeline,
		Stage:      loc.Stage,
		Step:       loc.Step,
		Timestamp:  ts.Format(time.RFC3339Nano),
		DurationMs: dur.Milliseconds(),
		Extra:      extra,
	}

	line, err := json.Marshal(env)
	if err != nil {
		// Only Extra can fail to encode. Drop what fails and say so, rather
		// than losing this line and every line after it.
		env.Extra = encodable(extra)

		if line, err = json.Marshal(env); err != nil {
			// Not expected once every value is pre-encoded, but a bad value
			// must never stop the log: keep the line, drop all of Extra.
			env.Extra = fields{"marshalError": err.Error()}

			if line, err = json.Marshal(env); err != nil {
				o.err = fmt.Errorf("json observer: encode %s event: %w", typ, err)

				return
			}
		}
	}

	_, o.err = o.w.Write(append(line, '\n'))
}

// encodable returns a copy of extra in which each value is encoded once and
// kept as [json.RawMessage], so no marshaler runs again. Values that can't be
// encoded are left out, and a "marshalError" field names each one and why.
func encodable(extra fields) fields {
	out := make(fields, len(extra)+1)

	var problems []string

	for _, k := range slices.Sorted(maps.Keys(extra)) {
		raw, err := encodeField(extra[k])
		if err != nil {
			problems = append(problems, k+": "+err.Error())

			continue
		}

		out[k] = raw
	}

	if len(problems) > 0 {
		out["marshalError"] = strings.Join(problems, "; ")
	}

	return out
}

// encodeField encodes v and checks the result still parses when nested one
// level down, as it is inside the envelope: a value at the JSON depth limit
// encodes on its own but not within the line.
func encodeField(v any) (json.RawMessage, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}

	nested := make([]byte, 0, len(raw)+2)
	nested = append(nested, '[')
	nested = append(nested, raw...)
	nested = append(nested, ']')

	if !json.Valid(nested) {
		return nil, errors.New("value nests too deeply to embed")
	}

	return raw, nil
}

// captureDescriptions records the stage/step descriptions declared in the
// pipeline definition so Started events can carry them.
func (o *Observer) captureDescriptions(def pipeline.Pipeline) {
	clear(o.descriptions)

	for _, stage := range def.Stages {
		loc := pipeline.Location{Pipeline: def.Name, Stage: stage.Name}

		if stage.Description != "" {
			o.descriptions[loc] = stage.Description
		}

		for _, step := range stage.Steps {
			if step.Description != "" {
				o.descriptions[loc.WithStep(step.Name)] = step.Description
			}
		}
	}
}

// descriptionFields returns a "description" field for the location, or nil
// when the definition declared none.
func (o *Observer) descriptionFields(loc pipeline.Location) fields {
	desc, ok := o.descriptions[loc]
	if !ok {
		return nil
	}

	return fields{"description": desc}
}

func (o *Observer) duration(loc pipeline.Location, now time.Time) time.Duration {
	start, ok := o.starts[loc]
	if !ok {
		return 0
	}

	delete(o.starts, loc)

	return now.Sub(start)
}
