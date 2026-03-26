package pipeline_test

import (
	"context"
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
