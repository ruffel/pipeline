package pipeline_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/ruffel/pipeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// journalOf numbers events the way a journal would.
func journalOf(events ...pipeline.Event) []pipeline.Event {
	for i := range events {
		events[i].V = pipeline.SchemaVersion
		events[i].Seq = int64(i + 1)
	}

	return events
}

func replay(t *testing.T, events ...pipeline.Event) *pipeline.State {
	t.Helper()

	s, err := pipeline.Replay(journalOf(events...))
	require.NoError(t, err)

	return s
}

func started(run, path string) pipeline.Event {
	return pipeline.Event{Kind: pipeline.EventStepStarted, Run: run, Path: path}
}

// running records that Run was called, in attempt r1.
func running(path string) pipeline.Event {
	return pipeline.Event{Kind: pipeline.EventStepRunning, Run: "r1", Path: path}
}

func finished(run, path string, o pipeline.Outcome, r pipeline.Reason) pipeline.Event {
	return pipeline.Event{Kind: pipeline.EventStepFinished, Run: run, Path: path, Outcome: o, Reason: r}
}

func runStarted(run string, attempt int) pipeline.Event {
	return pipeline.Event{Kind: pipeline.EventRunStarted, Run: run, Attempt: attempt}
}

func runFinished(run string, o pipeline.Outcome) pipeline.Event {
	return pipeline.Event{Kind: pipeline.EventRunFinished, Run: run, Outcome: o}
}

func TestReplay_EmptyJournal(t *testing.T) {
	t.Parallel()

	s := replay(t)
	assert.Empty(t, s.Attempts)
	assert.False(t, s.Unfinished())
	assert.Equal(t, pipeline.ElementState{Status: pipeline.StatusNotReached}, s.Element("p/a"))
}

func TestReplay_StepLifecycle(t *testing.T) {
	t.Parallel()

	s := replay(t,
		runStarted("r1", 1),
		started("r1", "p/checked-done"),
		finished("r1", "p/checked-done", pipeline.OutcomeSkipped, pipeline.ReasonDone),
		started("r1", "p/ran"),
		running("p/ran"),
		finished("r1", "p/ran", pipeline.OutcomePassed, ""),
		started("r1", "p/crashed"),
		running("p/crashed"),
	)

	assert.Equal(t, pipeline.ElementState{
		Status: pipeline.StatusSkipped, Reason: pipeline.ReasonDone, Run: "r1", Done: true,
	}, s.Element("p/checked-done"), "Check said done, so Run was never called")
	assert.Equal(t, pipeline.ElementState{
		Status: pipeline.StatusPassed, Run: "r1", Done: true, RunCalled: true,
	}, s.Element("p/ran"))
	assert.Equal(t, pipeline.ElementState{
		Status: pipeline.StatusStarted, Run: "r1", RunCalled: true,
	}, s.Element("p/crashed"), "interrupted part-way through Run")
	assert.Equal(t, pipeline.StatusNotReached, s.Element("p/never").Status)
	assert.True(t, s.Unfinished(), "the attempt never finished")
}

func TestReplay_LaterAttemptsUpdateEarlierOutcomes(t *testing.T) {
	t.Parallel()

	failure := &pipeline.ErrorInfo{Kind: pipeline.ErrorPlain, Message: "boom"}

	s := replay(t,
		runStarted("r1", 1),
		started("r1", "p/a"), running("p/a"), finished("r1", "p/a", pipeline.OutcomePassed, ""),
		started("r1", "p/b"), running("p/b"),
		pipeline.Event{Kind: pipeline.EventStepFinished, Run: "r1", Path: "p/b", Outcome: pipeline.OutcomeFailed, Error: failure},
		runFinished("r1", pipeline.OutcomeFailed),
		runStarted("r2", 2),
		started("r2", "p/a"), finished("r2", "p/a", pipeline.OutcomeSkipped, pipeline.ReasonEarlierAttempt),
		started("r2", "p/b"),
	)

	a := s.Element("p/a")
	assert.Equal(t, pipeline.StatusSkipped, a.Status)
	assert.True(t, a.Done, "skipped as already done keeps it done")
	assert.True(t, a.RunCalled, "Run was called in an earlier attempt")
	assert.Equal(t, "r2", a.Run)

	b := s.Element("p/b")
	assert.Equal(t, pipeline.StatusStarted, b.Status)
	assert.Nil(t, b.Error, "a new start clears the old error")
	assert.True(t, b.RunCalled)

	require.Len(t, s.Attempts, 2)
	assert.Equal(t, pipeline.OutcomeFailed, s.Attempts[0].Outcome)
	assert.Empty(t, s.Attempts[1].Outcome)
}

func TestReplay_FailureClearsDone(t *testing.T) {
	t.Parallel()

	s := replay(t,
		runStarted("r1", 1),
		started("r1", "p/a"), finished("r1", "p/a", pipeline.OutcomePassed, ""),
		runStarted("r2", 2),
		started("r2", "p/a"), finished("r2", "p/a", pipeline.OutcomeFailed, ""),
	)

	assert.False(t, s.Element("p/a").Done)
}

func TestReplay_SkippedBecauseTheRunEndedKeepsDoneAsItWas(t *testing.T) {
	t.Parallel()

	s := replay(t,
		runStarted("r1", 1),
		finished("r1", "p/a", pipeline.OutcomeSkipped, pipeline.ReasonNotReached),
	)

	el := s.Element("p/a")
	assert.Equal(t, pipeline.StatusSkipped, el.Status)
	assert.False(t, el.Done)
}

