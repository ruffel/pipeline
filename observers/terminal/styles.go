package terminal

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/ruffel/pipeline"
)

// boxWidth is the standard width used for pipeline start/end boxes.
const boxWidth = 72

// DefaultPalette returns the default semantic color palette.
func DefaultPalette() Palette {
	return Palette{
		Primary:    lipgloss.Color("63"),
		Accent:     lipgloss.AdaptiveColor{Light: "#43BF6D", Dark: "#73F59F"},
		Foreground: lipgloss.Color(""),
		Muted:      lipgloss.AdaptiveColor{Light: "#D9DCCF", Dark: "#383838"},
		Success:    lipgloss.AdaptiveColor{Light: "#43BF6D", Dark: "#73F59F"},
		Warning:    lipgloss.Color("#FFC72C"),
		Error:      lipgloss.Color("196"),
		Info:       lipgloss.Color("63"),

		SuccessIcon: "✓",
		ErrorIcon:   "✗",
		WarnIcon:    "!",
		InfoIcon:    "i",
		SkipIcon:    "⊘",
	}
}

// DefaultOptions returns the standard terminal layout implementation.
func DefaultOptions() Options {
	return Options{
		FormatPipelineStart: formatPipelineStart,
		FormatPipelineEnd:   formatPipelineEnd,
		FormatStageStart:    formatStageStart,
		FormatStepPass:      formatStepPass,
		FormatStepFail:      formatStepFail,
		FormatStepSkip:      formatStepSkip,
		FormatMessage:       formatMessage,
		FormatProgress:      formatProgress,
		FormatOutput:        formatOutput,
	}
}

// ---------------------------------------------------------------------------
// Rendering helpers
// ---------------------------------------------------------------------------

// render provides terse, palette-aware helper methods for the common styled
// atoms that appear across all formatters. This keeps the formatter functions
// focused on assembly logic rather than style construction.
type render struct {
	p Palette
	s State
}

// colored renders text with the given color.
func (r render) colored(text string, c lipgloss.TerminalColor) string {
	return lipgloss.NewStyle().Foreground(c).Render(text)
}

// muted renders text in the muted/dimmed color.
func (r render) muted(text string) string { return r.colored(text, r.p.Muted) }

// pipe renders the standard "│" separator in the muted color.
func (r render) pipe() string { return r.muted("│") }

// stderrPipe renders the thick "┃" separator in the error color.
func (r render) stderrPipe() string { return r.colored("┃", r.p.Error) }

// prefix renders "[step-name  ]" left-padded to the max step width.
func (r render) prefix(name string) string {
	return r.muted(fmt.Sprintf("[%-[1]*[2]s]", r.s.MaxStepLen, name))
}

// duration renders a step duration like " (803ms)" in the muted color.
func (r render) duration(d time.Duration) string {
	return r.muted(fmt.Sprintf(" (%v)", d.Round(time.Millisecond)))
}

// marker renders the progress marker "◷" in the muted color.
func (r render) marker() string { return r.muted("◷") }

// ---------------------------------------------------------------------------
// Box styles (pipeline start/end, stage headers)
// ---------------------------------------------------------------------------

func pipelineBox(p Palette) (lipgloss.Style, lipgloss.Style) {
	border := lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(p.Primary).
		Width(boxWidth).Padding(0, 1)
	header := lipgloss.NewStyle().
		Bold(true).PaddingBottom(1).
		Width(boxWidth).Align(lipgloss.Center)

	return border, header
}

// ---------------------------------------------------------------------------
// Formatters
// ---------------------------------------------------------------------------

func formatPipelineStart(_ context.Context, e pipeline.PipelineStartedEvent, _ State, p Palette) string {
	border, header := pipelineBox(p)
	r := render{p: p}

	title := "Pipeline: " + e.Definition.Name

	stageNames := make([]string, 0, len(e.Definition.Stages))
	for _, stage := range e.Definition.Stages {
		stageNames = append(stageNames, stage.Name)
	}

	flow := r.muted(strings.Join(stageNames, " ➔  "))

	return border.Render(lipgloss.JoinVertical(lipgloss.Top,
		header.Render(title),
		lipgloss.NewStyle().Width(boxWidth).Align(lipgloss.Center).Render(flow),
	))
}

