package runbooktest

import (
	"slices"
	"testing"

	"github.com/ruffel/pipeline/runbook"
)

// CheckIdempotent checks that a step survives a crash at any point of its
// run: that a resumed run, which calls its Check and maybe its Run again,
// still passes. That is the contract every step must keep, because a real
// process can die after Run changed the system and before the journal said
// so.
//
// It runs the step once to find the points where it writes to the journal:
// just after it starts running, and just after each value and item it
// records. Then, for each point, in a subtest of its own, it runs the step
// on a new journal, crashes just after the point, restarts, and resumes. The
// subtest fails if the crash point is not reached or the resumed run does
// not pass. A crash lets Run carry on until its next write to the journal,
// so the crash after the last point is Run finishing with nothing recorded,
// and the resume runs it a second time if its Check doesn't find the work
// done.
//
// newStep is called for each run with that run's test, and must return the
// step with a fresh system to work on, such as a new fake. Checks on the
// system's final state belong in a t.Cleanup it registers: they then run
// once for each crash point. The options are [RunStep]'s, and apply to
// every run.
func CheckIdempotent(t *testing.T, newStep func(t *testing.T) runbook.StepNode, opts ...StepOption) {
	t.Helper()

	cfg := newStepConfig(t, opts)

	plan, path, err := stepPlan(newStep(t), cfg)
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

	for _, point := range points(clean) {
		t.Run("crash after "+string(point), func(t *testing.T) {
			cfg := newStepConfig(t, opts)

			plan, _, err := stepPlan(newStep(t), cfg)
			if err != nil {
				t.Fatalf("runbooktest: %v", err)
			}

			crash := CrashAfter(path, point)

			first := runOn(t, cfg, plan, path, crash.Journal)
			if !crash.Crashed() {
				t.Fatalf("runbooktest: %s did not reach %s this time: %v", path, point, first.Err)
			}

			crash.Restart()

			if resumed := runOn(t, cfg, plan, path, crash.Journal); resumed.Err != nil {
				t.Errorf("%s does not survive a crash after %s: the resumed run failed: %v", path, point, resumed.Err)
			}
		})
	}
}

// points returns where a run of the step wrote to the journal, each once,
// in order: Running, if it ran, then each value and item.
func points(r *StepRun) []Point {
	var out []Point

	for _, e := range r.events {
		var p Point

		switch e.Kind { //nolint:exhaustive // Only the events a crash point can follow.
		case runbook.EventStepRunning:
			p = Running
		case runbook.EventValue:
			p = Value(e.Key)
		case runbook.EventItem:
			p = Item(e.Item)
		default:
			continue
		}

		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}

	return out
}
