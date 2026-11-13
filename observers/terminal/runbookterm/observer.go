// Package runbookterm renders a [runbook] run in a terminal: a box when the
// run starts and finishes, and between them the plan as a tree, one line per
// event that matters to an operator. It writes lines and never rewrites the
// screen, so its output reads the same in a log, a pipe or a viewer that
// attached late with [runbook.Follow].
//
//	╭──────────────────────────────────────╮
//	│            Runbook: upgrade          │
//	│      prechecks ➔ nodes ➔ finalise    │
//	╰──────────────────────────────────────╯
//	▶ prechecks — Read-only checks
//	│ ✓ ring (1.8s)
//	│ ✓ disks · 3 passed (900ms)
//	✓ prechecks (2.7s)
//	▶ nodes
//	│ ▶ node1
//	│ ▶ node2
//	│ │ [node1/packages] │ Unpacking hyperstore (8.2) ...
//	│ │ [node2/packages] │ Unpacking hyperstore (8.2) ...
//	│ ✓ node1/packages (40s)
//	│ ✓ node1 (41s)
//	│ ✗ node2/packages — exit status 1 (38s)
//	│ ✗ node2 (38s)
//	✗ nodes (41s)
//
// Lines from below a parallel group interleave, so they all sit one level
// below it and name their element by its path below the group. Item results are rows under their
// step, and the step's line counts them.
//
// The observer writes as it is called. Wrap it in [runbook.AsyncObserver] so
// a slow or lost terminal cannot hold up a run.
package runbookterm

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/ruffel/pipeline/observers/terminal"
	"github.com/ruffel/pipeline/runbook"
)

// Observer renders a runbook run in a terminal. See the package
// documentation for what it shows.
type Observer struct {
	mu sync.Mutex
	w  io.Writer
	p  terminal.Palette
	r  *lipgloss.Renderer

	// The run being rendered, reset by each run.started.
	layout   layout
	rollback bool
	steps    map[string]*stepState
	failures []string
	// cancelled is the steps a cancel cut short, which didn't fail on
	// their own.
	cancelled []string
}

// stepState is what the observer keeps about a step while it runs.
type stepState struct {
	// opened records that a line naming the step has been written, so lines
	// under it can follow.
	opened bool
	// items holds the latest status for each target the step reported on.
	items map[string]runbook.ItemStatus
}

// Option configures an [Observer].
type Option func(*Observer)

// WithPalette sets the colors and icons the observer uses, in place of
// [terminal.DefaultPalette].
func WithPalette(p terminal.Palette) Option {
	return func(o *Observer) { o.p = p }
}

// New returns an Observer writing to w. Colors are used only if w is a
// terminal that supports them.
func New(w io.Writer, opts ...Option) *Observer {
	o := &Observer{
		w:     w,
		p:     terminal.DefaultPalette(),
		r:     lipgloss.NewRenderer(w),
		steps: map[string]*stepState{},
	}

	for _, opt := range opts {
		opt(o)
	}

	// Adaptive colors depend on the terminal's background, which lipgloss
	// asks the terminal for when first needed; the answer arrives on stdin.
	// Asking now, before a run starts, keeps the answer from reaching a
	// StdinApprover reading stdin at a gate.
	o.r.HasDarkBackground()

	return o
}

// OnEvent implements [runbook.Observer].
func (o *Observer) OnEvent(_ context.Context, e runbook.Event) {
	o.mu.Lock()
	defer o.mu.Unlock()

	switch e.Kind {
	case runbook.EventRunStarted:
		o.runStarted(e)
	case runbook.EventRunFinished:
		o.runFinished(e)
	case runbook.EventGroupStarted:
		o.line(e.Path, o.color("▶", o.p.Primary)+" "+o.layout.name(e.Path)+o.description(e.Path))
	case runbook.EventGroupReset:
		o.line(e.Path, o.color("↺ "+o.layout.name(e.Path)+" reset", o.p.Warning)+
			o.muted(": a resume runs it again from the start"))
	case runbook.EventStepStarted, runbook.EventUndoStarted:
		if o.layout.descriptions[e.Path] != "" {
			o.open(e.Path)
		}
	case runbook.EventStepFinished, runbook.EventGroupFinished:
		o.finished(e)
	case runbook.EventUndoFinished:
		o.undoFinished(e)
	case runbook.EventItem:
		o.item(e)
	case runbook.EventOutput:
		o.output(e)
	case runbook.EventMessage, runbook.EventProgress, runbook.EventCustom, runbook.EventStepRetry:
		o.detail(e.Path, o.detailText(e), false)
	case runbook.EventStopRequested:
		o.stopRequested(e)
	case runbook.EventGateWaiting:
		o.step(e.Path).opened = true
		o.line(e.Path, o.color("⏸ "+o.layout.name(e.Path), o.p.Warning)+dash(e.Prompt)+o.muted(deadline(e.Deadline)))
	case runbook.EventGateReleased:
		o.line(e.Path, o.color("▶", o.p.Primary)+" "+o.layout.name(e.Path)+" "+o.muted(released(e)))
	case runbook.EventStepRunning, runbook.EventValue:
		// Bookkeeping for resume and rollback; nothing for a reader.
	}
}

