package pipeline_test

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ruffel/pipeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gate is an observer that blocks on its first event until released, so
// tests can fill the async queue while the wrapped observer is busy.
type gate struct {
	recorder

	busy    chan struct{} // closed once the first event is being handled
	release chan struct{}
	first   bool
}

func newGate() *gate {
	return &gate{busy: make(chan struct{}), release: make(chan struct{}), first: true}
}

func (g *gate) OnEvent(ctx context.Context, e pipeline.Event) {
	if g.first {
		g.first = false

		close(g.busy)
		<-g.release
	}

	g.recorder.OnEvent(ctx, e)
}

func event(kind pipeline.EventKind, path, text string) pipeline.Event {
	e := pipeline.Event{Kind: kind, Path: path}

	switch kind { //nolint:exhaustive // Only the kinds these tests build.
	case pipeline.EventOutput:
		e.Line = text
	case pipeline.EventProgress:
		e.Key, e.Message = "copy", text
	default:
		e.Message = text
	}

	return e
}

// summary renders delivered events as "kind:text".
func summary(events []pipeline.Event) []string {
	out := make([]string, 0, len(events))

	for _, e := range events {
		text := e.Message
		if e.Kind == pipeline.EventOutput {
			text = e.Line
		}

		out = append(out, string(e.Kind)+":"+text)
	}

	return out
}

func closeOK(t *testing.T, a *pipeline.AsyncObserver) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	require.NoError(t, a.Close(ctx))
}

func TestAsyncObserver_DeliversEverythingInOrder(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	a := pipeline.NewAsyncObserver(rec, 0)

	want := make([]string, 0, 50)

	for i := range 50 {
		e := event(pipeline.EventOutput, "p/a", strconv.Itoa(i))
		a.OnEvent(t.Context(), e)

		want = append(want, "output:"+strconv.Itoa(i))
	}

	closeOK(t, a)
	assert.Equal(t, want, summary(rec.all()))
	require.NoError(t, a.Err())
}

func TestAsyncObserver_NeverDropsLifecycleEvents(t *testing.T) {
	t.Parallel()

	g := newGate()
	a := pipeline.NewAsyncObserver(g, 2)

	a.OnEvent(t.Context(), event(pipeline.EventRunStarted, "p", "first"))
	<-g.busy

	for i := range 1000 {
		a.OnEvent(t.Context(), event(pipeline.EventStepFinished, fmt.Sprintf("p/s%d", i), ""))
	}

	close(g.release)
	closeOK(t, a)
	assert.Len(t, g.all(), 1001)
}

func TestAsyncObserver_KeepsOnlyTheLatestProgressPerStepAndKey(t *testing.T) {
	t.Parallel()

	g := newGate()
	a := pipeline.NewAsyncObserver(g, 0)

	a.OnEvent(t.Context(), event(pipeline.EventRunStarted, "p", ""))
	<-g.busy

	for i := range 100 {
		a.OnEvent(t.Context(), event(pipeline.EventProgress, "p/a", fmt.Sprintf("a %d%%", i)))
		a.OnEvent(t.Context(), event(pipeline.EventProgress, "p/b", fmt.Sprintf("b %d%%", i)))
	}

	a.OnEvent(t.Context(), event(pipeline.EventStepFinished, "p/a", ""))

	close(g.release)
	closeOK(t, a)
	assert.Equal(t, []string{
		"run.started:", "progress:a 99%", "progress:b 99%", "step.finished:",
	}, summary(g.all()))
}

func TestAsyncObserver_DropsTheOldestChatterAndSaysSo(t *testing.T) {
	t.Parallel()

	g := newGate()
	a := pipeline.NewAsyncObserver(g, 3)

	a.OnEvent(t.Context(), event(pipeline.EventRunStarted, "p", ""))
	<-g.busy

	for i := range 10 {
		a.OnEvent(t.Context(), event(pipeline.EventOutput, "p/a", strconv.Itoa(i)))
	}

	a.OnEvent(t.Context(), event(pipeline.EventStepFinished, "p/a", ""))

	close(g.release)
	closeOK(t, a)

	got := summary(g.all())
	assert.Equal(t, []string{
		"run.started:",
		"message:observer fell behind: 7 output and message events were dropped",
		"output:7", "output:8", "output:9",
		"step.finished:",
	}, got)

	notice := g.all()[1]
	assert.Equal(t, pipeline.LevelWarn, notice.Level)
	assert.Zero(t, notice.Seq, "the notice is not a journal event")
}

