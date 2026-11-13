package runbookterm_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ruffel/pipeline/observers/terminal/runbookterm"
	"github.com/ruffel/pipeline/runbook"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Writing to a buffer, the observer uses no color, so output compares
// exactly.

func render(t *testing.T, events ...runbook.Event) string {
	t.Helper()

	var buf bytes.Buffer

	obs := runbookterm.New(&buf)
	for _, e := range events {
		obs.OnEvent(t.Context(), e)
	}

	return buf.String()
}

// lines drops the boxes' right edges and padding, so expectations stay
// readable.
func lines(s string) string {
	out := strings.Split(s, "\n")
	for i, l := range out {
		out[i] = strings.TrimRight(strings.TrimSuffix(l, "│"), " ")
	}

	return strings.Join(out, "\n")
}

func upgrade() *runbook.NodeSpec {
	return &runbook.NodeSpec{
		Name: "upgrade", Path: "upgrade", Kind: runbook.KindGroup, Description: "Upgrade the cluster",
		Children: []runbook.NodeSpec{
			{
				Name: "prechecks", Path: "upgrade/prechecks", Kind: runbook.KindGroup, Description: "Read-only checks",
				Children: []runbook.NodeSpec{
					{Name: "ring", Path: "upgrade/prechecks/ring", Kind: runbook.KindEnsure},
					{Name: "disks", Path: "upgrade/prechecks/disks", Kind: runbook.KindEnsure},
				},
			},
			{Name: "backup", Path: "upgrade/backup", Kind: runbook.KindEffect},
			{Name: "apply", Path: "upgrade/apply", Kind: runbook.KindEffect, Description: "Install the packages"},
		},
	}
}

func TestObserver_SequentialRun(t *testing.T) {
	t.Parallel()

	out := render(t,
		runbook.Event{Kind: runbook.EventRunStarted, Path: "upgrade", Plan: upgrade(), Attempt: 2},
		runbook.Event{Kind: runbook.EventGroupStarted, Path: "upgrade/prechecks"},
		runbook.Event{Kind: runbook.EventStepStarted, Path: "upgrade/prechecks/ring"},
		runbook.Event{Kind: runbook.EventStepRunning, Path: "upgrade/prechecks/ring"},
		runbook.Event{Kind: runbook.EventStepFinished, Path: "upgrade/prechecks/ring", Outcome: runbook.OutcomePassed, DurationMs: 1800},
		runbook.Event{Kind: runbook.EventStepStarted, Path: "upgrade/prechecks/disks"},
		runbook.Event{Kind: runbook.EventItem, Path: "upgrade/prechecks/disks", Item: "node1", Status: runbook.ItemPassed},
		runbook.Event{Kind: runbook.EventItem, Path: "upgrade/prechecks/disks", Item: "node2", Status: runbook.ItemFailed, Message: "92% full"},
		runbook.Event{Kind: runbook.EventItem, Path: "upgrade/prechecks/disks", Item: "node2", Status: runbook.ItemPassed, Message: "cleaned"},
		runbook.Event{Kind: runbook.EventItem, Path: "upgrade/prechecks/disks", Item: "node3", Status: runbook.ItemSkipped, Message: "down"},
		runbook.Event{Kind: runbook.EventStepFinished, Path: "upgrade/prechecks/disks", Outcome: runbook.OutcomePassed, DurationMs: 900},
		runbook.Event{Kind: runbook.EventGroupFinished, Path: "upgrade/prechecks", Outcome: runbook.OutcomePassed, DurationMs: 2700},
		runbook.Event{Kind: runbook.EventStepFinished, Path: "upgrade/backup", Outcome: runbook.OutcomeSkipped, Reason: runbook.ReasonDone},
		runbook.Event{Kind: runbook.EventStepStarted, Path: "upgrade/apply"},
		runbook.Event{Kind: runbook.EventValue, Path: "upgrade/apply", Key: "previous", Value: json.RawMessage(`"8.1"`)},
		runbook.Event{Kind: runbook.EventOutput, Path: "upgrade/apply", Stream: "stdout", Line: "Unpacking hyperstore (8.2) ..."},
		runbook.Event{Kind: runbook.EventOutput, Path: "upgrade/apply", Stream: "stderr", Line: "E: dpkg was interrupted"},
		runbook.Event{Kind: runbook.EventMessage, Path: "upgrade/apply", Level: runbook.LevelWarn, Message: "careful"},
		runbook.Event{Kind: runbook.EventProgress, Path: "upgrade/apply", Message: "configuring", Current: 2, Total: 3},
		runbook.Event{Kind: runbook.EventCustom, Path: "upgrade/apply", Type: "deploy.ready", Data: json.RawMessage(`{"port":443}`)},
		runbook.Event{Kind: runbook.EventStepRetry, Path: "upgrade/apply", Try: 1, Error: &runbook.ErrorInfo{Message: "flaky"}},
		runbook.Event{
			Kind: runbook.EventStepFinished, Path: "upgrade/apply", Outcome: runbook.OutcomeFailed,
			Error: &runbook.ErrorInfo{Message: "exit status 1"}, DurationMs: 400,
		},
		runbook.Event{
			Kind: runbook.EventRunFinished, Path: "upgrade", Outcome: runbook.OutcomeFailed,
			Error: &runbook.ErrorInfo{Message: "upgrade/apply: exit status 1"}, DurationMs: 3100,
		},
	)

	assert.Equal(t, `╭────────────────────────────────────────────────────────────────────────╮
│                            Runbook: upgrade                            │
│                          Upgrade the cluster                           │
│                               attempt 2                                │
│                       prechecks ➔ backup ➔ apply                       │
╰────────────────────────────────────────────────────────────────────────╯
▶ prechecks — Read-only checks
│ ✓ ring (1.8s)
│ · disks
│ │ ✓ node1
│ │ ✗ node2 — 92% full
│ │ ✓ node2 — cleaned
│ │ ⊘ node3 — down
│ ✓ disks · 2 passed, 1 skipped (900ms)
✓ prechecks (2.7s)
⊘ backup — done
· apply — Install the packages
│ Unpacking hyperstore (8.2) ...
│ E: dpkg was interrupted
│ [WARN] careful
│ ◷ configuring (2/3)
│ [deploy.ready] {"port":443}
│ ↻ try 1 failed — flaky; retrying
✗ apply — exit status 1 (400ms)
╭────────────────────────────────────────────────────────────────────────╮
│ Runbook failed                                                         │
│                                                                        │
│ Failed:                                                                │
│   ✗ apply — exit status 1                                              │
│                                                                        │
│ Total time: 3.1s                                                       │
╰────────────────────────────────────────────────────────────────────────╯
`, out)
}

