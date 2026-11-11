package runbook_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/ruffel/pipeline/runbook"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// journalOf numbers events the way a journal would.
func journalOf(events ...runbook.Event) []runbook.Event {
	for i := range events {
		events[i].V = runbook.SchemaVersion
		events[i].Seq = int64(i + 1)
	}

	return events
}

func replay(t *testing.T, events ...runbook.Event) *runbook.State {
	t.Helper()

	s, err := runbook.Replay(journalOf(events...))
	require.NoError(t, err)

	return s
}

func started(run, path string) runbook.Event {
	return runbook.Event{Kind: runbook.EventStepStarted, Run: run, Path: path}
}

// running records that Run was called, in attempt r1.
func running(path string) runbook.Event {
	return runbook.Event{Kind: runbook.EventStepRunning, Run: "r1", Path: path}
}

func finished(run, path string, o runbook.Outcome, r runbook.Reason) runbook.Event {
	return runbook.Event{Kind: runbook.EventStepFinished, Run: run, Path: path, Outcome: o, Reason: r}
}

func runStarted(run string, attempt int) runbook.Event {
	return runbook.Event{Kind: runbook.EventRunStarted, Run: run, Attempt: attempt}
}

func runFinished(run string, o runbook.Outcome) runbook.Event {
	return runbook.Event{Kind: runbook.EventRunFinished, Run: run, Outcome: o}
}

func TestReplay_EmptyJournal(t *testing.T) {
	t.Parallel()

	s := replay(t)
	assert.Empty(t, s.Attempts)
	assert.False(t, s.Unfinished())
	assert.Equal(t, runbook.ElementState{Status: runbook.StatusNotReached}, s.Element("p/a"))
}

func TestReplay_StepLifecycle(t *testing.T) {
	t.Parallel()

	s := replay(t,
		runStarted("r1", 1),
		started("r1", "p/checked-done"),
		finished("r1", "p/checked-done", runbook.OutcomeSkipped, runbook.ReasonDone),
		started("r1", "p/ran"),
		running("p/ran"),
		finished("r1", "p/ran", runbook.OutcomePassed, ""),
		started("r1", "p/crashed"),
		running("p/crashed"),
	)

	assert.Equal(t, runbook.ElementState{
		Status: runbook.StatusSkipped, Reason: runbook.ReasonDone, Run: "r1", Done: true,
	}, s.Element("p/checked-done"), "Check said done, so Run was never called")
	assert.Equal(t, runbook.ElementState{
		Status: runbook.StatusPassed, Run: "r1", Done: true, RunCalled: true,
	}, s.Element("p/ran"))
	assert.Equal(t, runbook.ElementState{
		Status: runbook.StatusStarted, Run: "r1", RunCalled: true,
	}, s.Element("p/crashed"), "interrupted part-way through Run")
	assert.Equal(t, runbook.StatusNotReached, s.Element("p/never").Status)
	assert.True(t, s.Unfinished(), "the attempt never finished")
}

func TestReplay_LaterAttemptsUpdateEarlierOutcomes(t *testing.T) {
	t.Parallel()

	failure := &runbook.ErrorInfo{Kind: runbook.ErrorPlain, Message: "boom"}

	s := replay(t,
		runStarted("r1", 1),
		started("r1", "p/a"), running("p/a"), finished("r1", "p/a", runbook.OutcomePassed, ""),
		started("r1", "p/b"), running("p/b"),
		runbook.Event{Kind: runbook.EventStepFinished, Run: "r1", Path: "p/b", Outcome: runbook.OutcomeFailed, Error: failure},
		runFinished("r1", runbook.OutcomeFailed),
		runStarted("r2", 2),
		started("r2", "p/a"), finished("r2", "p/a", runbook.OutcomeSkipped, runbook.ReasonEarlierAttempt),
		started("r2", "p/b"),
	)

	a := s.Element("p/a")
	assert.Equal(t, runbook.StatusSkipped, a.Status)
	assert.True(t, a.Done, "skipped as already done keeps it done")
	assert.True(t, a.RunCalled, "Run was called in an earlier attempt")
	assert.Equal(t, "r2", a.Run)

	b := s.Element("p/b")
	assert.Equal(t, runbook.StatusStarted, b.Status)
	assert.Nil(t, b.Error, "a new start clears the old error")
	assert.True(t, b.RunCalled)

	require.Len(t, s.Attempts, 2)
	assert.Equal(t, runbook.OutcomeFailed, s.Attempts[0].Outcome)
	assert.Empty(t, s.Attempts[1].Outcome)
}

func TestReplay_FailureClearsDone(t *testing.T) {
	t.Parallel()

	s := replay(t,
		runStarted("r1", 1),
		started("r1", "p/a"), finished("r1", "p/a", runbook.OutcomePassed, ""),
		runStarted("r2", 2),
		started("r2", "p/a"), finished("r2", "p/a", runbook.OutcomeFailed, ""),
	)

	assert.False(t, s.Element("p/a").Done)
}

func TestReplay_SkippedBecauseTheRunEndedKeepsDoneAsItWas(t *testing.T) {
	t.Parallel()

	s := replay(t,
		runStarted("r1", 1),
		finished("r1", "p/a", runbook.OutcomeSkipped, runbook.ReasonNotReached),
	)

	el := s.Element("p/a")
	assert.Equal(t, runbook.StatusSkipped, el.Status)
	assert.False(t, el.Done)
}

