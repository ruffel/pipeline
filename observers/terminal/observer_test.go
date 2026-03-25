package terminal_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/ruffel/pipeline"
	termobs "github.com/ruffel/pipeline/observers/terminal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestObserver_Integration(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	obs := termobs.New(&buf)
	ex := pipeline.NewExecutor(obs)

	p := pipeline.NewPipeline("deploy",
		pipeline.NewStage("build",
			pipeline.NewStep("api", func(_ context.Context) error { return nil }),
		),
	)

	err := ex.Run(t.Context(), p)
	require.NoError(t, err)

	out := buf.String()

	// Just checking for some known styled outputs rather than exact byte matching
	// because lipgloss outputs raw ANSI escapes which can vary slightly by env.
	assert.Contains(t, out, "Pipeline: deploy")
	assert.Contains(t, out, "Stage: build")
	assert.Contains(t, out, "api") // step name
	assert.Contains(t, out, "Pipeline Complete")
}
