package runbook_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ruffel/pipeline/runbook"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// signals returns a step whose Run returns err.
func signals(err error) runbook.Step {
	return runbook.StepFunc(func(context.Context, *runbook.StepContext) error { return err })
}

// skippedRun returns the contiguous run of trace lines starting at the first
// skipped element.
func skippedRun(lines []string) []string {
	start := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, "skipped/") })
	if start < 0 {
		return nil
	}

	end := start
	for end < len(lines) && strings.Contains(lines[end], "skipped/") {
		end++
	}

	return lines[start:end]
}

// statuses returns "status/reason" for each path.
func statuses(s *runbook.State, paths ...string) map[string]string {
	out := map[string]string{}

	for _, p := range paths {
		el := s.Element(p)
		out[p] = string(el.Status)

		if el.Reason != "" {
			out[p] += "/" + string(el.Reason)
		}
	}

	return out
}

func TestSkipGroup_SkipsTheRestOfTheGroupOnly(t *testing.T) {
	t.Parallel()

	out := runPlan(t, t.Context(), mustPlan(t,
		runbook.Group("g",
			runbook.Effect("a", ok()),
			runbook.Effect("signal", signals(runbook.ErrSkipGroup)),
			runbook.Effect("c", ok()),
			runbook.Group("inner", runbook.Effect("d", ok())),
		),
		runbook.Effect("after", ok()),
	))

	require.NoError(t, out.err)
	assert.Equal(t, runbook.OutcomePassed, out.res.Outcome)
	assert.Equal(t, map[string]string{
		"p/g/a":       "passed",
		"p/g/signal":  "passed",
		"p/g/c":       "skipped/group-skipped",
		"p/g/inner":   "skipped/group-skipped",
		"p/g/inner/d": "skipped/group-skipped",
		"p/g":         "passed",
		"p/after":     "passed",
	}, statuses(out.state, "p/g/a", "p/g/signal", "p/g/c", "p/g/inner", "p/g/inner/d", "p/g", "p/after"))

	signalled := out.state.Element("p/g/signal")
	assert.Equal(t, runbook.SkipGroup, signalled.Skip, "replay keeps the signal, for resume")
	assert.True(t, signalled.Done)
	assert.False(t, out.state.Element("p/g/c").Done, "skipped by a signal is not done")

	// A skipped group is recorded after its descendants, and nothing that was
	// skipped is recorded as started.
	assert.Equal(t, []string{
		"step.finished p/g/c skipped/group-skipped",
		"step.finished p/g/inner/d skipped/group-skipped",
		"group.finished p/g/inner skipped/group-skipped",
	}, skippedRun(trace(out.events)))
}

func TestSkipGroup_InANestedGroupLeavesItsParentRunning(t *testing.T) {
	t.Parallel()

	out := runPlan(t, t.Context(), mustPlan(t,
		runbook.Group("outer",
			runbook.Group("inner",
				runbook.Effect("signal", signals(runbook.ErrSkipGroup)),
				runbook.Effect("skipped", ok()),
			),
			runbook.Effect("next", ok()),
		),
	))

	require.NoError(t, out.err)
	assert.Equal(t, map[string]string{
		"p/outer/inner/skipped": "skipped/group-skipped",
		"p/outer/inner":         "passed",
		"p/outer/next":          "passed",
	}, statuses(out.state, "p/outer/inner/skipped", "p/outer/inner", "p/outer/next"))
}

