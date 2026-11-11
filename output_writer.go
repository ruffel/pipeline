package pipeline

import (
	"bufio"
	"context"
	"errors"
	"io"

	"github.com/ruffel/pipeline/internal/linesplit"
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

		split := linesplit.New(maxOutputLine)

		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), maxOutputLine)
		scanner.Split(split.Split)

		for scanner.Scan() {
			if split.Warn() {
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