func rolling() *runbook.NodeSpec {
	return &runbook.NodeSpec{
		Name: "upgrade", Path: "upgrade", Kind: runbook.KindGroup,
		Children: []runbook.NodeSpec{
			{Name: "nodes", Path: "upgrade/nodes", Kind: runbook.KindGroup, Parallel: true, Children: []runbook.NodeSpec{
				{Name: "node1", Path: "upgrade/nodes/node1", Kind: runbook.KindGroup, Children: []runbook.NodeSpec{
					{Name: "packages", Path: "upgrade/nodes/node1/packages", Kind: runbook.KindEffect},
				}},
				{Name: "node10", Path: "upgrade/nodes/node10", Kind: runbook.KindEffect},
			}},
		},
	}
}

func TestObserver_ParallelLinesNameTheirElement(t *testing.T) {
	t.Parallel()

	out := render(t,
		runbook.Event{Kind: runbook.EventRunStarted, Path: "upgrade", Plan: rolling()},
		runbook.Event{Kind: runbook.EventGroupStarted, Path: "upgrade/nodes"},
		runbook.Event{Kind: runbook.EventGroupStarted, Path: "upgrade/nodes/node1"},
		runbook.Event{Kind: runbook.EventStepStarted, Path: "upgrade/nodes/node10"},
		runbook.Event{Kind: runbook.EventStepStarted, Path: "upgrade/nodes/node1/packages"},
		runbook.Event{Kind: runbook.EventOutput, Path: "upgrade/nodes/node1/packages", Stream: "stdout", Line: "Unpacking"},
		runbook.Event{Kind: runbook.EventOutput, Path: "upgrade/nodes/node10", Stream: "stderr", Line: "warning: low disk"},
		runbook.Event{Kind: runbook.EventItem, Path: "upgrade/nodes/node10", Item: "disk0", Status: runbook.ItemPassed},
		runbook.Event{Kind: runbook.EventStepFinished, Path: "upgrade/nodes/node1/packages", Outcome: runbook.OutcomePassed, DurationMs: 4000},
		runbook.Event{Kind: runbook.EventGroupFinished, Path: "upgrade/nodes/node1", Outcome: runbook.OutcomePassed, DurationMs: 4100},
		runbook.Event{Kind: runbook.EventStepFinished, Path: "upgrade/nodes/node10", Outcome: runbook.OutcomePassed, DurationMs: 5000},
		runbook.Event{Kind: runbook.EventGroupFinished, Path: "upgrade/nodes", Outcome: runbook.OutcomePassed, DurationMs: 5000},
		runbook.Event{Kind: runbook.EventRunFinished, Path: "upgrade", Outcome: runbook.OutcomePassed, DurationMs: 5000},
	)

	assert.Equal(t, `▶ nodes
│ ▶ node1
│ │ [node1/packages] │ Unpacking
│ │ [node10        ] ┃ warning: low disk
│ │ [node10        ] │ ✓ disk0
│ ✓ node1/packages (4s)
│ ✓ node1 (4.1s)
│ ✓ node10 · 1 passed (5s)
✓ nodes (5s)
`, between(t, out))
}