func TestSkipRun_SkipsEverythingNotStartedAndPasses(t *testing.T) {
	t.Parallel()

	out := runPlan(t, t.Context(), mustPlan(t,
		runbook.Effect("first", ok()),
		runbook.Group("outer",
			runbook.Group("inner",
				runbook.Effect("signal", signals(fmt.Errorf("already upgraded: %w", runbook.ErrSkipRun))),
				runbook.Effect("rest-of-inner", ok()),
			),
			runbook.Effect("rest-of-outer", ok()),
		),
		runbook.Group("later", runbook.Effect("deep", ok())),
	))

	require.NoError(t, out.err)
	assert.Equal(t, runbook.OutcomePassed, out.res.Outcome)
	assert.Equal(t, map[string]string{
		"p/first":                     "passed",
		"p/outer/inner/signal":        "passed",
		"p/outer/inner/rest-of-inner": "skipped/run-skipped",
		"p/outer/inner":               "passed",
		"p/outer/rest-of-outer":       "skipped/run-skipped",
		"p/outer":                     "passed",
		"p/later":                     "skipped/run-skipped",
		"p/later/deep":                "skipped/run-skipped",
	}, statuses(out.state, "p/first", "p/outer/inner/signal", "p/outer/inner/rest-of-inner", "p/outer/inner",
		"p/outer/rest-of-outer", "p/outer", "p/later", "p/later/deep"))
	assert.Equal(t, runbook.SkipRun, out.state.Element("p/outer/inner/signal").Skip)
	assert.False(t, out.state.Unfinished(), "a run ended by ErrSkipRun counts as passed")
}

func TestSkipGroup_InAParallelGroupSkipsQueuedChildren(t *testing.T) {
	t.Parallel()

	out := runPlan(t, t.Context(), mustPlan(t, runbook.Group("g",
		runbook.Effect("signal", signals(runbook.ErrSkipGroup)),
		runbook.Effect("b", ok()),
		runbook.Effect("c", ok()),
	).Parallel(1)))

	require.NoError(t, out.err)
	assert.Equal(t, map[string]string{
		"p/g/b": "skipped/group-skipped",
		"p/g/c": "skipped/group-skipped",
		"p/g":   "passed",
	}, statuses(out.state, "p/g/b", "p/g/c", "p/g"))
}

func TestSkipRun_LetsRunningParallelSiblingsFinish(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})

	out := runPlan(t, t.Context(), mustPlan(t,
		runbook.Group("g",
			runbook.Effect("slow", runbook.StepFunc(func(ctx context.Context, _ *runbook.StepContext) error {
				<-release

				return ctx.Err()
			})),
			runbook.Effect("signal", runbook.StepFunc(func(context.Context, *runbook.StepContext) error {
				defer close(release)

				return runbook.ErrSkipRun
			})),
		).Parallel(0),
		runbook.Effect("after", ok()),
	))

	require.NoError(t, out.err)
	assert.Equal(t, map[string]string{
		"p/g/slow": "passed",
		"p/after":  "skipped/run-skipped",
	}, statuses(out.state, "p/g/slow", "p/after"))
}

func TestSkip_FailuresStillFailTheGroup(t *testing.T) {
	t.Parallel()

	out := runPlan(t, t.Context(), mustPlan(t, runbook.Group("g",
		runbook.Effect("broken", fail("boom")),
		runbook.Effect("signal", signals(runbook.ErrSkipGroup)),
		runbook.Effect("skipped", ok()),
	).ContinueOnError()))

	require.ErrorContains(t, out.err, "p/g/broken: boom")
	assert.Equal(t, map[string]string{
		"p/g/skipped": "skipped/group-skipped",
		"p/g":         "failed",
	}, statuses(out.state, "p/g/skipped", "p/g"))
}

func TestSkip_SignalsAreNotRetried(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	out := runPlan(t, t.Context(), mustPlan(t, runbook.Effect("a", runbook.StepFunc(
		func(context.Context, *runbook.StepContext) error {
			calls.Add(1)

			return runbook.ErrSkipGroup
		})).Retry(3, time.Millisecond)))

	require.NoError(t, out.err)
	assert.Equal(t, int32(1), calls.Load())
}

type signallingCheck struct{}

func (signallingCheck) Check(context.Context, *runbook.StepContext) (bool, error) {
	return false, runbook.ErrSkipRun
}

func (signallingCheck) Run(context.Context, *runbook.StepContext) error { return nil }