// runStarted takes in the run's plan and writes the opening box.
func (o *Observer) runStarted(e runbook.Event) {
	o.layout = newLayout(e.Plan, e.Path)
	o.rollback = e.Rollback
	o.steps = map[string]*stepState{}
	o.failures = nil
	o.cancelled = nil

	lines := []string{o.r.NewStyle().Bold(true).Render("Runbook: " + e.Path)}

	if d := o.layout.descriptions[e.Path]; d != "" {
		lines = append(lines, o.muted(d))
	}

	var about []string

	if e.Rollback {
		about = append(about, "rollback")
	}

	if e.Attempt > 1 {
		about = append(about, fmt.Sprintf("attempt %d", e.Attempt))
	}

	if e.Forced {
		about = append(about, "forced")
	}

	if len(about) > 0 {
		lines = append(lines, o.muted(strings.Join(about, " · ")))
	}

	if len(o.layout.flow) > 0 {
		lines = append(lines, o.muted(strings.Join(o.layout.flow, " ➔ ")))
	}

	o.box(lipgloss.Center, lines)
}

// runFinished writes the closing box: the outcome, what failed, and how
// long the run took.
func (o *Observer) runFinished(e runbook.Event) {
	what := "Runbook"
	if o.rollback {
		what = "Rollback"
	}

	title := o.r.NewStyle().Bold(true).Foreground(o.outcomeColor(e.Outcome)).
		Render(what + " " + string(e.Outcome))
	lines := []string{title, ""}

	lines = append(lines, o.list("Failed:", o.failures, o.p.ErrorIcon, o.p.Error)...)
	lines = append(lines, o.list("Cancelled:", o.cancelled, "■", o.p.Warning)...)

	if len(o.failures)+len(o.cancelled) == 0 && e.Error != nil {
		lines = append(lines, o.color(e.Error.Message, o.p.Error), "")
	}

	if e.Outcome == runbook.OutcomeStopped || e.Outcome == runbook.OutcomeCancelled {
		lines = append(lines, o.muted("Running it again resumes where it stopped."), "")
	}

	lines = append(lines, fmt.Sprintf("Total time: %s", time.Duration(e.DurationMs)*time.Millisecond))

	o.box(lipgloss.Left, lines)
}

// list is a heading and its entries for the closing box, or nothing if
// there are none. An entry's lines after the first are indented under it.
func (o *Observer) list(heading string, entries []string, icon string, c lipgloss.TerminalColor) []string {
	if len(entries) == 0 {
		return nil
	}

	out := []string{heading}

	for _, e := range entries {
		out = append(out, o.color("  "+icon+" "+strings.ReplaceAll(e, "\n", "\n    "), c))
	}

	return append(out, "")
}

// finished writes a step's or group's outcome.
func (o *Observer) finished(e runbook.Event) {
	text := o.mark(e.Outcome) + " " + o.layout.name(e.Path) + dash(string(e.Reason)) + o.errorText(e.Error)

	if st := o.steps[e.Path]; st != nil {
		text += o.muted(counts(st.items))
		delete(o.steps, e.Path)
	}

	o.line(e.Path, text+o.elapsed(e.DurationMs))

	if e.Kind != runbook.EventStepFinished || e.Outcome != runbook.OutcomeFailed {
		return
	}

	if e.Error != nil && e.Error.Kind == runbook.ErrorCancelled {
		// A cancel alone says nothing; one that ended a retry's backoff
		// keeps the try's error, which does.
		entry := o.layout.below(e.Path)
		if e.Error.Message != context.Canceled.Error() {
			entry += message(e.Error)
		}

		o.cancelled = append(o.cancelled, entry)
	} else {
		o.failures = append(o.failures, o.layout.below(e.Path)+message(e.Error))
	}
}