func TestReplay_UnfinishedFollowsTheLatestAttempt(t *testing.T) {
	t.Parallel()

	for outcome, want := range map[runbook.Outcome]bool{
		runbook.OutcomePassed:    false,
		runbook.OutcomeFailed:    true,
		runbook.OutcomeStopped:   true,
		runbook.OutcomeCancelled: true,
	} {
		s := replay(t, runStarted("r1", 1), runFinished("r1", outcome))
		assert.Equal(t, want, s.Unfinished(), outcome)
	}
}

func TestReplay_AttemptsAndPlan(t *testing.T) {
	t.Parallel()

	t0 := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	plan := &runbook.NodeSpec{Name: "p", Path: "p", Kind: runbook.KindGroup}

	s := replay(t,
		runbook.Event{
			Kind: runbook.EventRunStarted, Run: "r1", Attempt: 1, TS: t0, Plan: plan,
			Fingerprint: "sha256:abc", Labels: runbook.L{"operation": "u-42"},
		},
		runbook.Event{Kind: runbook.EventRunFinished, Run: "r1", TS: t0.Add(time.Minute), Outcome: runbook.OutcomePassed},
	)

	assert.Equal(t, plan, s.Plan)
	assert.Equal(t, "sha256:abc", s.Fingerprint)
	assert.Equal(t, runbook.L{"operation": "u-42"}, s.Labels)
	assert.Equal(t, []runbook.AttemptState{{
		Run: "r1", Attempt: 1, Started: t0, Finished: t0.Add(time.Minute), Outcome: runbook.OutcomePassed,
	}}, s.Attempts)
	assert.Equal(t, int64(2), s.LastSeq)
}

func TestReplay_ValuesAndItemsKeepTheLatest(t *testing.T) {
	t.Parallel()

	s := replay(t,
		runStarted("r1", 1),
		runbook.Event{Kind: runbook.EventValue, Run: "r1", Path: "p/backup", Key: "dir", Value: json.RawMessage(`"/old"`)},
		runbook.Event{Kind: runbook.EventItem, Run: "r1", Path: "p/stage", Item: "node7", Status: runbook.ItemFailed, Message: "timeout"},
		runbook.Event{Kind: runbook.EventItem, Run: "r1", Path: "p/stage", Item: "node8", Status: runbook.ItemPassed},
		runStarted("r2", 2),
		runbook.Event{Kind: runbook.EventValue, Run: "r2", Path: "p/backup", Key: "dir", Value: json.RawMessage(`"/new"`)},
		runbook.Event{Kind: runbook.EventItem, Run: "r2", Path: "p/stage", Item: "node7", Status: runbook.ItemPassed},
	)

	v, ok := s.Value("p/backup", "dir")
	require.True(t, ok)
	assert.JSONEq(t, `"/new"`, string(v))

	_, ok = s.Value("p/backup", "missing")
	assert.False(t, ok)

	items := s.Items("p/stage")
	assert.Equal(t, map[string]runbook.ItemState{
		"node7": {Status: runbook.ItemPassed, Run: "r2"},
		"node8": {Status: runbook.ItemPassed, Run: "r1"},
	}, items)

	delete(items, "node7")
	assert.Len(t, s.Items("p/stage"), 2, "Items returns a copy")
}

func TestReplay_BestEffortEventsChangeNoState(t *testing.T) {
	t.Parallel()

	s := replay(t,
		runStarted("r1", 1),
		runbook.Event{Kind: runbook.EventOutput, Run: "r1", Path: "p/a", Line: "x"},
		runbook.Event{Kind: runbook.EventProgress, Run: "r1", Path: "p/a", Key: "k", Current: 1, Total: 2},
		runbook.Event{Kind: runbook.EventMessage, Run: "r1", Path: "p/a", Level: "warn", Message: "w"},
		runbook.Event{Kind: runbook.EventCustom, Run: "r1", Path: "p/a", Type: "x.y", Data: json.RawMessage(`{}`)},
		runbook.Event{Kind: runbook.EventStepRetry, Run: "r1", Path: "p/a", Try: 1},
	)

	assert.Equal(t, runbook.StatusNotReached, s.Element("p/a").Status)
}

func TestReplay_RefusesBadJournals(t *testing.T) {
	t.Parallel()

	_, err := runbook.Replay([]runbook.Event{{V: 2, Seq: 1, Kind: runbook.EventRunStarted}})
	require.ErrorContains(t, err, "schema version 2 is newer")

	_, err = runbook.Replay([]runbook.Event{
		{V: 1, Seq: 2, Kind: runbook.EventRunStarted},
		{V: 1, Seq: 2, Kind: runbook.EventRunFinished},
	})
	require.ErrorContains(t, err, "sequence numbers must increase")
}

func TestEventKind_Durable(t *testing.T) {
	t.Parallel()

	for _, k := range []runbook.EventKind{
		runbook.EventRunStarted, runbook.EventRunFinished, runbook.EventGroupStarted, runbook.EventGroupFinished,
		runbook.EventStepStarted, runbook.EventStepRunning, runbook.EventStepFinished, runbook.EventValue, runbook.EventItem,
		"some.future.kind",
	} {
		assert.True(t, k.Durable(), k)
	}

	for _, k := range []runbook.EventKind{
		runbook.EventOutput, runbook.EventProgress, runbook.EventMessage, runbook.EventCustom, runbook.EventStepRetry,
	} {
		assert.False(t, k.Durable(), k)
	}
}