func TestAsyncObserver_APanicStopsDeliveryWithoutCrashing(t *testing.T) {
	t.Parallel()

	calls := 0
	bad := pipeline.ObserverFunc(func(_ context.Context, e pipeline.Event) {
		calls++

		if e.Kind == pipeline.EventStepStarted {
			panic("renderer bug")
		}
	})

	a := pipeline.NewAsyncObserver(bad, 0)
	a.OnEvent(t.Context(), event(pipeline.EventRunStarted, "p", ""))
	a.OnEvent(t.Context(), event(pipeline.EventStepStarted, "p/a", ""))
	a.OnEvent(t.Context(), event(pipeline.EventStepFinished, "p/a", ""))

	closeOK(t, a)
	require.ErrorContains(t, a.Err(), "renderer bug")
	assert.Equal(t, 2, calls, "nothing is delivered after the panic")
}

func TestAsyncObserver_CloseGivesUpOnAStuckObserver(t *testing.T) {
	t.Parallel()

	g := newGate()
	a := pipeline.NewAsyncObserver(g, 0)

	a.OnEvent(t.Context(), event(pipeline.EventRunStarted, "p", ""))
	<-g.busy

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()

	require.ErrorIs(t, a.Close(ctx), context.DeadlineExceeded)

	a.OnEvent(t.Context(), event(pipeline.EventRunFinished, "p", ""))
	close(g.release)

	closeOK(t, a)
	assert.Len(t, g.all(), 1, "events after Close are ignored")
}

func TestAsyncObserver_KeepsASlowObserverFromStallingTheRun(t *testing.T) {
	t.Parallel()

	g := newGate()
	a := pipeline.NewAsyncObserver(g, 0)

	out := runPlan(t, t.Context(), mustPlan(t, pipeline.Effect("a", pipeline.StepFunc(
		func(_ context.Context, s *pipeline.StepContext) error {
			for i := range 100 {
				s.Info(strconv.Itoa(i))
			}

			return nil
		}))), pipeline.WithObservers(a))

	// The run finished while the wrapped observer was still on its first
	// event.
	require.NoError(t, out.err)

	close(g.release)
	closeOK(t, a)
	assert.Equal(t, summary(out.events), summary(g.all()), "with room to spare, nothing is dropped")
	assert.True(t, strings.HasPrefix(summary(g.all())[0], "run.started"))
}

// blockingStringer's String never returns.
type blockingStringer struct{ block chan struct{} }

func (b blockingStringer) String() string {
	<-b.block

	return "never"
}

// A panic value whose String blocks must not stall the run or Close.
func TestAsyncObserver_ABlockingPanicFormatterDoesNotStallEnqueueOrClose(t *testing.T) {
	t.Parallel()

	block := make(chan struct{})
	defer close(block)

	a := pipeline.NewAsyncObserver(pipeline.ObserverFunc(func(context.Context, pipeline.Event) {
		panic(blockingStringer{block: block})
	}), 0)

	finished := make(chan error, 1)

	go func() {
		a.OnEvent(t.Context(), event(pipeline.EventRunStarted, "p", ""))
		time.Sleep(10 * time.Millisecond) // let delivery reach the formatter
		a.OnEvent(t.Context(), event(pipeline.EventRunFinished, "p", ""))

		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()

		finished <- a.Close(ctx)
	}()

	select {
	case err := <-finished:
		require.ErrorIs(t, err, context.DeadlineExceeded, "Close gives up; the formatter is still stuck")
	case <-time.After(5 * time.Second):
		t.Fatal("OnEvent or Close blocked behind the panic formatter")
	}
}
