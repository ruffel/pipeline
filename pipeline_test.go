package pipeline_test

import (
	"fmt"
	"testing"

	"github.com/ruffel/pipeline"
	"github.com/stretchr/testify/assert"
)

func TestSentinelErrors(t *testing.T) {
	t.Parallel()

	sentinels := []struct {
		name string
		err  error
	}{
		{"ErrSkipPipeline", pipeline.ErrSkipPipeline},
		{"ErrSkipStage", pipeline.ErrSkipStage},
	}

	t.Run("survives wrapping", func(t *testing.T) {
		t.Parallel()

		for _, tt := range sentinels {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				wrapped := fmt.Errorf("some context: %w", tt.err)
				assert.ErrorIs(t, wrapped, tt.err)
			})
		}
	})

	t.Run("no cross-matching", func(t *testing.T) {
		t.Parallel()

		cases := []struct {
			name   string
			target error
			other  error
		}{
			{"Pipeline vs Stage", pipeline.ErrSkipPipeline, pipeline.ErrSkipStage},
		}

		for _, tt := range cases {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				assert.NotErrorIs(t, tt.target, tt.other)
			})
		}
	})
}
