//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package pipeline_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ruffel/pipeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A recorded warning is among what a viewer that joins late is replayed,
// where a best-effort message is not, and its text is redacted.
func TestRecordWarning_IsReplayedToALateViewerAndRedacted(t *testing.T) {
	t.Parallel()

	path := journalPath(t)

	j, err := pipeline.OpenJournal(path)
	require.NoError(t, err)

	_, err = pipeline.Run(t.Context(), mustPlan(t, pipeline.Effect("a", pipeline.StepFunc(
		func(_ context.Context, s *pipeline.StepContext) error {
			s.Warn("a message")

			return s.RecordWarning("the token hunter2 was left in place")
		}))), j, pipeline.WithRedactor(func(s string) string { return strings.ReplaceAll(s, "hunter2", "***") }))
	require.NoError(t, err)
	require.NoError(t, j.Close())

	var seen []string

	watch := pipeline.ObserverFunc(func(_ context.Context, e pipeline.Event) {
		if e.Kind == pipeline.EventWarning || e.Kind == pipeline.EventMessage {
			seen = append(seen, string(e.Kind)+": "+e.Message)
		}
	})

	require.NoError(t, pipeline.Follow(t.Context(), path, pipeline.FollowOptions{History: pipeline.HistoryLifecycle}, watch))
	assert.Equal(t, []string{"warning: the token *** was left in place"}, seen)

	state, err := pipeline.Inspect(path)
	require.NoError(t, err)
	assert.Equal(t, []string{"p/a: the token *** was left in place"}, warned(state))
}