func TestSkip_CheckMayNotSignal(t *testing.T) {
	t.Parallel()

	out := runPlan(t, t.Context(), mustPlan(t,
		runbook.Effect("a", signallingCheck{}),
		runbook.Effect("b", ok()),
	))

	require.ErrorContains(t, out.err, "only Run may skip")
	require.NotErrorIs(t, out.err, runbook.ErrSkipRun, "the signal is not passed on")
	assert.Equal(t, runbook.StatusFailed, out.state.Element("p/a").Status)
	assert.Equal(t, runbook.StatusNotReached, out.state.Element("p/b").Status)
}

// A run-wide skip signalled after an earlier failure in a continuing group
// still marks everything not started as run-skipped. The failure stands.
func TestSkipRun_AfterAFailureInAContinuingGroup(t *testing.T) {
	t.Parallel()

	out := runPlan(t, t.Context(), mustPlan(t,
		runbook.Group("outer",
			runbook.Group("inner",
				runbook.Effect("broken", fail("boom")),
				runbook.Effect("signal", signals(runbook.ErrSkipRun)),
			).ContinueOnError(),
			runbook.Effect("next", ok()),
		),
		runbook.Effect("after", ok()),
	))

	require.ErrorContains(t, out.err, "p/outer/inner/broken: boom")
	assert.Equal(t, map[string]string{
		"p/outer/inner": "failed",
		"p/outer/next":  "skipped/run-skipped",
		"p/after":       "skipped/run-skipped",
	}, statuses(out.state, "p/outer/inner", "p/outer/next", "p/after"))
}

// In a bounded parallel group, a signal that arrives after a failure has
// stopped admission still marks the queued children skipped.
func TestSkipGroup_SignalAfterAFailureStoppedAdmission(t *testing.T) {
	t.Parallel()

	failed := make(chan struct{})

	out := runPlan(t, t.Context(), mustPlan(t, runbook.Group("g",
		runbook.Effect("broken", runbook.StepFunc(func(context.Context, *runbook.StepContext) error {
			defer close(failed)

			return errors.New("boom")
		})),
		runbook.Effect("signal", runbook.StepFunc(func(context.Context, *runbook.StepContext) error {
			<-failed

			return runbook.ErrSkipGroup
		})),
		runbook.Effect("queued-1", ok()),
		runbook.Effect("queued-2", ok()),
	).Parallel(2)))

	require.ErrorContains(t, out.err, "p/g/broken: boom")
	assert.Equal(t, map[string]string{
		"p/g/queued-1": "skipped/group-skipped",
		"p/g/queued-2": "skipped/group-skipped",
		"p/g":          "failed",
	}, statuses(out.state, "p/g/queued-1", "p/g/queued-2", "p/g"))
}

// Replay keeps a step's signal through later skips, so resume can apply it;
// it changes only when Run finishes again.
func TestReplay_KeepsTheSignalThroughLaterSkips(t *testing.T) {
	t.Parallel()

	signalled := runbook.Event{
		Kind: runbook.EventStepFinished, Run: "r1", Path: "p/a",
		Outcome: runbook.OutcomePassed, Skip: runbook.SkipGroup,
	}

	s := replay(t,
		runStarted("r1", 1), started("r1", "p/a"), signalled,
		runStarted("r2", 2), finished("r2", "p/a", runbook.OutcomeSkipped, runbook.ReasonEarlierAttempt),
		runStarted("r3", 3), finished("r3", "p/a", runbook.OutcomeSkipped, runbook.ReasonRunSkipped),
	)
	assert.Equal(t, runbook.SkipGroup, s.Element("p/a").Skip)

	s = replay(t,
		runStarted("r1", 1), started("r1", "p/a"), signalled,
		runStarted("r2", 2), started("r2", "p/a"), finished("r2", "p/a", runbook.OutcomePassed, ""),
	)
	assert.Empty(t, s.Element("p/a").Skip, "Run finished again without signalling")
}
