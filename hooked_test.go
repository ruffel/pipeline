package pipeline_test

import (
	"github.com/ruffel/pipeline"
	"github.com/ruffel/pipeline/internal/journalhook"
)

// hooked installs hook on j, so a test can make the journal's writes fail:
// see the internal journalhook package.
func hooked(j *pipeline.Journal, hook func(pipeline.Event) error) *pipeline.Journal {
	journalhook.Set(j, hook)

	return j
}
