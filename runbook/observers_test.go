package runbook_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ruffel/pipeline/runbook"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJSONObserver_WritesTheJournalFormat(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	obs := runbook.NewJSONObserver(&buf)

	out := runPlan(t, t.Context(), mustPlan(t,
		runbook.Group("g",
			runbook.Effect("a", runbook.StepFunc(func(_ context.Context, s *runbook.StepContext) error {
				s.Info("hello")

				return s.Record("k", map[string]int{"n": 1})
			})),
			runbook.Effect("b", fail("boom")),
		),
	), runbook.WithObservers(obs))

	require.Error(t, out.err)
	require.NoError(t, obs.Err())

	var written []runbook.Event

	scanner := bufio.NewScanner(&buf)
	for scanner.Scan() {
		var e runbook.Event

		require.NoError(t, json.Unmarshal(scanner.Bytes(), &e))

		written = append(written, e)
	}

	assert.Equal(t, out.events, written, "the observer's file is a copy of the journal")

	state, err := runbook.Replay(written)
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
	obs := runbook.NewJSONObserver(w)

	out := runPlan(t, t.Context(), mustPlan(t, runbook.Effect("a", ok())), runbook.WithObservers(obs))

	require.NoError(t, out.err, "an observer's write error does not fail the run")
	require.ErrorContains(t, obs.Err(), "disk full")
	assert.Equal(t, 1, w.calls)
}

func TestPlainObserver_Rendering(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	obs := runbook.NewPlainObserver(&buf)
	plan := &runbook.NodeSpec{Name: "upgrade", Path: "upgrade", Kind: runbook.KindGroup, Children: []runbook.NodeSpec{
		{Name: "prechecks", Path: "upgrade/prechecks", Kind: runbook.KindGroup, Description: "Read-only checks"},
		{Name: "apply", Path: "upgrade/apply", Kind: runbook.KindEffect, Description: "Install the packages"},
	}}

	for _, e := range []runbook.Event{
		{Kind: runbook.EventRunStarted, Path: "upgrade", Plan: plan},
		{Kind: runbook.EventGroupStarted, Path: "upgrade/prechecks"},
		{Kind: runbook.EventStepStarted, Path: "upgrade/prechecks/ring"},
		{Kind: runbook.EventStepRunning, Path: "upgrade/prechecks/ring"},
		{Kind: runbook.EventProgress, Path: "upgrade/prechecks/ring", Message: "probing", Current: 2, Total: 3},
		{Kind: runbook.EventStepFinished, Path: "upgrade/prechecks/ring", Outcome: runbook.OutcomePassed, DurationMs: 1800},
		{Kind: runbook.EventGroupFinished, Path: "upgrade/prechecks", Outcome: runbook.OutcomePassed, DurationMs: 1900},
		{Kind: runbook.EventStepFinished, Path: "upgrade/backup", Outcome: runbook.OutcomeSkipped, Reason: runbook.ReasonDone},
		{Kind: runbook.EventStepStarted, Path: "upgrade/apply"},
		{Kind: runbook.EventValue, Path: "upgrade/apply", Key: "previous", Value: json.RawMessage(`"8.1"`)},
		{Kind: runbook.EventOutput, Path: "upgrade/apply", Stream: "stdout", Line: "E: dpkg was interrupted"},
		{Kind: runbook.EventItem, Path: "upgrade/apply", Item: "node7", Status: runbook.ItemFailed, Message: "timeout"},
		{Kind: runbook.EventMessage, Path: "upgrade/apply", Level: runbook.LevelWarn, Message: "careful"},
		{Kind: runbook.EventCustom, Path: "upgrade/apply", Type: "deploy.ready", Data: json.RawMessage(`{"port":443}`)},
		{Kind: runbook.EventStepRetry, Path: "upgrade/apply", Try: 1, Error: &runbook.ErrorInfo{Message: "flaky"}},
		{
			Kind: runbook.EventStepFinished, Path: "upgrade/apply", Outcome: runbook.OutcomeFailed,
			Error: &runbook.ErrorInfo{Message: "exit status 1"}, DurationMs: 400,
		},
		{
			Kind: runbook.EventRunFinished, Path: "upgrade", Outcome: runbook.OutcomeFailed,
			Error: &runbook.ErrorInfo{Message: "upgrade/apply: exit status 1"}, DurationMs: 2300,
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
		runbook.Group("g", runbook.Effect("a", runbook.StepFunc(func(_ context.Context, s *runbook.StepContext) error {
			_, _ = s.Stdout().Write([]byte("hello\n"))

			return nil
		}))),
	), runbook.WithObservers(runbook.NewPlainObserver(&buf)))

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

	obs := runbook.NewPlainObserver(&buf)

	for _, e := range []runbook.Event{
		{Kind: runbook.EventRunStarted, Path: "upgrade"},
		{Kind: runbook.EventOutput, Path: "upgrade/nodes/node1/stop", Line: "stopping"},
		{Kind: runbook.EventOutput, Path: "upgrade/nodes/node2/stop", Line: "stopping"},
		{Kind: runbook.EventMessage, Path: "upgrade/nodes/node1/stop", Level: runbook.LevelInfo, Message: "done"},
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

	obs := runbook.NewPlainObserver(&buf)

	for _, e := range []runbook.Event{
		{Kind: runbook.EventRunStarted, Path: "upgrade"},
		{Kind: runbook.EventGroupStarted, Path: "upgrade/DC1"},
		{Kind: runbook.EventStopRequested, Path: "upgrade", Mode: runbook.StopGraceful, By: "stopper", Who: "alice"},
		{Kind: runbook.EventGroupFinished, Path: "upgrade/DC1", Outcome: runbook.OutcomeStopped},
		{Kind: runbook.EventRunFinished, Path: "upgrade", Outcome: runbook.OutcomeStopped},
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
