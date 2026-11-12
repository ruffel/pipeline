//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package runbook_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ruffel/pipeline/runbook"
)

// writeLargeJournal writes a journal of about size bytes shaped like a long
// upgrade: for each step a start, a run marker, many lines of subprocess
// output with the odd progress event, and a finish. Output is the bulk, as
// it is in practice.
func writeLargeJournal(tb testing.TB, path string, size int64) (int, int64) {
	tb.Helper()

	f, err := os.Create(path) //nolint:gosec // A file in the test's own temp dir.
	if err != nil {
		tb.Fatal(err)
	}

	w := bufio.NewWriterSize(f, 1<<20)
	enc := json.NewEncoder(w)
	ts := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)

	var (
		steps   int
		seq     int64
		written int64
	)

	emit := func(e runbook.Event) {
		seq++
		e.V, e.Seq, e.TS, e.Run = runbook.SchemaVersion, seq, ts, "r1"

		if err := enc.Encode(e); err != nil {
			tb.Fatal(err)
		}
	}

	line := "[INFO] 2026-11-11 09:00:00,123 salt.state: Completed state [pkg.installed] at time 09:00:00.123456 " +
		strings.Repeat("=", 60)

	emit(runbook.Event{Kind: runbook.EventRunStarted, Path: "upgrade", Attempt: 1})

	for written < size {
		steps++
		path := fmt.Sprintf("upgrade/region1/DC1/node%d/apply", steps)

		emit(runbook.Event{Kind: runbook.EventStepStarted, Path: path})
		emit(runbook.Event{Kind: runbook.EventStepRunning, Path: path})

		for i := range 5000 {
			emit(runbook.Event{Kind: runbook.EventOutput, Path: path, Stream: "stdout", Line: line})

			if i%100 == 0 {
				emit(runbook.Event{Kind: runbook.EventProgress, Path: path, Key: "apply", Message: "applying", Current: i, Total: 5000})
			}
		}

		emit(runbook.Event{Kind: runbook.EventStepFinished, Path: path, Outcome: runbook.OutcomePassed, DurationMs: 1200})

		written = int64(w.Buffered())
		if info, err := f.Stat(); err == nil {
			written += info.Size()
		}
	}

	emit(runbook.Event{Kind: runbook.EventRunFinished, Path: "upgrade", Outcome: runbook.OutcomePassed})

	if err := w.Flush(); err != nil {
		tb.Fatal(err)
	}

	if err := f.Close(); err != nil {
		tb.Fatal(err)
	}

	return steps, seq
}

func BenchmarkInspect(b *testing.B) {
	const size = 256 << 20

	path := filepath.Join(b.TempDir(), "large.journal")
	steps, events := writeLargeJournal(b, path, size)

	info, err := os.Stat(path)
	if err != nil {
		b.Fatal(err)
	}

	b.SetBytes(info.Size())
	b.ReportMetric(float64(events), "events")
	b.ResetTimer()

	for b.Loop() {
		state, err := runbook.Inspect(path)
		if err != nil {
			b.Fatal(err)
		}

		if state.LastSeq != events || state.Element(fmt.Sprintf("upgrade/region1/DC1/node%d/apply", steps)).Status != runbook.StatusPassed {
			b.Fatal("replay reached the wrong state")
		}
	}
}

func BenchmarkOpenJournal(b *testing.B) {
	const size = 256 << 20

	path := filepath.Join(b.TempDir(), "large.journal")
	writeLargeJournal(b, path, size)

	info, err := os.Stat(path)
	if err != nil {
		b.Fatal(err)
	}

	b.SetBytes(info.Size())
	b.ResetTimer()

	for b.Loop() {
		j, err := runbook.OpenJournal(path)
		if err != nil {
			b.Fatal(err)
		}

		if err := j.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

// The replay target, as a property that holds on any machine: replaying a
// journal must not cost an allocation per output line, nor hold the journal
// in memory. Before replay streamed, a 256 MiB journal cost 3 GB of
// allocations and ran at 124 MB/s; it now costs about 0.5 MB and runs at
// about 2 GB/s (see BenchmarkInspect).
//
// It is not parallel: it counts the process's allocations.
//
//nolint:paralleltest // AllocsPerRun must not run beside other tests.
func TestInspect_DoesNotAllocatePerOutputLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.journal")
	steps, events := writeLargeJournal(t, path, 32<<20)

	var state *runbook.State

	allocs := testing.AllocsPerRun(1, func() {
		var err error

		state, err = runbook.Inspect(path)
		if err != nil {
			t.Fatal(err)
		}
	})

	if state.LastSeq != events {
		t.Fatalf("replayed to seq %d, want %d", state.LastSeq, events)
	}

	last := fmt.Sprintf("upgrade/region1/DC1/node%d/apply", steps)
	if got := state.Element(last); got.Status != runbook.StatusPassed || !got.RunCalled {
		t.Fatalf("%s replayed as %+v", last, got)
	}

	// A few allocations per step for its state, none per line of output.
	if limit := float64(steps*40 + 500); allocs > limit {
		t.Fatalf("Inspect made %.0f allocations for %d events in %d steps; want under %.0f", allocs, events, steps, limit)
	}
}
