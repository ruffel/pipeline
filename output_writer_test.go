package pipeline_test

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"testing"

	"github.com/ruffel/pipeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOutputWriter_EmitsLines(t *testing.T) {
	t.Parallel()

	obs := &recordingObserver{}
	em := pipeline.NewEmitter(obs.OnEvent, pipeline.Location{
		Pipeline: "p",
		Stage:    "s",
		Step:     "step",
	})

	ctx := pipeline.WithEmitter(t.Context(), em)
	w := pipeline.OutputWriter(ctx, pipeline.Stdout)

	_, err := w.Write([]byte("line one\nline two\nline three\n"))
	require.NoError(t, err)
	require.NoError(t, w.Close())

	// Give the scanner goroutine time to process.
	// Closing the writer signals EOF, scanner exits.
	var lines []string

	for _, e := range obs.events {
		if oe, ok := e.(pipeline.OutputEvent); ok {
			lines = append(lines, oe.Line)
		}
	}

	assert.Equal(t, []string{"line one", "line two", "line three"}, lines)
}

func TestOutputWriter_NoEmitter(t *testing.T) {
	t.Parallel()

	w := pipeline.OutputWriter(t.Context(), pipeline.Stdout)

	_, err := w.Write([]byte("hello\n"))
	require.NoError(t, err)
	require.NoError(t, w.Close())
}

func TestOutputWriter_ContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())

	w := pipeline.OutputWriter(ctx, pipeline.Stdout)

	cancel()

	_, _ = w.Write([]byte("after cancel\n"))
	_ = w.Close()
}

func TestOutputWriter_LongLineIsSplitNotFatal(t *testing.T) {
	t.Parallel()

	obs, w := newOutputWriter(t)

	long := strings.Repeat("x", 2*1024*1024+10)

	_, err := w.Write([]byte("before\n" + long + "\n"))
	require.NoError(t, err)

	_, err = w.Write([]byte("after\n"))
	require.NoError(t, err)
	require.NoError(t, w.Close())

	lines, warnings := outputOf(obs)

	require.Len(t, lines, 5)
	assert.Equal(t, "before", lines[0])
	assert.Equal(t, long, lines[1]+lines[2]+lines[3])
	assert.Len(t, lines[1], 1024*1024)
	assert.Equal(t, "after", lines[4])
	assert.Len(t, warnings, 1, "one warning per over-long line")
}

func TestOutputWriter_LongLineOnChunkBoundary(t *testing.T) {
	t.Parallel()

	obs, w := newOutputWriter(t)

	long := strings.Repeat("x", 2*1024*1024)

	// The second line's CRLF arrives split across writes.
	for _, chunk := range []string{long + "\nafter\n", long + "\r", "\nend\n"} {
		_, err := w.Write([]byte(chunk))
		require.NoError(t, err)
	}

	require.NoError(t, w.Close())

	lines, _ := outputOf(obs)
	require.Len(t, lines, 6, "no empty line after the last chunk")
	assert.Equal(t, "after", lines[2])
	assert.Equal(t, "end", lines[5])
}

func TestOutputWriter_WarnsOncePerLongLine(t *testing.T) {
	t.Parallel()

	obs, w := newOutputWriter(t)

	long := strings.Repeat("x", 1024*1024+1)

	_, err := w.Write([]byte(long + "\nshort\n" + long + "\n"))
	require.NoError(t, err)
	require.NoError(t, w.Close())

	_, warnings := outputOf(obs)
	assert.Len(t, warnings, 2)
}

func TestOutputWriter_CarriageReturnEndsLine(t *testing.T) {
	t.Parallel()

	obs, w := newOutputWriter(t)

	// Progress redraws, a CRLF line, a trailing CR with no newline after it,
	// and a CR split across two writes.
	for _, chunk := range []string{"\r10%\r20%\r", "100%\r\ncrlf\r\nlast\r", "\n", "tail"} {
		_, err := w.Write([]byte(chunk))
		require.NoError(t, err)
	}

	require.NoError(t, w.Close())

	lines, warnings := outputOf(obs)
	assert.Equal(t, []string{"10%", "20%", "100%", "crlf", "last", "tail"}, lines)
	assert.Empty(t, warnings)
}

func TestOutputWriter_ProgressWithoutNewlineKeepsDraining(t *testing.T) {
	t.Parallel()

	obs, w := newOutputWriter(t)

	// About 1.2 MiB of "\r" progress with no newline: one huge line before
	// "\r" ended lines, which stopped the stream.
	for i := range 60000 {
		_, err := fmt.Fprintf(w, "\rdownloading %6d/60000", i)
		require.NoError(t, err)
	}

	_, err := w.Write([]byte("\ndone\n"))
	require.NoError(t, err)
	require.NoError(t, w.Close())

	lines, warnings := outputOf(obs)
	require.Len(t, lines, 60001)
	assert.Equal(t, "downloading  59999/60000", lines[59999])
	assert.Equal(t, "done", lines[60000])
	assert.Empty(t, warnings)
}

func TestOutputWriter_SubprocessSurvivesLongLine(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("needs sh")
	}

	obs, w := newOutputWriter(t)

	cmd := exec.CommandContext(t.Context(), "sh", "-c",
		`echo before; head -c 2097152 /dev/zero | tr '\0' a; echo; echo after`)
	cmd.Stdout = w

	require.NoError(t, cmd.Run(), "the subprocess must not see a broken pipe")
	require.NoError(t, w.Close())

	lines, _ := outputOf(obs)
	require.NotEmpty(t, lines)
	assert.Equal(t, "before", lines[0])
	assert.Equal(t, "after", lines[len(lines)-1])
}

func newOutputWriter(t *testing.T) (*recordingObserver, io.WriteCloser) {
	t.Helper()

	obs := &recordingObserver{}
	em := pipeline.NewEmitter(obs.OnEvent, pipeline.Location{Pipeline: "p", Stage: "s", Step: "step"})

	return obs, pipeline.OutputWriter(pipeline.WithEmitter(t.Context(), em), pipeline.Stdout)
}

// outputOf returns the output lines and warning messages recorded, in order.
func outputOf(obs *recordingObserver) ([]string, []string) {
	obs.mu.Lock()
	defer obs.mu.Unlock()

	var lines, warnings []string

	for _, e := range obs.events {
		switch ev := e.(type) {
		case pipeline.OutputEvent:
			lines = append(lines, ev.Line)
		case pipeline.MessageEvent:
			if ev.Level == pipeline.LevelWarn {
				warnings = append(warnings, ev.Message)
			}
		}
	}

	return lines, warnings
}
