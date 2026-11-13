// Command detached shows a runbook run that outlives its terminal: the run
// writes a journal, and any other terminal can watch it, approve its gate,
// stop it, or see where it stands. Stop it, or kill it, and running it
// again resumes where it left off.
//
// Try it with two terminals:
//
//	# terminal 1: start the run (nohup ... & works too)
//	go run . run
//
//	# terminal 2: watch it, approve its gate, stop it
//	go run . watch
//	go run . approve
//	go run . stop
//
//	# either terminal: where everything stands, then resume
//	go run . status
//	go run . run
//
// Add -fail node3 to the first run to see a node fail and the rest finish;
// run again without it to resume. Ctrl-C in the runner stops it gracefully,
// and a second Ctrl-C cancels the running steps. Killing a viewer never
// affects the run.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"os/user"
	"strings"
	"syscall"
	"time"

	"github.com/ruffel/pipeline/observers/terminal/runbookterm"
	"github.com/ruffel/pipeline/runbook"
)

const usage = `usage: detached <command> [flags]

commands:
  run      run the upgrade, or resume it
  watch    follow the run from another terminal
  approve  approve the gate the run is waiting at
  stop     stop the run (-cancel to cancel its running steps)
  status   show where the run stands
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	commands := map[string]func([]string) error{
		"run": run, "watch": watch, "approve": approve, "stop": stop, "status": status,
	}

	cmd, ok := commands[os.Args[1]]
	if !ok {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	if err := cmd(os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "detached %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
}

func flags(name string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	journal := fs.String("journal", "upgrade.jsonl", "the run's journal file")

	return fs, journal
}

// run runs the plan against the journal. If the journal holds an earlier
// attempt that didn't pass, it resumes: what finished is not done again.
func run(args []string) error {
	fs, journal := flags("run")
	state := fs.String("state", ".detached-state", "directory for the mock upgrade's marker files")
	fail := fs.String("fail", "", "a node whose packages fail to install, such as node3")
	_ = fs.Parse(args)

	if err := os.MkdirAll(*state, 0o700); err != nil {
		return err
	}

	plan, err := newPlan(*state, *fail)
	if err != nil {
		return err
	}

	j, err := runbook.OpenJournal(*journal)
	if errors.Is(err, runbook.ErrJournalBusy) {
		return fmt.Errorf("%s is in use by another run; follow it with: detached watch", *journal)
	}

	if err != nil {
		return err
	}

	defer func() { _ = j.Close() }()

	// The first Ctrl-C (or SIGTERM) stops the run once its running steps
	// finish, the second cancels them, and a third ends the process.
	// SIGHUP, the terminal going away, is only logged: the run carries on.
	//
	// OnSignal must not block, or the next signal waits: stderr can be a
	// full pipe. Its notices are written on another goroutine, and dropped
	// if that one is stuck.
	stopper := runbook.NewStopper()
	notices := make(chan string, 2)
	stops := 0

	go func() {
		for n := range notices {
			fmt.Fprintln(os.Stderr, n)
		}
	}()

	release := runbook.HandleSignals(stopper, runbook.SignalOptions{OnSignal: func(s os.Signal) {
		if s == syscall.SIGHUP {
			return
		}

		stops++

		notice := "\ncancelling the running steps"
		if stops == 1 {
			notice = "\nstopping once the running steps finish; press Ctrl-C again to cancel them"
		}

		select {
		case notices <- notice:
		default:
		}
	}})

	defer close(notices)
	defer release()

	// A terminal can block, or vanish; behind an AsyncObserver it can't
	// hold up the run. When the gate is answered at this terminal, though,
	// the view is called directly, so the gate's prompt is on screen before
	// StdinApprover asks.
	var view runbook.Observer = runbookterm.New(os.Stdout)

	var async *runbook.AsyncObserver
	if !interactive() {
		async = runbook.NewAsyncObserver(view, runbook.DefaultChatterLimit)
		view = async
	}

	_, err = runbook.Run(context.Background(), plan, j,
		runbook.WithObservers(view), runbook.WithStopper(stopper), runbook.WithRunLabels(runbook.L{"operator": who()}))

	if async != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_ = async.Close(ctx)
	}

	if errors.Is(err, runbook.ErrAlreadyComplete) {
		return fmt.Errorf("%s holds a finished upgrade; remove it and %s to start again", *journal, *state)
	}

	return err
}

// watch follows the run from any terminal. It replays where everything
// stands, then shows events as the run writes them. Ctrl-C ends it, and
// never the run.
func watch(args []string) error {
	fs, journal := flags("watch")
	all := fs.Bool("all", false, "replay all past output, not just where everything stands")
	keep := fs.Bool("keep", false, "keep following when the run ends, for the next attempt")
	_ = fs.Parse(args)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	history := runbook.HistoryLifecycle
	if *all {
		history = runbook.HistoryAll
	}

	err := runbook.Follow(ctx, *journal, runbook.FollowOptions{History: history, KeepFollowing: *keep},
		runbookterm.New(os.Stdout))

	switch {
	case errors.Is(err, runbook.ErrRunnerGone):
		return errors.New("the run ended without finishing, and nothing is running it; resume it with: detached run")
	case ctx.Err() != nil:
		return nil
	default:
		return err
	}
}

// approve releases the gate the run is waiting at, from any terminal.
func approve(args []string) error {
	fs, journal := flags("approve")
	gate := fs.String("gate", "upgrade/approve", "the gate's path")
	_ = fs.Parse(args)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := runbook.Approve(ctx, *journal, *gate, who())
	if errors.Is(err, runbook.ErrGateNotWaiting) {
		return fmt.Errorf("%s is not waiting for approval", *gate)
	}

	return err
}

// stop asks the run to stop, from any terminal, and waits until it has
// seen the request.
func stop(args []string) error {
	fs, journal := flags("stop")
	cancelSteps := fs.Bool("cancel", false, "cancel the running steps instead of letting them finish")
	_ = fs.Parse(args)

	mode := runbook.StopGraceful
	if *cancelSteps {
		mode = runbook.StopCancel
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	return runbook.RequestStop(ctx, *journal, mode, who())
}

// status shows where the run stands, from the journal alone.
func status(args []string) error {
	fs, journal := flags("status")
	_ = fs.Parse(args)

	st, err := runbook.Inspect(*journal)
	if err != nil {
		return err
	}

	if len(st.Attempts) == 0 || st.Plan == nil {
		fmt.Println("no run yet")

		return nil
	}

	last := st.Attempts[len(st.Attempts)-1]

	outcome := string(last.Outcome)

	switch {
	case outcome != "":
	case st.Live:
		outcome = "running"
	default:
		outcome = "interrupted; resume it with: detached run"
	}

	fmt.Printf("attempt %d: %s\n\n", last.Attempt, outcome)

	var show func(n runbook.NodeSpec)

	show = func(n runbook.NodeSpec) {
		for _, c := range n.Children {
			el := st.Element(c.Path)

			why := ""
			if el.Reason != "" {
				why = " (" + string(el.Reason) + ")"
			}

			fmt.Printf("  %-12s %s%s%s\n", el.Status, strings.Repeat("  ", strings.Count(c.Path, "/")-1), c.Name, why)
			show(c)
		}
	}

	show(*st.Plan)

	return nil
}

// who names the operator in the journal.
func who() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}

	return ""
}
