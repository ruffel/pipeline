//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package pipeline_test

import (
	"context"
	"testing"

	"github.com/ruffel/pipeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A viewer in another process has the data as soon as the attempt has
// started: here it is read while the attempt's first step is still running.
func TestRunData_IsThereForAViewerThatJoinsARunningAttempt(t *testing.T) {
	t.Parallel()

	path := journalPath(t)

	j, err := pipeline.OpenJournal(path)
	require.NoError(t, err)

	var seen *pipeline.State

	_, err = pipeline.Run(t.Context(), mustPlan(t, pipeline.Effect("a", pipeline.StepFunc(
		func(context.Context, *pipeline.StepContext) error {
			var err error

			seen, err = pipeline.Inspect(path)

			return err
		}))), j, pipeline.WithRunData(layout{Controller: "node1", Nodes: []string{"node1"}}))
	require.NoError(t, err)
	require.NoError(t, j.Close())

	require.Len(t, seen.Attempts, 1)
	assert.Empty(t, seen.Attempts[0].Outcome, "the attempt was still running")
	assert.JSONEq(t, `{"controller":"node1","nodes":["node1"]}`, string(seen.Attempts[0].Data))
}