func TestObserver_NestedParallelGroupsNameInFull(t *testing.T) {
	t.Parallel()

	spec := &runbook.NodeSpec{Name: "run", Path: "run", Kind: runbook.KindGroup, Children: []runbook.NodeSpec{
		{Name: "regions", Path: "run/regions", Kind: runbook.KindGroup, Parallel: true, Children: []runbook.NodeSpec{
			{Name: "eu", Path: "run/regions/eu", Kind: runbook.KindGroup, Parallel: true, Children: []runbook.NodeSpec{
				{Name: "n1", Path: "run/regions/eu/n1", Kind: runbook.KindEffect},
			}},
			{Name: "us", Path: "run/regions/us", Kind: runbook.KindGroup, Parallel: true, Children: []runbook.NodeSpec{
				{Name: "n1", Path: "run/regions/us/n1", Kind: runbook.KindEffect},
			}},
		}},
	}}

	out := render(t,
		runbook.Event{Kind: runbook.EventRunStarted, Path: "run", Plan: spec},
		runbook.Event{Kind: runbook.EventOutput, Path: "run/regions/eu/n1", Line: "a"},
		runbook.Event{Kind: runbook.EventOutput, Path: "run/regions/us/n1", Line: "b"},
		runbook.Event{Kind: runbook.EventStepFinished, Path: "run/regions/us/n1", Outcome: runbook.OutcomePassed},
		runbook.Event{Kind: runbook.EventRunFinished, Path: "run", Outcome: runbook.OutcomePassed},
	)

	assert.Equal(t, `│ │ [eu/n1] │ a
│ │ [us/n1] │ b
│ ✓ us/n1
`, between(t, out))
}

// between returns what a run wrote between its two boxes.
func between(t *testing.T, out string) string {
	t.Helper()

	parts := strings.Split(out, "╯\n")
	require.Len(t, parts, 3)

	return strings.SplitN(parts[1], "╭", 2)[0]
}

func TestObserver_StopsGatesAndResets(t *testing.T) {
	t.Parallel()

	who := "alice"
	until := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

	out := render(t,
		runbook.Event{Kind: runbook.EventRunStarted, Path: "upgrade", Plan: upgrade()},
		runbook.Event{Kind: runbook.EventStepStarted, Path: "upgrade/approve"},
		runbook.Event{Kind: runbook.EventGateWaiting, Path: "upgrade/approve", Prompt: "Upgrade region 2?", Deadline: until},
		runbook.Event{Kind: runbook.EventGateReleased, Path: "upgrade/approve", By: "control", Who: who},
		runbook.Event{Kind: runbook.EventStepFinished, Path: "upgrade/approve", Outcome: runbook.OutcomePassed},
		runbook.Event{Kind: runbook.EventGroupReset, Path: "upgrade/prechecks"},
		runbook.Event{Kind: runbook.EventStopRequested, Who: who},
		runbook.Event{Kind: runbook.EventStepFinished, Path: "upgrade/apply", Outcome: runbook.OutcomeSkipped, Reason: runbook.ReasonNotReached},
		runbook.Event{
			Kind: runbook.EventRunFinished, Path: "upgrade", Outcome: runbook.OutcomeStopped,
			Error: &runbook.ErrorInfo{Message: "runbook: stopped"}, DurationMs: 60000,
		},
	)

	assert.Equal(t, `⏸ approve — Upgrade region 2? (waiting until 2026-11-13T12:00:00Z)
▶ approve approved by alice (control)
✓ approve
↺ prechecks reset: a resume runs it again from the start
■ stop requested by alice; running steps will finish
⊘ apply — not-reached
`, between(t, out))
	assert.Contains(t, lines(out), `│ Runbook stopped
│
│ runbook: stopped
│
│ Running it again resumes where it stopped.
│
│ Total time: 1m0s`)
}

