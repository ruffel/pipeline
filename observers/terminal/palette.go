package terminal

import "github.com/charmbracelet/lipgloss"

// Palette is the colors and icons an [Observer] uses, so its output can be
// re-branded without changing its layout.
type Palette struct {
	// Primary colors the run's boxes and the mark of a group starting or a
	// gate released.
	Primary lipgloss.TerminalColor
	// Muted dims what is secondary: guides, durations, descriptions.
	Muted lipgloss.TerminalColor
	// Output colors a step's stdout.
	Output lipgloss.TerminalColor

	// Success, Warning and Error color outcomes and what led to them: a
	// stop, a retry, a gate waiting, stderr. Info colors info messages and
	// undone steps.
	Success lipgloss.TerminalColor
	Warning lipgloss.TerminalColor
	Error   lipgloss.TerminalColor
	Info    lipgloss.TerminalColor

	// Icons.
	SuccessIcon string
	ErrorIcon   string
	SkipIcon    string
}

// DefaultPalette returns the palette an [Observer] uses unless given another
// with [WithPalette].
func DefaultPalette() Palette {
	return Palette{
		Primary: lipgloss.Color("63"),
		Muted:   lipgloss.AdaptiveColor{Light: "#999999", Dark: "#6C6C6C"},
		Output:  lipgloss.AdaptiveColor{Light: "#666666", Dark: "#999999"},
		Success: lipgloss.AdaptiveColor{Light: "#43BF6D", Dark: "#73F59F"},
		Warning: lipgloss.Color("#FFC72C"),
		Error:   lipgloss.Color("196"),
		Info:    lipgloss.Color("63"),

		SuccessIcon: "✓",
		ErrorIcon:   "✗",
		SkipIcon:    "⊘",
	}
}
