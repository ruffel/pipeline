package pipeline

import (
	"bufio"
	"context"
	"errors"
	"io"
)

// scannerMaxLine is the maximum token (line) size the scanner will accept.
const scannerMaxLine = 1024 * 1024 // 1 MiB

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

		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), scannerMaxLine)

		for scanner.Scan() {
			EmitOutput(ctx, stream, scanner.Text())
		}

		// Report scanner errors (e.g. bufio.ErrTooLong for lines exceeding
		// scannerMaxLine) through the event system. The only other channel is
		// Close() but callers usually discard the Close() error.
		if err := scanner.Err(); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			EmitWarnf(ctx, "output stream interrupted: %v", err)

			_ = r.Close() // Unblock the writer so the subprocess doesn't hang.
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