// undoFinished writes what a rollback did with a step.
func (o *Observer) undoFinished(e runbook.Event) {
	var items string

	if st := o.steps[e.Path]; st != nil {
		items = o.muted(counts(st.items))
		delete(o.steps, e.Path)
	}

	var done string

	switch e.Outcome { //nolint:exhaustive // An undo is undone, skipped or failed.
	case runbook.OutcomeUndone:
		done = "undone"
	case runbook.OutcomeSkipped:
		done = "not undone"
	default:
		done = "undo failed"

		o.failures = append(o.failures, "undo of "+o.layout.below(e.Path)+message(e.Error))
	}

	o.line(e.Path, o.mark(e.Outcome)+" "+o.layout.name(e.Path)+" "+done+dash(string(e.Reason))+
		o.errorText(e.Error)+items+o.elapsed(e.DurationMs))
}

// item writes a step's result for one target as a row under the step.
func (o *Observer) item(e runbook.Event) {
	st := o.step(e.Path)
	if st.items == nil {
		st.items = map[string]runbook.ItemStatus{}
	}

	st.items[e.Item] = e.Status

	var icon string

	switch e.Status {
	case runbook.ItemPassed:
		icon = o.color(o.p.SuccessIcon, o.p.Success)
	case runbook.ItemFailed:
		icon = o.color(o.p.ErrorIcon, o.p.Error)
	case runbook.ItemSkipped:
		icon = o.muted(o.p.SkipIcon)
	default:
		icon = o.muted("?")
	}

	o.detail(e.Path, icon+" "+e.Item+dash(e.Message), false)
}

// output writes a line a step printed: as it is in a sequential group, so it
// can be copied, and named in a parallel one. Standard error is colored.
func (o *Observer) output(e runbook.Event) {
	stderr := e.Stream == "stderr"

	text := o.color(e.Line, o.p.Output)
	if stderr {
		text = o.color(e.Line, o.p.Warning)
	}

	o.detail(e.Path, text, stderr)
}

func (o *Observer) stopRequested(e runbook.Event) {
	by := ""
	if e.Who != "" {
		by = " by " + e.Who
	}

	text := "■ stop requested" + by + "; running steps will finish"
	if e.Mode == runbook.StopCancel {
		text = "■ cancel requested" + by + "; running steps are cancelled"
	}

	o.println(o.color(text, o.p.Warning))
}

// detailText is what a message, progress report, custom event or retry
// shows under its step.
func (o *Observer) detailText(e runbook.Event) string {
	switch e.Kind { //nolint:exhaustive // Only the kinds shown under a step.
	case runbook.EventMessage:
		if e.Level == runbook.LevelWarn {
			return "[" + o.color("WARN", o.p.Warning) + "] " + e.Message
		}

		return "[" + o.color("INFO", o.p.Info) + "] " + e.Message
	case runbook.EventProgress:
		if e.Total > 0 {
			return o.muted("◷") + " " + e.Message + o.muted(fmt.Sprintf(" (%d/%d)", e.Current, e.Total))
		}

		return o.muted("◷") + " " + e.Message
	case runbook.EventStepRetry:
		return o.color(fmt.Sprintf("↻ try %d failed%s; retrying", e.Try, message(e.Error)), o.p.Warning)
	default:
		return o.muted(fmt.Sprintf("[%s] %s", e.Type, e.Data))
	}
}

// detail writes text under the element at path, one line per line of text.
// In a sequential group the element's own line comes first, written now if
// it hasn't been; in a parallel group every line is prefixed with the
// element's name. Text with no path, or the root's, belongs to the run.
func (o *Observer) detail(path, text string, stderr bool) {
	prefix := o.guides(o.layout.indent(path) + 1)

	if width, parallel := o.layout.parallel(path); parallel {
		pipe := o.muted("│")
		if stderr {
			pipe = o.color("┃", o.p.Error)
		}

		prefix += o.muted(fmt.Sprintf("[%-*s]", width, o.layout.name(path))) + " " + pipe + " "
	} else if path != "" && path != o.layout.root && !o.step(path).opened {
		o.open(path)
	}

	for l := range strings.SplitSeq(text, "\n") {
		o.println(prefix + l)
	}
}

// open writes the line that names a step before anything under it.
func (o *Observer) open(path string) {
	o.step(path).opened = true
	o.line(path, o.muted("·")+" "+o.layout.name(path)+o.description(path))
}

// step returns what the observer keeps about the step at path.
func (o *Observer) step(path string) *stepState {
	st := o.steps[path]
	if st == nil {
		st = &stepState{}
		o.steps[path] = st
	}

	return st
}

