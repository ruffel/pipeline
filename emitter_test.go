package pipeline_test

import (
	"context"
	"testing"

	"github.com/ruffel/pipeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingObserver captures all events for test assertions.
type recordingObserver struct {
	events []pipeline.Event
}

func (r *recordingObserver) OnEvent(_ context.Context, event pipeline.Event) {
	r.events = append(r.events, event)
}

// -----------------------------------------------------------------------------
// Context round-trip
// -----------------------------------------------------------------------------

func TestEmitterContextRoundTrip(t *testing.T) {
	t.Parallel()

	obs := &recordingObserver{}
	loc := pipeline.Location{Pipeline: "p", Stage: "s", Step: "step"}
	em := pipeline.NewEmitter([]pipeline.Observer{obs}, loc)

	ctx := pipeline.WithEmitter(t.Context(), em)
	got := pipeline.EmitterFrom(ctx)

	require.NotNil(t, got)
	assert.Same(t, em, got)
}

func TestEmitterFromReturnsNilWhenMissing(t *testing.T) {
	t.Parallel()

	got := pipeline.EmitterFrom(t.Context())

	assert.Nil(t, got)
}

// -----------------------------------------------------------------------------
// No-op safety — calling EmitX on a bare context must not panic.
// -----------------------------------------------------------------------------

func TestEmitXNoOpWithoutEmitter(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	// None of these should panic.
	assert.NotPanics(t, func() {
		pipeline.EmitInfo(ctx, "msg")
		pipeline.EmitInfof(ctx, "msg %d", 1)
		pipeline.EmitWarn(ctx, "msg")
		pipeline.EmitWarnf(ctx, "msg %d", 1)
		pipeline.EmitDebug(ctx, "msg")
		pipeline.EmitDebugf(ctx, "msg %d", 1)
		pipeline.EmitProgress(ctx, "", "uploading", 1, 10)
		pipeline.EmitOutput(ctx, pipeline.Stdout, "line")
		pipeline.EmitCustom(ctx, "x.y", nil)
	})
}

// -----------------------------------------------------------------------------
// Each EmitX helper emits the correct event type
// -----------------------------------------------------------------------------

func TestEmitInfo(t *testing.T) {
	t.Parallel()

	obs, ctx := emitterCtx(t)
	pipeline.EmitInfo(ctx, "hello")

	require.Len(t, obs.events, 1)
	e, ok := obs.events[0].(pipeline.MessageEvent)
	require.True(t, ok)
	assert.Equal(t, pipeline.LevelInfo, e.Level)
	assert.Equal(t, "hello", e.Message)
}

func TestEmitInfof(t *testing.T) {
	t.Parallel()

	obs, ctx := emitterCtx(t)
	pipeline.EmitInfof(ctx, "count: %d", 42)

	require.Len(t, obs.events, 1)
	e, ok := obs.events[0].(pipeline.MessageEvent)
	require.True(t, ok)
	assert.Equal(t, "count: 42", e.Message)
}

func TestEmitWarn(t *testing.T) {
	t.Parallel()

	obs, ctx := emitterCtx(t)
	pipeline.EmitWarn(ctx, "careful")

	require.Len(t, obs.events, 1)
	e, ok := obs.events[0].(pipeline.MessageEvent)
	require.True(t, ok)
	assert.Equal(t, pipeline.LevelWarn, e.Level)
}

func TestEmitDebug(t *testing.T) {
	t.Parallel()

	obs, ctx := emitterCtx(t)
	pipeline.EmitDebug(ctx, "trace")

	require.Len(t, obs.events, 1)
	e, ok := obs.events[0].(pipeline.MessageEvent)
	require.True(t, ok)
	assert.Equal(t, pipeline.LevelDebug, e.Level)
}

func TestEmitProgress(t *testing.T) {
	t.Parallel()

	obs, ctx := emitterCtx(t)
	pipeline.EmitProgress(ctx, "upload", "sending", 3, 10)

	require.Len(t, obs.events, 1)
	e, ok := obs.events[0].(pipeline.ProgressEvent)
	require.True(t, ok)
	assert.Equal(t, "upload", e.Key)
	assert.Equal(t, "sending", e.Message)
	assert.Equal(t, 3, e.Current)
	assert.Equal(t, 10, e.Total)
}

func TestEmitOutput(t *testing.T) {
	t.Parallel()

	obs, ctx := emitterCtx(t)
	pipeline.EmitOutput(ctx, pipeline.Stderr, "error: boom")

	require.Len(t, obs.events, 1)
	e, ok := obs.events[0].(pipeline.OutputEvent)
	require.True(t, ok)
	assert.Equal(t, pipeline.Stderr, e.Stream)
	assert.Equal(t, "error: boom", e.Line)
}

func TestEmitCustom(t *testing.T) {
	t.Parallel()

	obs, ctx := emitterCtx(t)
	data := map[string]string{"url": "https://example.com"}
	pipeline.EmitCustom(ctx, "deploy.url-ready", data)

	require.Len(t, obs.events, 1)
	e, ok := obs.events[0].(pipeline.CustomEvent)
	require.True(t, ok)
	assert.Equal(t, "deploy.url-ready", e.Type)
	assert.Equal(t, data, e.Data)
}

func TestEmitCarriesLocation(t *testing.T) {
	t.Parallel()

	loc := pipeline.Location{Pipeline: "p", Stage: "s", Step: "step"}
	obs := &recordingObserver{}
	em := pipeline.NewEmitter([]pipeline.Observer{obs}, loc)
	ctx := pipeline.WithEmitter(t.Context(), em)

	pipeline.EmitInfo(ctx, "test")

	require.Len(t, obs.events, 1)
	e, ok := obs.events[0].(pipeline.MessageEvent)
	require.True(t, ok)
	assert.Equal(t, loc, e.Location)
}

// emitterCtx returns a recording observer and a context wired with an emitter.
func emitterCtx(t *testing.T) (*recordingObserver, context.Context) {
	t.Helper()

	obs := &recordingObserver{}
	loc := pipeline.Location{Pipeline: "test"}
	em := pipeline.NewEmitter([]pipeline.Observer{obs}, loc)

	return obs, pipeline.WithEmitter(t.Context(), em)
}
