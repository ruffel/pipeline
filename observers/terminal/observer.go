// Package terminal provides a richly-styled, line-based terminal observer
// built on [github.com/charmbracelet/lipgloss]. It renders boxes, colored
// status markers, and aligned step prefixes while remaining stream-friendly
// (no screen rewriting).
package terminal

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/ruffel/pipeline"
)

// Observer is a richly-styled, line-based terminal observer. It provides
// boxes, checkmarks, color, and aligned step prefixes while remaining
// stream-friendly (no screen rewriting).
type Observer struct {
	w    io.Writer
	opts Options
	mu   sync.Mutex

	// Pipeline-scoped state, reset on each PipelineStartedEvent.
	state State
}

// New creates a new terminal observer with default formatting.
func New(w io.Writer) *Observer {
	return NewWithOptions(w, DefaultOptions())
}

// NewWithOptions creates a new terminal observer with custom formatting overrides.
func NewWithOptions(w io.Writer, opts Options) *Observer {
	opts.applyDefaults()

	return &Observer{
		w:    w,
		opts: opts,
	}
}

// OnEvent implements [pipeline.Observer].
func (o *Observer) OnEvent(ctx context.Context, ev pipeline.Event) {
	o.mu.Lock()
	defer o.mu.Unlock()

	var output string

	switch e := ev.(type) {
	case pipeline.PipelineStartedEvent:
		o.state = newState(e.Definition)
		o.state.startTime = e.Timestamp
		output = o.opts.FormatPipelineStart(ctx, e, o.state, o.opts.Palette)

	case pipeline.PipelinePassedEvent:
		dur := e.Timestamp.Sub(o.state.startTime)
		output = o.opts.FormatPipelineEnd(ctx, e, dur, o.state, o.opts.Palette)

	case pipeline.PipelineFailedEvent:
		dur := e.Timestamp.Sub(o.state.startTime)
		output = o.opts.FormatPipelineEnd(ctx, e, dur, o.state, o.opts.Palette)

	case pipeline.StageStartedEvent:
		output = o.opts.FormatStageStart(ctx, e, o.state, o.opts.Palette)

	case pipeline.StepStartedEvent:
		o.state.stepTimes[e.Step] = e.Timestamp

	case pipeline.StepPassedEvent:
		dur := e.Timestamp.Sub(o.state.stepTimes[e.Step])
		output = o.opts.FormatStepPass(ctx, e, dur, o.state, o.opts.Palette)

	case pipeline.StepFailedEvent:
		dur := e.Timestamp.Sub(o.state.stepTimes[e.Step])
		o.state.FailedSteps = append(o.state.FailedSteps, e.Step+" - "+e.Err.Error())
		output = o.opts.FormatStepFail(ctx, e, dur, o.state, o.opts.Palette)

	case pipeline.StepSkippedEvent:
		output = o.opts.FormatStepSkip(ctx, e, o.state, o.opts.Palette)

	case pipeline.MessageEvent:
		output = o.opts.FormatMessage(ctx, e, o.state, o.opts.Palette)

	case pipeline.ProgressEvent:
		output = o.opts.FormatProgress(ctx, e, o.state, o.opts.Palette)

	case pipeline.OutputEvent:
		output = o.opts.FormatOutput(ctx, e, o.state, o.opts.Palette)
	}

	if output != "" {
		_, _ = fmt.Fprintln(o.w, output)
	}
}
