package pipeline

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
)

// maxOutputLine is the longest line emitted as a single [OutputEvent]. Longer
// lines are split into chunks of this size.
const maxOutputLine = 1024 * 1024 // 1 MiB

// OutputWriter returns an [io.WriteCloser] that scans lines and emits each as
// an [OutputEvent] on the given stream. The caller must close the writer when
// done to release the scanner goroutine.
//
// This is for piping subprocess output into the event system:
//
//	cmd := exec.CommandContext(ctx, "go", "test", "./...")
//	cmd.Stdout = pipeline.OutputWriter(ctx, pipeline.Stdout)
//	cmd.Stderr = pipeline.OutputWriter(ctx, pipeline.Stderr)
//	return cmd.Run()
//
// A line ends at "\n", "\r\n" or a lone "\r", so progress output that redraws
// a line with "\r" arrives as one event per update. Empty updates are dropped.
// A line longer than 1 MiB is emitted in 1 MiB chunks, with one warning per
// such line; the writer never stops draining because of line length.
//
// If the context is cancelled before the writer is closed, the read-side of
// the pipe is closed automatically to unblock the scanner.
//
// Close blocks until the scanner goroutine has finished processing all lines.
func OutputWriter(ctx context.Context, stream Stream) io.WriteCloser {
	r, w := io.Pipe()

	done := make(chan struct{})

	// Close the read-side when the context is done so the scanner unblocks.
	stop := context.AfterFunc(ctx, func() {
		_ = r.Close()
	})

	go func() {
		defer close(done)
		defer stop()

		split := &lineSplitter{max: maxOutputLine}

		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), maxOutputLine)
		scanner.Split(split.split)

		for scanner.Scan() {
			if split.warn {
				EmitWarnf(ctx, "output line longer than %d bytes was split into chunks", maxOutputLine)
			}

			EmitOutput(ctx, stream, scanner.Text())
		}

		// The splitter never fails, so any error here comes from the pipe
		// itself. Close the read-side regardless so the writer can't block.
		if err := scanner.Err(); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			_ = r.Close()

			EmitWarnf(ctx, "output stream interrupted: %v", err)
		}
	}()

	return &outputWriter{pw: w, done: done}
}

// lineSplitter is a [bufio.SplitFunc] that ends lines at "\n", "\r\n" or a
// lone "\r", and splits lines longer than max into chunks instead of failing.
type lineSplitter struct {
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
}

func (l *lineSplitter) split(data []byte, atEOF bool) (int, []byte, error) {
	l.warn = false

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
func (l *lineSplitter) loneCR(data []byte, i int, atEOF bool) (int, []byte, error) {
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
func (l *lineSplitter) endLine(advance int, token []byte) (int, []byte, error) {
	l.warn = l.pieces == 1
	l.pieces = 0

	return advance, token, nil
}

// afterChunk handles a line break straight after a chunk: the end of a long
// line that split exactly at a chunk boundary. The line has already been
// emitted in full, so the break is consumed without emitting anything.
func (l *lineSplitter) afterChunk(data []byte, atEOF bool) (int, []byte, error) {
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
func (l *lineSplitter) noBreak(data []byte, atEOF bool) (int, []byte, error) {
	switch {
	case len(data) >= l.max:
		l.warn = l.pieces == 1
		l.pieces++

		return l.max, data[:l.max], nil
	case atEOF && len(data) > 0:
		return l.endLine(len(data), data)
	default:
		return 0, nil, nil
	}
}

// outputWriter wraps an io.PipeWriter and waits for the scanner goroutine
// to finish when Close is called.
type outputWriter struct {
	pw   *io.PipeWriter
	done <-chan struct{}
}

func (w *outputWriter) Write(p []byte) (int, error) {
	return w.pw.Write(p)
}

func (w *outputWriter) Close() error {
	err := w.pw.Close()
	<-w.done // Wait for scanner goroutine to finish.

	return err
}