func TestReplay_UnfinishedFollowsTheLatestAttempt(t *testing.T) {
	t.Parallel()

	for outcome, want := range map[pipeline.Outcome]bool{
		pipeline.OutcomePassed:    false,
		pipeline.OutcomeFailed:    true,
		pipeline.OutcomeStopped:   true,
		pipeline.OutcomeCancelled: true,
	} {
		s := replay(t, runStarted("r1", 1), runFinished("r1", outcome))
		assert.Equal(t, want, s.Unfinished(), outcome)
	}
}

func TestReplay_AttemptsAndPlan(t *testing.T) {
	t.Parallel()

	t0 := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	plan := &pipeline.NodeSpec{Name: "p", Path: "p", Kind: pipeline.KindGroup}

	s := replay(t,
		pipeline.Event{
			Kind: pipeline.EventRunStarted, Run: "r1", Attempt: 1, TS: t0, Plan: plan,
			Fingerprint: "sha256:abc", Labels: pipeline.L{"operation": "u-42"},
		},
		pipeline.Event{Kind: pipeline.EventRunFinished, Run: "r1", TS: t0.Add(time.Minute), Outcome: pipeline.OutcomePassed},
	)

	assert.Equal(t, plan, s.Plan)
	assert.Equal(t, "sha256:abc", s.Fingerprint)
	assert.Equal(t, pipeline.L{"operation": "u-42"}, s.Labels)
	assert.Equal(t, []pipeline.AttemptState{{
		Run: "r1", Attempt: 1, Started: t0, Finished: t0.Add(time.Minute), Outcome: pipeline.OutcomePassed,
	}}, s.Attempts)
	assert.Equal(t, int64(2), s.LastSeq)
}

func TestReplay_ValuesAndItemsKeepTheLatest(t *testing.T) {
	t.Parallel()

	s := replay(t,
		runStarted("r1", 1),
		pipeline.Event{Kind: pipeline.EventValue, Run: "r1", Path: "p/backup", Key: "dir", Value: json.RawMessage(`"/old"`)},
		pipeline.Event{Kind: pipeline.EventItem, Run: "r1", Path: "p/stage", Item: "node7", Status: pipeline.ItemFailed, Message: "timeout"},
		pipeline.Event{Kind: pipeline.EventItem, Run: "r1", Path: "p/stage", Item: "node8", Status: pipeline.ItemPassed},
		runStarted("r2", 2),
		pipeline.Event{Kind: pipeline.EventValue, Run: "r2", Path: "p/backup", Key: "dir", Value: json.RawMessage(`"/new"`)},
		pipeline.Event{Kind: pipeline.EventItem, Run: "r2", Path: "p/stage", Item: "node7", Status: pipeline.ItemPassed},
	)

	v, ok := s.Value("p/backup", "dir")
	require.True(t, ok)
	assert.JSONEq(t, `"/new"`, string(v))

	_, ok = s.Value("p/backup", "missing")
	assert.False(t, ok)

	items := s.Items("p/stage")
	assert.Equal(t, map[string]pipeline.ItemState{
		"node7": {Status: pipeline.ItemPassed, Run: "r2"},
		"node8": {Status: pipeline.ItemPassed, Run: "r1"},
	}, items)

	delete(items, "node7")
	assert.Len(t, s.Items("p/stage"), 2, "Items returns a copy")
}

func TestReplay_BestEffortEventsChangeNoState(t *testing.T) {
	t.Parallel()

	s := replay(t,
		runStarted("r1", 1),
		pipeline.Event{Kind: pipeline.EventOutput, Run: "r1", Path: "p/a", Line: "x"},
		pipeline.Event{Kind: pipeline.EventProgress, Run: "r1", Path: "p/a", Key: "k", Current: 1, Total: 2},
		pipeline.Event{Kind: pipeline.EventMessage, Run: "r1", Path: "p/a", Level: "warn", Message: "w"},
		pipeline.Event{Kind: pipeline.EventCustom, Run: "r1", Path: "p/a", Type: "x.y", Data: json.RawMessage(`{}`)},
		pipeline.Event{Kind: pipeline.EventStepRetry, Run: "r1", Path: "p/a", Try: 1},
	)

	assert.Equal(t, pipeline.StatusNotReached, s.Element("p/a").Status)
}

func TestReplay_RefusesBadJournals(t *testing.T) {
	t.Parallel()

	_, err := pipeline.Replay([]pipeline.Event{{V: 2, Seq: 1, Kind: pipeline.EventRunStarted}})
	require.ErrorContains(t, err, "schema version 2 is newer")

	_, err = pipeline.Replay([]pipeline.Event{
		{V: 1, Seq: 2, Kind: pipeline.EventRunStarted},
		{V: 1, Seq: 2, Kind: pipeline.EventRunFinished},
	})
	require.ErrorContains(t, err, "sequence numbers must increase")
}

func TestEventKind_Durable(t *testing.T) {
	t.Parallel()

	for _, k := range []pipeline.EventKind{
		pipeline.EventRunStarted, pipeline.EventRunFinished, pipeline.EventGroupStarted, pipeline.EventGroupFinished,
		pipeline.EventStepStarted, pipeline.EventStepRunning, pipeline.EventStepFinished, pipeline.EventValue, pipeline.EventItem,
		"some.future.kind",
	} {
		assert.True(t, k.Durable(), k)
	}

	for _, k := range []pipeline.EventKind{
		pipeline.EventOutput, pipeline.EventProgress, pipeline.EventMessage, pipeline.EventCustom, pipeline.EventStepRetry,
	} {
		assert.False(t, k.Durable(), k)
	}
}