func TestObserver_Rollback(t *testing.T) {
	t.Parallel()

	out := render(t,
		runbook.Event{Kind: runbook.EventRunStarted, Path: "upgrade", Plan: upgrade(), Attempt: 3, Rollback: true},
		runbook.Event{Kind: runbook.EventUndoStarted, Path: "upgrade/apply"},
		runbook.Event{Kind: runbook.EventUndoFinished, Path: "upgrade/apply", Outcome: runbook.OutcomeUndone, DurationMs: 1200},
		runbook.Event{Kind: runbook.EventUndoFinished, Path: "upgrade/backup", Outcome: runbook.OutcomeSkipped, Reason: runbook.ReasonNoUndo},
		runbook.Event{Kind: runbook.EventUndoStarted, Path: "upgrade/prechecks/ring"},
		runbook.Event{
			Kind: runbook.EventUndoFinished, Path: "upgrade/prechecks/ring", Outcome: runbook.OutcomeFailed,
			Error: &runbook.ErrorInfo{Message: "ring locked"},
		},
		runbook.Event{Kind: runbook.EventRunFinished, Path: "upgrade", Outcome: runbook.OutcomeFailed, DurationMs: 1300},
	)

	assert.Contains(t, lines(out), "rollback · attempt 3")
	assert.Contains(t, out, `· apply — Install the packages
↶ apply undone (1.2s)
⊘ backup not undone — no-undo
│ ✗ ring undo failed — ring locked
`)
	assert.Contains(t, lines(out), `│ Rollback failed
│
│ Failed:
│   ✗ undo of prechecks/ring — ring locked`)
}

// TestObserver_EachRunStartsAfresh covers a viewer that follows a journal
// from the beginning, and so sees every attempt.
func TestObserver_EachRunStartsAfresh(t *testing.T) {
	t.Parallel()

	out := render(t,
		runbook.Event{Kind: runbook.EventRunStarted, Path: "upgrade", Plan: upgrade()},
		runbook.Event{Kind: runbook.EventStepStarted, Path: "upgrade/apply"},
		runbook.Event{Kind: runbook.EventItem, Path: "upgrade/apply", Item: "node1", Status: runbook.ItemFailed},
		runbook.Event{Kind: runbook.EventStopRequested},
		runbook.Event{Kind: runbook.EventStepFinished, Path: "upgrade/backup", Outcome: runbook.OutcomeFailed},
		runbook.Event{Kind: runbook.EventRunFinished, Path: "upgrade", Outcome: runbook.OutcomeFailed},
		runbook.Event{Kind: runbook.EventRunStarted, Path: "upgrade", Plan: upgrade(), Attempt: 2},
		runbook.Event{Kind: runbook.EventStepStarted, Path: "upgrade/apply"},
		runbook.Event{Kind: runbook.EventItem, Path: "upgrade/apply", Item: "node2", Status: runbook.ItemPassed},
		runbook.Event{Kind: runbook.EventStepFinished, Path: "upgrade/apply", Outcome: runbook.OutcomePassed},
		runbook.Event{Kind: runbook.EventRunFinished, Path: "upgrade", Outcome: runbook.OutcomePassed},
	)

	second := out[strings.LastIndex(out, "attempt 2"):]
	assert.Contains(t, second, "✓ apply · 1 passed\n")
	assert.NotContains(t, second, "Failed:")
}

func TestObserver_ARealRun(t *testing.T) {
	t.Parallel()

	plan, err := runbook.NewPlan("deploy",
		runbook.Group("hosts",
			runbook.Effect("web1", runbook.StepFunc(func(_ context.Context, sc *runbook.StepContext) error {
				return sc.Item("disk0", runbook.ItemPassed, "")
			})),
			runbook.Effect("web2", runbook.StepFunc(func(context.Context, *runbook.StepContext) error {
				return errors.New("unreachable")
			})),
		).Parallel(2),
	)
	require.NoError(t, err)

	var buf syncBuffer

	_, err = runbook.Run(t.Context(), plan, runbook.MemoryJournal(), runbook.WithObservers(runbookterm.New(&buf)))
	require.Error(t, err)

	out := buf.String()
	assert.Contains(t, out, "Runbook: deploy")
	assert.Contains(t, out, "│ ✓ web1 · 1 passed")
	assert.Contains(t, out, "│ ✗ web2 — unreachable")
	assert.Contains(t, out, "✗ hosts/web2 — unreachable")
	assert.Contains(t, out, "Runbook failed")
}

// TestObserver_ConcurrentEvents runs under -race: parallel steps call the
// observer from several goroutines.
func TestObserver_ConcurrentEvents(t *testing.T) {
	t.Parallel()

	var buf syncBuffer

	obs := runbookterm.New(&buf)
	obs.OnEvent(t.Context(), runbook.Event{Kind: runbook.EventRunStarted, Path: "upgrade", Plan: rolling()})

	var wg sync.WaitGroup

	for _, path := range []string{"upgrade/nodes/node1/packages", "upgrade/nodes/node10"} {
		wg.Go(func() {
			for range 100 {
				obs.OnEvent(context.Background(), runbook.Event{Kind: runbook.EventItem, Path: path, Item: "x", Status: runbook.ItemPassed})
			}

			obs.OnEvent(context.Background(), runbook.Event{Kind: runbook.EventStepFinished, Path: path, Outcome: runbook.OutcomePassed})
		})
	}

	wg.Wait()

	assert.Equal(t, 2, strings.Count(buf.String(), "· 1 passed"))
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}
