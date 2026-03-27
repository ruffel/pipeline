package terminal

import (
	"context"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/ruffel/pipeline"
)

// Palette defines a semantic set of colors and icons for the observer.
// This allows users to re-brand the observer without rewriting the
// structural layout of the terminal frames or boxes.
type Palette struct {
	// Accents
	Primary lipgloss.TerminalColor // Main brand color for borders and headers.
	Accent  lipgloss.TerminalColor // Highlights or active items.

	// Base
	Foreground lipgloss.TerminalColor // Standard text.
	Muted      lipgloss.TerminalColor // Dimmed or secondary text (structure/chrome).
	Output     lipgloss.TerminalColor // Subprocess stdout (readable but subordinate).

	// Statuses
	Success lipgloss.TerminalColor
	Warning lipgloss.TerminalColor
	Error   lipgloss.TerminalColor
	Info    lipgloss.TerminalColor

	// Icons
	SuccessIcon string
	ErrorIcon   string
	WarnIcon    string
	InfoIcon    string
	SkipIcon    string
}

// State holds pipeline-scoped runtime state that is computed by the
// [Observer] and passed read-only to each formatter. This keeps formatters
// pure functions while still giving them access to contextual information
// like which stages are parallel or what the longest step name is.
type State struct {
	// StageParallel maps stage names to whether they are parallel.
	StageParallel map[string]bool

	// MaxStepLen is the length of the longest step name in the pipeline,
	// used for left-column alignment in parallel stages.
	MaxStepLen int

	// FailedSteps tracks failed step descriptions for the end-of-pipeline summary.
	FailedSteps []string

	// startTime is the pipeline start timestamp (unexported, used by Observer).
	startTime time.Time

	// stageTimes tracks per-stage start timestamps (unexported, used by Observer).
	stageTimes map[string]time.Time

	// stepTimes tracks per-step start timestamps (unexported, used by Observer).
	stepTimes map[string]time.Time
}

// newState initialises a State from a pipeline definition.
func newState(def pipeline.Pipeline) State {
	s := State{
		StageParallel: make(map[string]bool, len(def.Stages)),
		stageTimes:    make(map[string]time.Time),
		stepTimes:     make(map[string]time.Time),
	}

	for _, stage := range def.Stages {
		s.StageParallel[stage.Name] = stage.Parallel

		for _, step := range stage.Steps {
			if len(step.Name) > s.MaxStepLen {
				s.MaxStepLen = len(step.Name)
			}
		}
	}

	return s
}

// IsParallel reports whether the given stage+step combination should use
// the aligned, prefixed parallel layout.
func (s State) IsParallel(stage, step string) bool {
	return s.StageParallel[stage] && step != ""
}

// Options provides extension points for formatting events. If any field is nil,
// [DefaultOptions] provides the fallback behavior.
type Options struct {
	Palette Palette

	FormatPipelineStart func(ctx context.Context, e pipeline.PipelineStartedEvent, s State, p Palette) string
	FormatPipelineEnd   func(ctx context.Context, e pipeline.Event, duration time.Duration, s State, p Palette) string
	FormatStageStart    func(ctx context.Context, e pipeline.StageStartedEvent, s State, p Palette) string
	FormatStagePass     func(ctx context.Context, e pipeline.StagePassedEvent, d time.Duration, s State, p Palette) string
	FormatStageFail     func(ctx context.Context, e pipeline.StageFailedEvent, d time.Duration, s State, p Palette) string
	FormatStageSkip     func(ctx context.Context, e pipeline.StageSkippedEvent, s State, p Palette) string
	FormatStepPass      func(ctx context.Context, e pipeline.StepPassedEvent, d time.Duration, s State, p Palette) string
	FormatStepFail      func(ctx context.Context, e pipeline.StepFailedEvent, d time.Duration, s State, p Palette) string
	FormatStepSkip      func(ctx context.Context, e pipeline.StepSkippedEvent, s State, p Palette) string
	FormatMessage       func(ctx context.Context, e pipeline.MessageEvent, s State, p Palette) string
	FormatProgress      func(ctx context.Context, e pipeline.ProgressEvent, s State, p Palette) string
	FormatOutput        func(ctx context.Context, e pipeline.OutputEvent, s State, p Palette) string
	FormatCustom        func(ctx context.Context, e pipeline.CustomEvent, s State, p Palette) string
}

func (o *Options) applyDefaults() {
	defaults := DefaultOptions()

	if o.Palette.Primary == nil {
		o.Palette = DefaultPalette()
	}

	if o.FormatPipelineStart == nil {
		o.FormatPipelineStart = defaults.FormatPipelineStart
	}

	if o.FormatPipelineEnd == nil {
		o.FormatPipelineEnd = defaults.FormatPipelineEnd
	}

	if o.FormatStageStart == nil {
		o.FormatStageStart = defaults.FormatStageStart
	}

	if o.FormatStagePass == nil {
		o.FormatStagePass = defaults.FormatStagePass
	}

	if o.FormatStageFail == nil {
		o.FormatStageFail = defaults.FormatStageFail
	}

	if o.FormatStageSkip == nil {
		o.FormatStageSkip = defaults.FormatStageSkip
	}

	if o.FormatStepPass == nil {
		o.FormatStepPass = defaults.FormatStepPass
	}

	if o.FormatStepFail == nil {
		o.FormatStepFail = defaults.FormatStepFail
	}

	if o.FormatStepSkip == nil {
		o.FormatStepSkip = defaults.FormatStepSkip
	}

	if o.FormatMessage == nil {
		o.FormatMessage = defaults.FormatMessage
	}

	if o.FormatProgress == nil {
		o.FormatProgress = defaults.FormatProgress
	}

	if o.FormatOutput == nil {
		o.FormatOutput = defaults.FormatOutput
	}

	if o.FormatCustom == nil {
		o.FormatCustom = defaults.FormatCustom
	}
}
