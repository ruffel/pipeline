// Package journalhook lets this module's own tests, and package pipelinetest,
// make a journal's writes fail on purpose: a simulated crash, or a disk that
// fills up. It is internal so that it is not part of pipeline's API.
package journalhook

// Set installs hook on journal, which must be a *pipeline.Journal: hook, a
// func(pipeline.Event) error, is called with each event, already numbered,
// before the journal stores it. If hook returns an error, the write fails
// with it exactly as a storage failure would, and the run stops.
//
// Call it before the journal is used. Package pipeline sets it, and the
// types are left open here because this package cannot import pipeline.
var Set func(journal, hook any) //nolint:gochecknoglobals // Filled in by package pipeline, which this one cannot import.
