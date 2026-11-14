package pipeline_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ruffel/pipeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJSONObserver_WritesTheJournalFormat(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	obs := pipeline.NewJSONObserver(&buf)

	out := runPlan(t, t.Context(), mustPlan(t,
		pipeline.Group("g",
			pipeline.Effect("a", pipeline.StepFunc(func(_ context.Context, s *pipeline.StepContext) error {
				s.Info("hello")

				return s.Record("k", map[string]int{"n": 1})
			})),
			pipeline.Effect("b", fail("boom")),
		),
	), pipeline.WithObservers(obs))

	require.Error(t, out.err)
	require.NoError(t, obs.Err())

	var written []pipeline.Event

	scanner := bufio.NewScanner(&buf)
	for scanner.Scan() {
		var e pipeline.Event

		require.NoError(t, json.Unmarshal(scanner.Bytes(), &e))

		written = append(written, e)
	}

	assert.Equal(t, out.events, written, "the observer's file is a copy of the journal")

	state, err := pipeline.Replay(written)
	require.NoError(t, err)
	assert.Equal(t, out.state.Element("p/g/b"), state.Element("p/g/b"))
}

type failingWriter struct{ calls int }

func (f *failingWriter) Write([]byte) (int, error) {
	f.calls++

	return 0, errors.New("disk full")
}

func TestJSONObserver_StopsAtTheFirstWriteError(t *testing.T) {
	t.Parallel()

	w := &failingWriter{}
	obs := pipeline.NewJSONObserver(w)

	out := runPlan(t, t.Context(), mustPlan(t, pipeline.Effect("a", ok())), pipeline.WithObservers(obs))

	require.NoError(t, out.err, "an observer's write error does not fail the run")
	require.ErrorContains(t, obs.Err(), "disk full")
	assert.Equal(t, 1, w.calls)
}

func TestPlainObserver_Rendering(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	obs := pipeline.NewPlainObserver(&buf)
	plan := &pipeline.NodeSpec{Name: "upgrade", Path: "upgrade", Kind: pipeline.KindGroup, Children: []pipeline.NodeSpec{
		{Name: "prechecks", Path: "upgrade/prechecks", Kind: pipeline.KindGroup, Description: "Read-only checks"},
		{Name: "apply", Path: "upgrade/apply", Kind: pipeline.KindEffect, Description: "Install the packages"},
	}}

	for _, e := range []pipeline.Event{
		{Kind: pipeline.EventRunStarted, Path: "upgrade", Plan: plan},
		{Kind: pipeline.EventGroupStarted, Path: "upgrade/prechecks"},
		{Kind: pipeline.EventStepStarted, Path: "upgrade/prechecks/ring"},
		{Kind: pipeline.EventStepRunning, Path: "upgrade/prechecks/ring"},
		{Kind: pipeline.EventProgress, Path: "upgrade/prechecks/ring", Message: "probing", Current: 2, Total: 3},
		{Kind: pipeline.EventStepFinished, Path: "upgrade/prechecks/ring", Outcome: pipeline.OutcomePassed, DurationMs: 1800},
		{Kind: pipeline.EventGroupFinished, Path: "upgrade/prechecks", Outcome: pipeline.OutcomePassed, DurationMs: 1900},
		{Kind: pipeline.EventStepFinished, Path: "upgrade/backup", Outcome: pipeline.OutcomeSkipped, Reason: pipeline.ReasonDone},
		{Kind: pipeline.EventStepStarted, Path: "upgrade/apply"},
		{Kind: pipeline.EventValue, Path: "upgrade/apply", Key: "previous", Value: json.RawMessage(`"8.1"`)},
		{Kind: pipeline.EventOutput, Path: "upgrade/apply", Stream: "stdout", Line: "E: dpkg was interrupted"},
		{Kind: pipeline.EventItem, Path: "upgrade/apply", Item: "node7", Status: pipeline.ItemFailed, Message: "timeout"},
		{Kind: pipeline.EventMessage, Path: "upgrade/apply", Level: pipeline.LevelWarn, Message: "careful"},
		{Kind: pipeline.EventCustom, Path: "upgrade/apply", Type: "deploy.ready", Data: json.RawMessage(`{"port":443}`)},
		{Kind: pipeline.EventStepRetry, Path: "upgrade/apply", Try: 1, Error: &pipeline.ErrorInfo{Message: "flaky"}},
		{
			Kind: pipeline.EventStepFinished, Path: "upgrade/apply", Outcome: pipeline.OutcomeFailed,
			Error: &pipeline.ErrorInfo{Message: "exit status 1"}, DurationMs: 400,
		},
		{
			Kind: pipeline.EventRunFinished, Path: "upgrade", Outcome: pipeline.OutcomeFailed,
			Error: &pipeline.ErrorInfo{Message: "upgrade/apply: exit status 1"}, DurationMs: 2300,
		},
	} {
		obs.OnEvent(t.Context(), e)
	}

	assert.Equal(t, `▶ upgrade
  ▶ prechecks — Read-only checks
      │ prechecks/ring: probing (2/3)
    ✓ ring (1.8s)
  ✓ prechecks (1.9s)
  ⊘ backup — done
  · apply — Install the packages
    │ apply: E: dpkg was interrupted
    │ apply: node7 failed — timeout
    │ apply: [WARN] careful
    │ apply: [deploy.ready] {"port":443}
    │ apply: try 1 failed — flaky, retrying
  ✗ apply — exit status 1 (400ms)
✗ upgrade failed — upgrade/apply: exit status 1 (2.3s)
`, buf.String())
}

