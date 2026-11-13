package runbooktest

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/ruffel/pipeline/runbook"
)

// CheckIdempotent checks that a step survives a crash at any point of its
// run: that a resumed run, which calls its Check and maybe its Run again,
// still passes. That is the contract every step must keep, because a real
// process can die after Run changed the system and before the journal said
// so.
//
// It runs the step once and notes every event the step writes to the
// journal before its outcome: each message, output line, progress report,
// value, item and retry, every time it is written. Then, for each event, in
// a subtest of its own, it runs the step on a new journal, crashes just
// after that event, restarts, and resumes. The subtest fails if the run
// doesn't write the same event at that point again, or the resumed run does
// not pass. A crash lets Run carry on until its next write to the journal,
// so the crash after the last event is Run finishing with nothing recorded,
// and the resume runs it a second time if its Check doesn't find the work
// done. A step that writes many events, such as many output lines, gets as
// many subtests.
//
// The system the step works on outlives a crash, and the step does not:
// newSystem is called once for each run, with that run's test, and returns
// a fresh system, such as a new fake; newStep is called with it to build the
// step, and again to build the step the resumed run uses, as a restarted
// process would. So a step whose Check reads its own memory, not the
// system, fails the check. Checks on the system's final state belong in a
// t.Cleanup that newSystem registers: they then run once for each crash
// point. The options are [RunStep]'s, and apply to every run.
func CheckIdempotent[S any](t *testing.T, newSystem func(t *testing.T) S, newStep func(S) runbook.StepNode,
	opts ...StepOption,
) {
	t.Helper()

	cfg := newStepConfig(t, opts)
	sys := newSystem(t)

	plan, path, err := stepPlan(newStep(sys), cfg)
	if err != nil {
		t.Fatalf("runbooktest: %v", err)
	}

	clean := runOn(t, cfg, plan, path, runbook.MemoryJournal())
	if clean.Err != nil {
		t.Fatalf("runbooktest: %s fails without a crash: %v", path, clean.Err)
	}

	if !clean.Element().RunCalled {
		t.Fatalf("runbooktest: %s was not run: its Check found the work done, so there is nothing to crash", path)
	}

	for i, e := range clean.events {
		if e.Kind == runbook.EventStepFinished {
			break
		}

		n, point := i+1, label(e)

		t.Run(fmt.Sprintf("crash after event %d (%s)", n, point), func(t *testing.T) {
			cfg := newStepConfig(t, opts)
			sys := newSystem(t)

			plan, _, err := stepPlan(newStep(sys), cfg)
			if err != nil {
				t.Fatalf("runbooktest: %v", err)
			}

			crash := crashAtEvent(path, n)

			first := runOn(t, cfg, plan, path, crash.Journal)
			if !crash.Crashed() || len(first.events) < n || label(first.events[n-1]) != point {
				t.Fatalf("runbooktest: %s did not write %s as its event %d this time: %v", path, point, n, first.Err)
			}

			crash.Restart()

			// The step is built again, as a restarted process would, on the
			// same system.
			plan, _, err = stepPlan(newStep(sys), cfg)
			if err != nil {
				t.Fatalf("runbooktest: %v", err)
			}

			if resumed := runOn(t, cfg, plan, path, crash.Journal); resumed.Err != nil {
				t.Errorf("%s does not survive a crash after its event %d (%s): the resumed run failed: %v",
					path, n, point, resumed.Err)
			}
		})
	}
}

// crashAtEvent returns a memory journal that crashes just after the step at
// path writes its nth event.
func crashAtEvent(path string, n int) *Crash {
	c := &Crash{}

	var seen atomic.Int64

	c.Journal = runbook.MemoryJournal(runbook.WithWriteHook(func(e runbook.Event) error {
		if c.crashed.Load() {
			return ErrCrashed
		}

		if !c.disarmed.Load() && e.Path == path && seen.Add(1) == int64(n) {
			c.crashed.Store(true)
		}

		return nil
	}))

	return c
}

// label names an event for a subtest: its kind, and what it is about.
func label(e runbook.Event) string {
	switch e.Kind { //nolint:exhaustive // Only these kinds say what they are about.
	case runbook.EventValue, runbook.EventProgress:
		return string(e.Kind) + ":" + e.Key
	case runbook.EventItem:
		return string(e.Kind) + ":" + e.Item
	case runbook.EventOutput:
		return string(e.Kind) + ":" + e.Stream
	case runbook.EventCustom:
		return string(e.Kind) + ":" + e.Type
	default:
		return string(e.Kind)
	}
}