// line writes a line for the element at path, under its parent. Lines after
// the first, such as those of a joined error, hang below it in the tree.
// They need no name: one event's lines are written together.
func (o *Observer) line(path, text string) {
	guides := o.guides(o.layout.indent(path))

	first, rest, more := strings.Cut(text, "\n")
	o.println(guides + first)

	if more {
		for l := range strings.SplitSeq(rest, "\n") {
			o.println(guides + "  " + l)
		}
	}
}

// guides draws the tree's lines for n levels.
func (o *Observer) guides(n int) string {
	if n <= 0 {
		return ""
	}

	return o.muted(strings.Repeat("│ ", n))
}

const boxWidth = 72

func (o *Observer) box(align lipgloss.Position, lines []string) {
	inner := o.r.NewStyle().Width(boxWidth - 2).Align(align)
	for i, l := range lines {
		lines[i] = inner.Render(l)
	}

	border := o.r.NewStyle().BorderStyle(lipgloss.RoundedBorder()).BorderForeground(o.p.Primary).
		Width(boxWidth).Padding(0, 1)

	o.println(border.Render(lipgloss.JoinVertical(lipgloss.Left, lines...)))
}

func (o *Observer) println(s string) {
	_, _ = fmt.Fprintln(o.w, s)
}

// color colors each line of s on its own, so the lines can be written with
// a prefix each, and lipgloss doesn't pad them to one width.
func (o *Observer) color(s string, c lipgloss.TerminalColor) string {
	style := o.r.NewStyle().Foreground(c)

	ls := strings.Split(s, "\n")
	for i, l := range ls {
		ls[i] = style.Render(l)
	}

	return strings.Join(ls, "\n")
}

func (o *Observer) muted(s string) string {
	if s == "" {
		return ""
	}

	return o.color(s, o.p.Muted)
}

func (o *Observer) description(path string) string {
	return o.muted(dash(o.layout.descriptions[path]))
}

func (o *Observer) errorText(e *runbook.ErrorInfo) string {
	if e == nil {
		return ""
	}

	return o.color(dash(e.Message), o.p.Error)
}

func (o *Observer) elapsed(ms int64) string {
	if ms <= 0 {
		return ""
	}

	return o.muted(fmt.Sprintf(" (%s)", time.Duration(ms)*time.Millisecond))
}

func (o *Observer) mark(out runbook.Outcome) string {
	switch out {
	case runbook.OutcomePassed:
		return o.color(o.p.SuccessIcon, o.p.Success)
	case runbook.OutcomeSkipped:
		return o.muted(o.p.SkipIcon)
	case runbook.OutcomeStopped:
		return o.color("■", o.p.Warning)
	case runbook.OutcomeFailed, runbook.OutcomeCancelled:
		return o.color(o.p.ErrorIcon, o.p.Error)
	case runbook.OutcomeUndone:
		return o.color("↶", o.p.Info)
	default:
		return o.muted("?")
	}
}

func (o *Observer) outcomeColor(out runbook.Outcome) lipgloss.TerminalColor {
	switch out { //nolint:exhaustive // Runs pass, stop, or fail.
	case runbook.OutcomePassed:
		return o.p.Success
	case runbook.OutcomeStopped:
		return o.p.Warning
	default:
		return o.p.Error
	}
}

// counts sums up a step's item results, such as " · 3 passed, 1 failed".
func counts(items map[string]runbook.ItemStatus) string {
	if len(items) == 0 {
		return ""
	}

	n := map[runbook.ItemStatus]int{}
	for _, s := range items {
		n[s]++
	}

	var parts []string

	for _, s := range []runbook.ItemStatus{runbook.ItemPassed, runbook.ItemFailed, runbook.ItemSkipped} {
		if n[s] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n[s], s))
		}
	}

	return " · " + strings.Join(parts, ", ")
}

// released says what released a gate.
func released(e runbook.Event) string {
	switch {
	case e.Policy != "":
		return "timed out: " + string(e.Policy)
	case e.Who != "":
		return fmt.Sprintf("approved by %s (%s)", e.Who, e.By)
	default:
		return "approved (" + e.By + ")"
	}
}

// deadline says when a gate times out, if it does.
func deadline(t time.Time) string {
	if t.IsZero() {
		return ""
	}

	return " (waiting until " + t.Format(time.RFC3339) + ")"
}

func message(e *runbook.ErrorInfo) string {
	if e == nil {
		return ""
	}

	return dash(e.Message)
}

func dash(s string) string {
	if s == "" {
		return ""
	}

	return " — " + s
}