func TestPlainObserver_ARealRun(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	out := runPlan(t, t.Context(), mustPlan(t,
		pipeline.Group("g", pipeline.Effect("a", pipeline.StepFunc(func(_ context.Context, s *pipeline.StepContext) error {
			_, _ = s.Stdout().Write([]byte("hello\n"))

			return nil
		}))),
	), pipeline.WithObservers(pipeline.NewPlainObserver(&buf)))

	require.NoError(t, out.err)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	assert.Equal(t, "▶ p", lines[0])
	assert.Equal(t, "  ▶ g", lines[1])
	assert.Equal(t, "      │ g/a: hello", lines[2])
	assert.True(t, strings.HasPrefix(lines[3], "    ✓ a"))
	assert.True(t, strings.HasPrefix(lines[len(lines)-1], "✓ p passed"))
}

// Steps in a parallel group interleave their output; each line names its
// step, even when steps in different groups share a name.
func TestPlainObserver_AttributesInterleavedOutput(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	obs := pipeline.NewPlainObserver(&buf)

	for _, e := range []pipeline.Event{
		{Kind: pipeline.EventRunStarted, Path: "upgrade"},
		{Kind: pipeline.EventOutput, Path: "upgrade/nodes/node1/stop", Line: "stopping"},
		{Kind: pipeline.EventOutput, Path: "upgrade/nodes/node2/stop", Line: "stopping"},
		{Kind: pipeline.EventMessage, Path: "upgrade/nodes/node1/stop", Level: pipeline.LevelInfo, Message: "done"},
	} {
		obs.OnEvent(t.Context(), e)
	}

	assert.Equal(t, `▶ upgrade
        │ nodes/node1/stop: stopping
        │ nodes/node2/stop: stopping
        │ nodes/node1/stop: [INFO] done
`, buf.String())
}

func TestPlainObserver_AStoppedRun(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	obs := pipeline.NewPlainObserver(&buf)

	for _, e := range []pipeline.Event{
		{Kind: pipeline.EventRunStarted, Path: "upgrade"},
		{Kind: pipeline.EventGroupStarted, Path: "upgrade/DC1"},
		{Kind: pipeline.EventStopRequested, Path: "upgrade", Mode: pipeline.StopGraceful, By: "stopper", Who: "alice"},
		{Kind: pipeline.EventGroupFinished, Path: "upgrade/DC1", Outcome: pipeline.OutcomeStopped},
		{Kind: pipeline.EventRunFinished, Path: "upgrade", Outcome: pipeline.OutcomeStopped},
	} {
		obs.OnEvent(t.Context(), e)
	}

	assert.Equal(t, `▶ upgrade
  ▶ DC1
■ stop requested by alice; running steps will finish
  ■ DC1
■ upgrade stopped
`, buf.String())
}

func TestPlainObserver_ACancelRequest(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	pipeline.NewPlainObserver(&buf).OnEvent(t.Context(), pipeline.Event{
		Kind: pipeline.EventStopRequested, Path: "upgrade", Mode: pipeline.StopCancel, By: "control", Who: "bob",
	})

	assert.Equal(t, "■ cancel requested by bob; running steps are cancelled\n", buf.String())
}
