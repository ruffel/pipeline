package terminal_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ruffel/pipeline"
	"github.com/ruffel/pipeline/observers/terminal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Writing to a buffer, the observer uses no color, so output compares
// exactly.

func render(t *testing.T, events ...pipeline.Event) string {
	t.Helper()

	var buf bytes.Buffer

	obs := terminal.New(&buf)
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

func upgrade() *pipeline.NodeSpec {
	return &pipeline.NodeSpec{
		Name: "upgrade", Path: "upgrade", Kind: pipeline.KindGroup, Description: "Upgrade the cluster",
		Children: []pipeline.NodeSpec{
			{
				Name: "prechecks", Path: "upgrade/prechecks", Kind: pipeline.KindGroup, Description: "Read-only checks",
				Children: []pipeline.NodeSpec{
					{Name: "ring", Path: "upgrade/prechecks/ring", Kind: pipeline.KindEnsure},
					{Name: "disks", Path: "upgrade/prechecks/disks", Kind: pipeline.KindEnsure},
				},
			},
			{Name: "backup", Path: "upgrade/backup", Kind: pipeline.KindEffect},
			{Name: "apply", Path: "upgrade/apply", Kind: pipeline.KindEffect, Description: "Install the packages"},
		},
	}
}

func TestObserver_SequentialRun(t *testing.T) {
	t.Parallel()

	out := render(t,
		pipeline.Event{Kind: pipeline.EventRunStarted, Path: "upgrade", Plan: upgrade(), Attempt: 2},
		pipeline.Event{Kind: pipeline.EventGroupStarted, Path: "upgrade/prechecks"},
		pipeline.Event{Kind: pipeline.EventStepStarted, Path: "upgrade/prechecks/ring"},
		pipeline.Event{Kind: pipeline.EventStepRunning, Path: "upgrade/prechecks/ring"},
		pipeline.Event{Kind: pipeline.EventStepFinished, Path: "upgrade/prechecks/ring", Outcome: pipeline.OutcomePassed, DurationMs: 1800},
		pipeline.Event{Kind: pipeline.EventStepStarted, Path: "upgrade/prechecks/disks"},
		pipeline.Event{Kind: pipeline.EventItem, Path: "upgrade/prechecks/disks", Item: "node1", Status: pipeline.ItemPassed},
		pipeline.Event{Kind: pipeline.EventItem, Path: "upgrade/prechecks/disks", Item: "node2", Status: pipeline.ItemFailed, Message: "92% full"},
		pipeline.Event{Kind: pipeline.EventItem, Path: "upgrade/prechecks/disks", Item: "node2", Status: pipeline.ItemPassed, Message: "cleaned"},
		pipeline.Event{Kind: pipeline.EventItem, Path: "upgrade/prechecks/disks", Item: "node3", Status: pipeline.ItemSkipped, Message: "down"},
		pipeline.Event{Kind: pipeline.EventStepFinished, Path: "upgrade/prechecks/disks", Outcome: pipeline.OutcomePassed, DurationMs: 900},
		pipeline.Event{Kind: pipeline.EventGroupFinished, Path: "upgrade/prechecks", Outcome: pipeline.OutcomePassed, DurationMs: 2700},
		pipeline.Event{Kind: pipeline.EventStepFinished, Path: "upgrade/backup", Outcome: pipeline.OutcomeSkipped, Reason: pipeline.ReasonDone},
		pipeline.Event{Kind: pipeline.EventStepStarted, Path: "upgrade/apply"},
		pipeline.Event{Kind: pipeline.EventValue, Path: "upgrade/apply", Key: "previous", Value: json.RawMessage(`"8.1"`)},
		pipeline.Event{Kind: pipeline.EventOutput, Path: "upgrade/apply", Stream: "stdout", Line: "Unpacking hyperstore (8.2) ..."},
		pipeline.Event{Kind: pipeline.EventOutput, Path: "upgrade/apply", Stream: "stderr", Line: "E: dpkg was interrupted"},
		pipeline.Event{Kind: pipeline.EventMessage, Path: "upgrade/apply", Level: pipeline.LevelWarn, Message: "careful"},
		pipeline.Event{Kind: pipeline.EventProgress, Path: "upgrade/apply", Message: "configuring", Current: 2, Total: 3},
		pipeline.Event{Kind: pipeline.EventCustom, Path: "upgrade/apply", Type: "deploy.ready", Data: json.RawMessage(`{"port":443}`)},
		pipeline.Event{Kind: pipeline.EventStepRetry, Path: "upgrade/apply", Try: 1, Error: &pipeline.ErrorInfo{Message: "flaky"}},
		pipeline.Event{
			Kind: pipeline.EventStepFinished, Path: "upgrade/apply", Outcome: pipeline.OutcomeFailed,
			Error: &pipeline.ErrorInfo{Message: "exit status 1"}, DurationMs: 400,
		},
		pipeline.Event{
			Kind: pipeline.EventRunFinished, Path: "upgrade", Outcome: pipeline.OutcomeFailed,
			Error: &pipeline.ErrorInfo{Message: "upgrade/apply: exit status 1"}, DurationMs: 3100,
		},
	)

	assert.Equal(t, `╭────────────────────────────────────────────────────────────────────────╮
│                           Pipeline: upgrade                            │
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
│ Pipeline failed                                                        │
│                                                                        │
│ Failed:                                                                │
│   ✗ apply — exit status 1                                              │
│                                                                        │
│ Total time: 3.1s                                                       │
╰────────────────────────────────────────────────────────────────────────╯
`, out)
}

func rolling() *pipeline.NodeSpec {
	return &pipeline.NodeSpec{
		Name: "upgrade", Path: "upgrade", Kind: pipeline.KindGroup,
		Children: []pipeline.NodeSpec{
			{Name: "nodes", Path: "upgrade/nodes", Kind: pipeline.KindGroup, Parallel: true, Children: []pipeline.NodeSpec{
				{Name: "node1", Path: "upgrade/nodes/node1", Kind: pipeline.KindGroup, Children: []pipeline.NodeSpec{
					{Name: "packages", Path: "upgrade/nodes/node1/packages", Kind: pipeline.KindEffect},
				}},
				{Name: "node10", Path: "upgrade/nodes/node10", Kind: pipeline.KindEffect},
			}},
		},
	}
}

func TestObserver_ParallelLinesNameTheirElement(t *testing.T) {
	t.Parallel()

	out := render(t,
		pipeline.Event{Kind: pipeline.EventRunStarted, Path: "upgrade", Plan: rolling()},
		pipeline.Event{Kind: pipeline.EventGroupStarted, Path: "upgrade/nodes"},
		pipeline.Event{Kind: pipeline.EventGroupStarted, Path: "upgrade/nodes/node1"},
		pipeline.Event{Kind: pipeline.EventStepStarted, Path: "upgrade/nodes/node10"},
		pipeline.Event{Kind: pipeline.EventStepStarted, Path: "upgrade/nodes/node1/packages"},
		pipeline.Event{Kind: pipeline.EventOutput, Path: "upgrade/nodes/node1/packages", Stream: "stdout", Line: "Unpacking"},
		pipeline.Event{Kind: pipeline.EventOutput, Path: "upgrade/nodes/node10", Stream: "stderr", Line: "warning: low disk"},
		pipeline.Event{Kind: pipeline.EventItem, Path: "upgrade/nodes/node10", Item: "disk0", Status: pipeline.ItemPassed},
		pipeline.Event{Kind: pipeline.EventStepFinished, Path: "upgrade/nodes/node1/packages", Outcome: pipeline.OutcomePassed, DurationMs: 4000},
		pipeline.Event{Kind: pipeline.EventGroupFinished, Path: "upgrade/nodes/node1", Outcome: pipeline.OutcomePassed, DurationMs: 4100},
		pipeline.Event{Kind: pipeline.EventStepFinished, Path: "upgrade/nodes/node10", Outcome: pipeline.OutcomePassed, DurationMs: 5000},
		pipeline.Event{Kind: pipeline.EventGroupFinished, Path: "upgrade/nodes", Outcome: pipeline.OutcomePassed, DurationMs: 5000},
		pipeline.Event{Kind: pipeline.EventRunFinished, Path: "upgrade", Outcome: pipeline.OutcomePassed, DurationMs: 5000},
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

	spec := &pipeline.NodeSpec{Name: "run", Path: "run", Kind: pipeline.KindGroup, Children: []pipeline.NodeSpec{
		{Name: "regions", Path: "run/regions", Kind: pipeline.KindGroup, Parallel: true, Children: []pipeline.NodeSpec{
			{Name: "eu", Path: "run/regions/eu", Kind: pipeline.KindGroup, Parallel: true, Children: []pipeline.NodeSpec{
				{Name: "n1", Path: "run/regions/eu/n1", Kind: pipeline.KindEffect},
			}},
			{Name: "us", Path: "run/regions/us", Kind: pipeline.KindGroup, Parallel: true, Children: []pipeline.NodeSpec{
				{Name: "n1", Path: "run/regions/us/n1", Kind: pipeline.KindEffect},
			}},
		}},
	}}

	out := render(t,
		pipeline.Event{Kind: pipeline.EventRunStarted, Path: "run", Plan: spec},
		pipeline.Event{Kind: pipeline.EventOutput, Path: "run/regions/eu/n1", Line: "a"},
		pipeline.Event{Kind: pipeline.EventOutput, Path: "run/regions/us/n1", Line: "b"},
		pipeline.Event{Kind: pipeline.EventStepFinished, Path: "run/regions/us/n1", Outcome: pipeline.OutcomePassed},
		pipeline.Event{Kind: pipeline.EventRunFinished, Path: "run", Outcome: pipeline.OutcomePassed},
	)

	assert.Equal(t, `│ │ [eu/n1] │ a
│ │ [us/n1] │ b
│ ✓ us/n1
`, between(t, out))
}

func TestObserver_UndoCountsItsItems(t *testing.T) {
	t.Parallel()

	out := render(t,
		pipeline.Event{Kind: pipeline.EventRunStarted, Path: "upgrade", Plan: upgrade(), Rollback: true},
		pipeline.Event{Kind: pipeline.EventUndoStarted, Path: "upgrade/backup"},
		pipeline.Event{Kind: pipeline.EventItem, Path: "upgrade/backup", Item: "node1", Status: pipeline.ItemPassed},
		pipeline.Event{Kind: pipeline.EventUndoFinished, Path: "upgrade/backup", Outcome: pipeline.OutcomeUndone},
		pipeline.Event{Kind: pipeline.EventRunFinished, Path: "upgrade", Outcome: pipeline.OutcomePassed},
	)

	assert.Equal(t, `· backup
│ ✓ node1
↶ backup undone · 1 passed
`, between(t, out))
}

func TestObserver_EveryLineOfAMultilineDetailIsPrefixed(t *testing.T) {
	t.Parallel()

	out := render(t,
		pipeline.Event{Kind: pipeline.EventRunStarted, Path: "upgrade", Plan: rolling()},
		pipeline.Event{Kind: pipeline.EventMessage, Path: "upgrade/nodes/node10", Level: pipeline.LevelInfo, Message: "checking\ncomplete"},
		pipeline.Event{Kind: pipeline.EventItem, Path: "upgrade/nodes/node10", Item: "disk0", Status: pipeline.ItemFailed, Message: "a\nb"},
		pipeline.Event{Kind: pipeline.EventRunFinished, Path: "upgrade", Outcome: pipeline.OutcomePassed},
	)

	assert.Equal(t, `│ │ [node10        ] │ [INFO] checking
│ │ [node10        ] │ complete
│ │ [node10        ] │ ✗ disk0 — a
│ │ [node10        ] │ b
`, between(t, out))
}

func TestObserver_MultilineErrorsStayInTheTree(t *testing.T) {
	t.Parallel()

	out := render(t,
		pipeline.Event{Kind: pipeline.EventRunStarted, Path: "upgrade", Plan: upgrade()},
		pipeline.Event{
			Kind: pipeline.EventGroupFinished, Path: "upgrade/prechecks", Outcome: pipeline.OutcomeFailed,
			Error: &pipeline.ErrorInfo{Message: "ring: down\ndisks: full"},
		},
		pipeline.Event{
			Kind: pipeline.EventStepFinished, Path: "upgrade/prechecks/ring", Outcome: pipeline.OutcomeFailed,
			Error: &pipeline.ErrorInfo{Message: "a\nb"},
		},
		pipeline.Event{Kind: pipeline.EventRunFinished, Path: "upgrade", Outcome: pipeline.OutcomeFailed},
	)

	assert.Equal(t, `✗ prechecks — ring: down
  disks: full
│ ✗ ring — a
│   b
`, between(t, out))
	assert.Contains(t, lines(out), `│   ✗ prechecks/ring — a
│     b`)
}

// TestObserver_CancelledStepsAreListedApart covers a cancel: the steps it
// cut short didn't fail on their own.
func TestObserver_CancelledStepsAreListedApart(t *testing.T) {
	t.Parallel()

	cancelled := &pipeline.ErrorInfo{Kind: pipeline.ErrorCancelled, Message: "context canceled"}

	out := render(t,
		pipeline.Event{Kind: pipeline.EventRunStarted, Path: "upgrade", Plan: upgrade()},
		pipeline.Event{
			Kind: pipeline.EventStepFinished, Path: "upgrade/backup", Outcome: pipeline.OutcomeFailed,
			Error: &pipeline.ErrorInfo{Message: "disk full"},
		},
		pipeline.Event{Kind: pipeline.EventStepFinished, Path: "upgrade/apply", Outcome: pipeline.OutcomeFailed, Error: cancelled},
		// A cancel during a retry's backoff keeps the try's error.
		pipeline.Event{
			Kind: pipeline.EventStepFinished, Path: "upgrade/prechecks/ring", Outcome: pipeline.OutcomeFailed,
			Error: &pipeline.ErrorInfo{Kind: pipeline.ErrorCancelled, Message: "ring down\ncontext canceled"},
		},
		pipeline.Event{Kind: pipeline.EventRunFinished, Path: "upgrade", Outcome: pipeline.OutcomeCancelled},
	)

	assert.Contains(t, lines(out), `│ Failed:
│   ✗ backup — disk full
│
│ Cancelled:
│   ■ apply
│   ■ prechecks/ring — ring down
│     context canceled
│
│ Running it again`)
}

// TestObserver_AWaitingGateNamesItself covers a warning from a gate's
// approver while the gate waits: the gate's line already names it.
func TestObserver_AWaitingGateNamesItself(t *testing.T) {
	t.Parallel()

	out := render(t,
		pipeline.Event{Kind: pipeline.EventRunStarted, Path: "upgrade", Plan: upgrade()},
		pipeline.Event{Kind: pipeline.EventStepStarted, Path: "upgrade/approve"},
		pipeline.Event{Kind: pipeline.EventGateWaiting, Path: "upgrade/approve", Prompt: "Go?"},
		pipeline.Event{Kind: pipeline.EventMessage, Path: "upgrade/approve", Level: pipeline.LevelWarn, Message: "approver withdrew"},
		pipeline.Event{Kind: pipeline.EventRunFinished, Path: "upgrade", Outcome: pipeline.OutcomeStopped},
	)

	assert.Equal(t, `⏸ approve — Go?
│ [WARN] approver withdrew
`, between(t, out))
}

// TestObserver_RunLevelMessagesOpenNoStep covers AsyncObserver's warning
// that it dropped events, which has no path.
func TestObserver_RunLevelMessagesOpenNoStep(t *testing.T) {
	t.Parallel()

	out := render(t,
		pipeline.Event{Kind: pipeline.EventRunStarted, Path: "upgrade", Plan: upgrade()},
		pipeline.Event{Kind: pipeline.EventMessage, Level: pipeline.LevelWarn, Message: "observer fell behind"},
		pipeline.Event{Kind: pipeline.EventRunFinished, Path: "upgrade", Outcome: pipeline.OutcomePassed},
	)

	assert.Equal(t, "[WARN] observer fell behind\n", between(t, out))
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
		pipeline.Event{Kind: pipeline.EventRunStarted, Path: "upgrade", Plan: upgrade()},
		pipeline.Event{Kind: pipeline.EventStepStarted, Path: "upgrade/approve"},
		pipeline.Event{Kind: pipeline.EventGateWaiting, Path: "upgrade/approve", Prompt: "Upgrade region 2?", Deadline: until},
		pipeline.Event{Kind: pipeline.EventGateReleased, Path: "upgrade/approve", By: "control", Who: who},
		pipeline.Event{Kind: pipeline.EventStepFinished, Path: "upgrade/approve", Outcome: pipeline.OutcomePassed},
		pipeline.Event{Kind: pipeline.EventGroupReset, Path: "upgrade/prechecks"},
		pipeline.Event{Kind: pipeline.EventStopRequested, Who: who},
		pipeline.Event{Kind: pipeline.EventStepFinished, Path: "upgrade/apply", Outcome: pipeline.OutcomeSkipped, Reason: pipeline.ReasonNotReached},
		pipeline.Event{
			Kind: pipeline.EventRunFinished, Path: "upgrade", Outcome: pipeline.OutcomeStopped,
			Error: &pipeline.ErrorInfo{Message: "pipeline: stopped"}, DurationMs: 60000,
		},
	)

	assert.Equal(t, `⏸ approve — Upgrade region 2? (waiting until 2026-11-13T12:00:00Z)
▶ approve approved by alice (control)
✓ approve
↺ prechecks reset: a resume runs it again from the start
■ stop requested by alice; running steps will finish
⊘ apply — not-reached
`, between(t, out))
	assert.Contains(t, lines(out), `│ Pipeline stopped
│
│ pipeline: stopped
│
│ Running it again resumes where it stopped.
│
│ Total time: 1m0s`)
}

func TestObserver_Rollback(t *testing.T) {
	t.Parallel()

	out := render(t,
		pipeline.Event{Kind: pipeline.EventRunStarted, Path: "upgrade", Plan: upgrade(), Attempt: 3, Rollback: true},
		pipeline.Event{Kind: pipeline.EventUndoStarted, Path: "upgrade/apply"},
		pipeline.Event{Kind: pipeline.EventUndoFinished, Path: "upgrade/apply", Outcome: pipeline.OutcomeUndone, DurationMs: 1200},
		pipeline.Event{Kind: pipeline.EventUndoFinished, Path: "upgrade/backup", Outcome: pipeline.OutcomeSkipped, Reason: pipeline.ReasonNoUndo},
		pipeline.Event{Kind: pipeline.EventUndoStarted, Path: "upgrade/prechecks/ring"},
		pipeline.Event{
			Kind: pipeline.EventUndoFinished, Path: "upgrade/prechecks/ring", Outcome: pipeline.OutcomeFailed,
			Error: &pipeline.ErrorInfo{Message: "ring locked"},
		},
		pipeline.Event{Kind: pipeline.EventRunFinished, Path: "upgrade", Outcome: pipeline.OutcomeFailed, DurationMs: 1300},
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
		pipeline.Event{Kind: pipeline.EventRunStarted, Path: "upgrade", Plan: upgrade()},
		pipeline.Event{Kind: pipeline.EventStepStarted, Path: "upgrade/apply"},
		pipeline.Event{Kind: pipeline.EventItem, Path: "upgrade/apply", Item: "node1", Status: pipeline.ItemFailed},
		pipeline.Event{Kind: pipeline.EventStopRequested},
		pipeline.Event{Kind: pipeline.EventStepFinished, Path: "upgrade/backup", Outcome: pipeline.OutcomeFailed},
		pipeline.Event{Kind: pipeline.EventRunFinished, Path: "upgrade", Outcome: pipeline.OutcomeFailed},
		pipeline.Event{Kind: pipeline.EventRunStarted, Path: "upgrade", Plan: upgrade(), Attempt: 2},
		pipeline.Event{Kind: pipeline.EventStepStarted, Path: "upgrade/apply"},
		pipeline.Event{Kind: pipeline.EventItem, Path: "upgrade/apply", Item: "node2", Status: pipeline.ItemPassed},
		pipeline.Event{Kind: pipeline.EventStepFinished, Path: "upgrade/apply", Outcome: pipeline.OutcomePassed},
		pipeline.Event{Kind: pipeline.EventRunFinished, Path: "upgrade", Outcome: pipeline.OutcomePassed},
	)

	second := out[strings.LastIndex(out, "attempt 2"):]
	assert.Contains(t, second, "✓ apply · 1 passed\n")
	assert.NotContains(t, second, "Failed:")
}

func TestObserver_ARealRun(t *testing.T) {
	t.Parallel()

	plan, err := pipeline.NewPlan("deploy",
		pipeline.Group("hosts",
			pipeline.Effect("web1", pipeline.StepFunc(func(_ context.Context, sc *pipeline.StepContext) error {
				return sc.Item("disk0", pipeline.ItemPassed, "")
			})),
			pipeline.Effect("web2", pipeline.StepFunc(func(context.Context, *pipeline.StepContext) error {
				return errors.New("unreachable")
			})),
		).Parallel(2),
	)
	require.NoError(t, err)

	var buf syncBuffer

	_, err = pipeline.Run(t.Context(), plan, pipeline.MemoryJournal(), pipeline.WithObservers(terminal.New(&buf)))
	require.Error(t, err)

	out := buf.String()
	assert.Contains(t, out, "Pipeline: deploy")
	assert.Contains(t, out, "│ ✓ web1 · 1 passed")
	assert.Contains(t, out, "│ ✗ web2 — unreachable")
	assert.Contains(t, out, "✗ hosts/web2 — unreachable")
	assert.Contains(t, out, "Pipeline failed")
}

// TestObserver_ConcurrentEvents runs under -race: parallel steps call the
// observer from several goroutines.
func TestObserver_ConcurrentEvents(t *testing.T) {
	t.Parallel()

	var buf syncBuffer

	obs := terminal.New(&buf)
	obs.OnEvent(t.Context(), pipeline.Event{Kind: pipeline.EventRunStarted, Path: "upgrade", Plan: rolling()})

	var wg sync.WaitGroup

	for _, path := range []string{"upgrade/nodes/node1/packages", "upgrade/nodes/node10"} {
		wg.Go(func() {
			for range 100 {
				obs.OnEvent(context.Background(), pipeline.Event{Kind: pipeline.EventItem, Path: path, Item: "x", Status: pipeline.ItemPassed})
			}

			obs.OnEvent(context.Background(), pipeline.Event{Kind: pipeline.EventStepFinished, Path: path, Outcome: pipeline.OutcomePassed})
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
