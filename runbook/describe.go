package runbook

import (
	"fmt"
	"strings"
)

// Describe renders the plan as a tree: every element that may run, with its
// kind, what it can do on resume and rollback, and its description.
//
//	upgrade
//	├── prechecks (parallel, limit 8, continue on error)
//	│   └── ring (observe)
//	└── backup (effect, check, undo) — Snapshot the data directory
func Describe(p *Plan) string {
	var b strings.Builder

	b.WriteString(describeLine(p.root))
	b.WriteByte('\n')
	describeChildren(&b, p.root, "")

	return b.String()
}

func describeChildren(b *strings.Builder, n *node, indent string) {
	for i, c := range n.children {
		branch, next := "├── ", "│   "
		if i == len(n.children)-1 {
			branch, next = "└── ", "    "
		}

		b.WriteString(indent + branch + describeLine(c))
		b.WriteByte('\n')
		describeChildren(b, c, indent+next)
	}
}

func describeLine(n *node) string {
	line := n.name

	if notes := describeNotes(n); len(notes) > 0 {
		line += " (" + strings.Join(notes, ", ") + ")"
	}

	if n.description != "" {
		line += " — " + n.description
	}

	return line
}

func describeNotes(n *node) []string {
	var notes []string

	switch n.kind {
	case KindGroup:
		if n.parallel {
			notes = append(notes, "parallel")

			if n.limit > 0 {
				notes = append(notes, fmt.Sprintf("limit %d", n.limit))
			}
		}

		switch n.failure {
		case FailureCancel:
			notes = append(notes, "cancel on error")
		case FailureContinue:
			notes = append(notes, "continue on error")
		case FailureStop:
		}
	case KindEffect, KindEnsure, KindObserve:
		notes = append(notes, string(n.kind))

		if n.hasCheck {
			notes = append(notes, "check")
		}

		if n.hasUndo {
			notes = append(notes, "undo")
		}

		if n.tries > 1 {
			notes = append(notes, fmt.Sprintf("%d tries", n.tries))
		}
	case KindGate:
		notes = append(notes, string(n.kind))

		if n.gate.timeout > 0 {
			notes = append(notes, fmt.Sprintf("%s after %s", n.gate.onTimeout, n.gate.timeout))
		}
	}

	return notes
}
