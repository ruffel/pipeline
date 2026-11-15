package terminal

import (
	"strings"

	"github.com/ruffel/pipeline"
)

// layout is what the observer learns from a run's plan: how to name each
// element, and which lines need a prefix because they interleave with others
// in a parallel group.
type layout struct {
	root         string
	descriptions map[string]string
	// within maps the path of every element below a parallel group to its
	// path below the outermost one, which names it there: lines below that
	// group interleave, and the name must tell them apart.
	within map[string]string
	// width maps the path of each outermost parallel group to the widest
	// name below it, so prefixes line up.
	width map[string]int
	// flow is the names of the root's children, in order.
	flow []string
}

func newLayout(spec *pipeline.NodeSpec, root string) layout {
	l := layout{
		root:         root,
		descriptions: map[string]string{},
		within:       map[string]string{},
		width:        map[string]int{},
	}

	if spec == nil {
		return l
	}

	for _, c := range spec.Children {
		l.flow = append(l.flow, c.Name)
	}

	l.walk(*spec, "")

	return l
}

// walk records n and everything below it. parallel is the path of the
// outermost parallel group above n, or empty.
func (l *layout) walk(n pipeline.NodeSpec, parallel string) {
	if n.Description != "" {
		l.descriptions[n.Path] = n.Description
	}

	if parallel != "" {
		rel := strings.TrimPrefix(n.Path, parallel+"/")
		l.within[n.Path] = rel
		l.width[parallel] = max(l.width[parallel], len(rel))
	}

	below := parallel
	if parallel == "" && n.Parallel {
		below = n.Path
	}

	for _, c := range n.Children {
		l.walk(c, below)
	}

	if n.OnFailure != nil {
		l.walk(*n.OnFailure, below)
	}
}

// name is how a line names the element at path: its path below the
// outermost parallel group, or its own name.
func (l *layout) name(path string) string {
	if rel, ok := l.within[path]; ok {
		return rel
	}

	return path[strings.LastIndexByte(path, '/')+1:]
}

// parallel reports whether lines for path interleave with others, and if so
// how wide the prefix naming it must be.
func (l *layout) parallel(path string) (int, bool) {
	group, ok := l.group(path)
	if !ok {
		return 0, false
	}

	return l.width[group], true
}

// group returns the outermost parallel group above path, if there is one.
func (l *layout) group(path string) (string, bool) {
	rel, ok := l.within[path]
	if !ok {
		return "", false
	}

	return strings.TrimSuffix(path, "/"+rel), true
}

// indent is how many levels of the tree's lines come before the line of the
// element at path. Every line below a parallel group sits one level below
// it, since they interleave and name their element in full.
func (l *layout) indent(path string) int {
	if group, ok := l.group(path); ok {
		return l.depth(group)
	}

	return l.depth(path) - 1
}

// depth is how many levels path is below the root: 1 for the root's
// children, 0 for the root.
func (l *layout) depth(path string) int {
	if path == l.root || !strings.HasPrefix(path, l.root+"/") {
		return 0
	}

	return strings.Count(strings.TrimPrefix(path, l.root+"/"), "/") + 1
}

// below is path relative to the root, as the run's summary names it.
func (l *layout) below(path string) string {
	return strings.TrimPrefix(path, l.root+"/")
}
