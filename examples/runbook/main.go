// Command runbook is an operator-facing runbook for a mock database failover:
// a description on every entry, approval gates, steps that ask the system
// whether their work is done, and a journal that is both the audit log and
// what a second run resumes from.
//
// Run it, answer its gates, and run it again to see a finished journal
// refused:
//
//	go run .
//
// Add -fail update-dns to see a step fail. Run again without it, and the run
// resumes: the steps that finished are checked, not done again. Ctrl-C at a
// gate says no: the run stops, and running again waits at that gate. -yes
// approves every gate, and -format picks the view: terminal, plain or json.
//
// To start again, remove the journal and the state directory.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"

	"github.com/ruffel/pipeline/observers/terminal/runbookterm"
	"github.com/ruffel/pipeline/runbook"
)

func main() {
	yes := flag.Bool("yes", false, "approve every gate")
	format := flag.String("format", "terminal", "how to show the run: terminal, plain or json")
	journal := flag.String("journal", "runbook.jsonl", "the run's journal, which is also its audit log")
	state := flag.String("state", ".runbook-state", "directory for the mock cluster's state")
	fail := flag.String("fail", "", "a failover step whose Run fails, such as update-dns")
	flag.Parse()

	if err := run(*yes, *format, *journal, *state, *fail); err != nil {
		fmt.Fprintf(os.Stderr, "runbook: %v\n", err)

		os.Exit(1)
	}
}

func run(yes bool, format, journal, state, fail string) error {
	if err := os.MkdirAll(state, 0o700); err != nil {
		return err
	}

	view, err := observer(format, os.Stdout)
	if err != nil {
		return err
	}

	plan, err := newPlan(&cluster{file: state + "/cluster.json"}, approvers(yes), fail)
	if err != nil {
		return err
	}

	j, err := runbook.OpenJournal(journal)
	if err != nil {
		return err
	}

	defer func() { _ = j.Close() }()

	// Ctrl-C stops the run once its running steps finish, and at a gate
	// stops it without approving; a second Ctrl-C cancels the running steps.
	stopper := runbook.NewStopper()

	release := runbook.HandleSignals(stopper, runbook.SignalOptions{})
	defer release()

	_, err = runbook.Run(context.Background(), plan, j,
		runbook.WithObservers(view), runbook.WithStopper(stopper), runbook.WithRunLabels(runbook.L{"operator": who()}))

	switch {
	case errors.Is(err, runbook.ErrAlreadyComplete):
		return fmt.Errorf("%s holds a finished failover; remove it and %s to start again", journal, state)
	case errors.Is(err, runbook.ErrStopped):
		return errors.New("stopped; run again to resume")
	default:
		return err
	}
}

// observer shows the run. It is called on the run's goroutine, so the view
// and a gate's question on the terminal stay in order.
func observer(format string, w io.Writer) (runbook.Observer, error) {
	switch format {
	case "terminal":
		return runbookterm.New(w), nil
	case "plain":
		return runbook.NewPlainObserver(w), nil
	case "json":
		return runbook.NewJSONObserver(w), nil
	default:
		return nil, fmt.Errorf("unknown format %q: use terminal, plain or json", format)
	}
}

// who names the operator in the journal.
func who() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}

	return ""
}
