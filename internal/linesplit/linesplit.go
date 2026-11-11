// Package linesplit splits process output into lines for the executors' output
// writers. Lines end at "\n", "\r\n" or a lone "\r", so progress output that
// redraws a line arrives as one line per update, and lines longer than a limit
// are split into chunks instead of failing.
package linesplit

import (
	"bytes"
	"sync"
)

// Splitter is a [bufio.SplitFunc] with state. Use its Split method with a
// [bufio.Scanner] whose maximum token size is the Splitter's max, or use a
// [Writer].
type Splitter struct {
	max int

	// pieces is how many chunks of the current line have been returned. A
	// non-zero value means the line continues.
	pieces int

	// skipLF: a "\r" was taken as a line end without seeing the next byte,
	// because the buffer was full. A "\n" straight after it belongs to it.
	skipLF bool

	// warn reports that the token just returned is the first proof that its
	// line is longer than max: a second piece of it. A line of exactly max
	// bytes is one chunk followed by a bare line end, and is not warned about.
	warn bool

	// cont reports that the token just returned is a later piece of a line.
	cont bool
}

// New returns a Splitter that splits lines longer than max bytes.
func New(maxLine int) *Splitter {
	return &Splitter{max: maxLine}
}

// Warn reports whether the token just returned is the first proof that its
// line is longer than the limit. It is true once per such line.
func (l *Splitter) Warn() bool {
	return l.warn
}

// Continued reports whether the token just returned is a later piece of a
// line that was split.
func (l *Splitter) Continued() bool {
	return l.cont
}

// Split implements [bufio.SplitFunc]. data must not be longer than the
// Splitter's max, which a [bufio.Scanner] with that maximum token size
// guarantees.
func (l *Splitter) Split(data []byte, atEOF bool) (int, []byte, error) {
	l.warn, l.cont = false, false

	if l.skipLF && len(data) > 0 {
		l.skipLF = false

		if data[0] == '\n' {
			return 1, nil, nil
		}
	}

	i := bytes.IndexAny(data, "\r\n")

	switch {
	case i < 0:
		return l.noBreak(data, atEOF)
	case i == 0 && l.pieces > 0:
		return l.afterChunk(data, atEOF)
	case data[i] == '\n':
		return l.endLine(i+1, data[:i])
	case i+1 < len(data) && data[i+1] == '\n':
		return l.endLine(i+2, data[:i])
	default:
		return l.loneCR(data, i, atEOF)
	}
}

// loneCR handles a line ended by a "\r" at data[i] with no "\n" after it yet.
func (l *Splitter) loneCR(data []byte, i int, atEOF bool) (int, []byte, error) {
	switch {
	case i == 0 && (len(data) > 1 || atEOF):
		// An empty "\r"-terminated line carries nothing; skip it.
		return 1, nil, nil
	case i+1 < len(data) || atEOF:
		return l.endLine(i+1, data[:i])
	case len(data) < l.max:
		// A trailing "\r" may be the first half of "\r\n": wait for more.
		return 0, nil, nil
	default:
		// The buffer is full and ends in "\r": take it now, and drop the
		// "\n" if that is what comes next.
		l.skipLF = true

		return l.endLine(i+1, data[:i])
	}
}

// endLine returns the last piece of a line.
func (l *Splitter) endLine(advance int, token []byte) (int, []byte, error) {
	l.warn = l.pieces == 1
	l.cont = l.pieces > 0
	l.pieces = 0

	return advance, token, nil
}

// afterChunk handles a line break straight after a chunk: the end of a long
// line that split exactly at a chunk boundary. The line has already been
// emitted in full, so the break is consumed without emitting anything.
func (l *Splitter) afterChunk(data []byte, atEOF bool) (int, []byte, error) {
	if len(data) == 1 && data[0] == '\r' && !atEOF {
		// Wait to see whether a "\n" follows.
		return 0, nil, nil
	}

	l.pieces = 0

	if len(data) > 1 && data[0] == '\r' && data[1] == '\n' {
		return 2, nil, nil
	}

	return 1, nil, nil
}

// noBreak handles data that holds no line break yet.
func (l *Splitter) noBreak(data []byte, atEOF bool) (int, []byte, error) {
	switch {
	case len(data) >= l.max:
		l.warn = l.pieces == 1
		l.cont = l.pieces > 0
		l.pieces++

		return l.max, data[:l.max], nil
	case atEOF && len(data) > 0:
		return l.endLine(len(data), data)
	default:
		return 0, nil, nil
	}
}

// Line is one line, or one piece of a long line, from a [Writer].
type Line struct {
	Text string
	// Warn is true on the piece that first proves a line longer than the
	// limit: once per such line.
	Warn bool
	// Continued is true on every piece of a split line but the first.
	Continued bool
}

// Writer splits what is written to it into lines as it arrives, without a
// goroutine, calling emit for each. Call Flush when the writer is done, to
// emit a final line that has no line break. It is safe for concurrent use.
type Writer struct {
	mu   sync.Mutex
	s    *Splitter
	max  int
	buf  []byte
	emit func(Line)
}

// NewWriter returns a Writer that splits lines longer than max bytes.
func NewWriter(maxLine int, emit func(Line)) *Writer {
	return &Writer{s: New(maxLine), max: maxLine, emit: emit}
}

// Write implements [io.Writer]. It never fails.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.buf = append(w.buf, p...)
	w.drain(false)

	return len(p), nil
}

// Flush emits what is left, as a final line.
func (w *Writer) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.drain(true)
}

// drain feeds the buffer to the Splitter at most max bytes at a time, as a
// bufio.Scanner with that maximum token size would.
func (w *Writer) drain(atEOF bool) {
	for {
		data, eof := w.buf, atEOF
		if len(data) > w.max {
			data, eof = data[:w.max], false
		}

		advance, token, _ := w.s.Split(data, eof)
		if advance == 0 {
			break
		}

		w.buf = w.buf[advance:]

		if token != nil {
			w.emit(Line{Text: string(token), Warn: w.s.Warn(), Continued: w.s.Continued()})
		}
	}

	if len(w.buf) == 0 {
		w.buf = nil
	}
}
