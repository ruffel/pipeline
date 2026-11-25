package pipeline

import (
	"fmt"
	"slices"
	"strings"
)

// Describe renders the plan as a tree: every element that may run, with its
// kind, what it can do on resume and rollback, and its description.
//
//	upgrade
//	├── prechecks (parallel, limit 8, continue on error)
//	│   └── ring (observe)
//	├── backup (effect, check, undo) — Snapshot the data directory
//	└── node1
//	    └── maintenance (ensure, until node1 passes)
//
// An Ensure step inside a group is marked with how long it holds: a resume
// applies it again only until its group has passed, because a group that
// passed is skipped whole. An Ensure step at the root has no such mark, and
// is applied on every attempt.
func Describe(p *Plan) string {
	var b strings.Builder

	b.WriteString(describeLine(p.root))
	b.WriteByte('\n')
	describeChildren(&b, p.root, "")

	return b.String()
}

func describeChildren(b *strings.Builder, n *node, indent string) {
	children := n.children
	if n.handler != nil {
		children = append(slices.Clone(children), n.handler)
	}

	for i, c := range children {
		branch, next := "├── ", "│   "
		if i == len(children)-1 {
			branch, next = "└── ", "    "
		}

		var extra []string

		switch {
		case c == n.handler && n.recovers:
			extra = []string{"recovers the group on failure"}
		case c == n.handler:
			extra = []string{"cleans up on failure"}
		case c.kind == KindEnsure && indent != "":
			// Only the root's own children have no indent. In any other
			// group, an Ensure step is applied again on resume only until
			// the group has passed: after that the group is skipped whole.
			extra = []string{"until " + n.name + " passes"}
		}

		b.WriteString(indent + branch + describeLine(c, extra...))
		b.WriteByte('\n')
		describeChildren(b, c, indent+next)
	}
}

func describeLine(n *node, extra ...string) string {
	line := n.name

	if notes := append(describeNotes(n), extra...); len(notes) > 0 {
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

		if n.drain {
			notes = append(notes, "finishes before a stop")
		}

		if n.order != nil {
			notes = append(notes, fmt.Sprintf("in the order %s records as %q", n.order.Path, n.order.Key))
		}
	case KindEffect, KindEnsure, KindObserve:
		notes = append(notes, string(n.kind))

		if n.hasCheck {
			notes = append(notes, "check")
		}

		if n.hasUndo {
			notes = append(notes, "undo")
		}

		if n.irreversible {
			notes = append(notes, "irreversible")
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