func formatPipelineEnd(_ context.Context, e pipeline.Event, duration time.Duration, s State, p Palette) string {
	border, header := pipelineBox(p)
	r := render{p: p}

	var title string

	switch e.(type) {
	case pipeline.PipelinePassedEvent:
		title = "Pipeline Complete"
	case pipeline.PipelineFailedEvent:
		title = "Pipeline Failed"
	}

	var body []string

	if len(s.FailedSteps) > 0 {
		body = append(body, "Failed Steps:")

		for _, fs := range s.FailedSteps {
			body = append(body, r.colored("  ✗ "+fs, p.Error))
		}

		body = append(body, "")
	}

	body = append(body, fmt.Sprintf("Total time: %v", duration.Round(time.Millisecond)))

	return border.Render(lipgloss.JoinVertical(lipgloss.Top,
		header.Render(title),
		lipgloss.JoinVertical(lipgloss.Left, body...),
	))
}

func formatStageStart(_ context.Context, e pipeline.StageStartedEvent, _ State, p Palette) string {
	border := lipgloss.NewStyle().
		BorderBottom(true).BorderTop(true).
		BorderForeground(p.Primary).BorderStyle(lipgloss.RoundedBorder()).
		Padding(0, 1)

	return border.Render("Stage: " + e.Stage)
}

func formatStepPass(_ context.Context, e pipeline.StepPassedEvent, d time.Duration, s State, p Palette) string {
	r := render{p: p, s: s}
	icon := r.colored(p.SuccessIcon, p.Success)
	dur := r.duration(d)

	if s.IsParallel(e.Stage, e.Step) {
		return fmt.Sprintf("%s %s %s %s%s", r.prefix(e.Step), r.pipe(), icon, e.Step, dur)
	}

	return icon + " " + e.Step + dur
}

func formatStepFail(_ context.Context, e pipeline.StepFailedEvent, d time.Duration, s State, p Palette) string {
	r := render{p: p, s: s}
	icon := r.colored(p.ErrorIcon, p.Error)
	errMsg := r.colored(e.Err.Error(), p.Error)
	dur := r.duration(d)

	if s.IsParallel(e.Stage, e.Step) {
		return fmt.Sprintf("%s %s %s %s - %s%s",
			r.prefix(e.Step), r.pipe(), icon, e.Step, errMsg, dur)
	}

	return icon + " " + e.Step + " - " + e.Err.Error() + dur
}

func formatStepSkip(_ context.Context, e pipeline.StepSkippedEvent, s State, p Palette) string {
	r := render{p: p, s: s}
	icon := r.muted(p.SkipIcon)
	reason := r.muted(e.Reason)

	if s.IsParallel(e.Stage, e.Step) {
		return fmt.Sprintf("%s %s %s %s - %s",
			r.prefix(e.Step), r.pipe(), icon, e.Step, reason)
	}

	return icon + " " + e.Step + " - skipped: " + e.Reason
}

func formatMessage(_ context.Context, e pipeline.MessageEvent, s State, p Palette) string {
	r := render{p: p, s: s}

	var level string

	switch e.Level {
	case pipeline.LevelWarn:
		level = r.colored("WARN", p.Warning)
	case pipeline.LevelDebug:
		level = r.muted("DEBUG")
	case pipeline.LevelInfo:
		level = r.colored("INFO", p.Info)
	}

	if s.IsParallel(e.Stage, e.Step) {
		return fmt.Sprintf("%s %s [%s] %s", r.prefix(e.Step), r.pipe(), level, e.Message)
	}

	return fmt.Sprintf("- [%s] %s", level, e.Message)
}

func formatProgress(_ context.Context, e pipeline.ProgressEvent, s State, p Palette) string {
	r := render{p: p, s: s}
	progress := r.muted(fmt.Sprintf("(%d/%d)", e.Current, e.Total))

	if s.IsParallel(e.Stage, e.Step) {
		return fmt.Sprintf("%s %s %s %s %s",
			r.prefix(e.Step), r.pipe(), r.marker(), e.Message, progress)
	}

	return fmt.Sprintf("  %s %s %s", r.marker(), e.Message, progress)
}

func formatOutput(_ context.Context, e pipeline.OutputEvent, s State, p Palette) string {
	r := render{p: p, s: s}
	line := e.Line

	if s.IsParallel(e.Stage, e.Step) {
		var pipe string
		if e.Stream == pipeline.Stderr {
			pipe = r.stderrPipe()
			line = r.colored(line, p.Warning)
		} else {
			pipe = r.pipe()
			line = r.muted(line)
		}

		return fmt.Sprintf("%s %s %s", r.prefix(e.Step), pipe, line)
	}

	// Sequential stages: raw, copyable output.
	if e.Stream == pipeline.Stderr {
		return r.colored(line, p.Warning)
	}

	return line
}
