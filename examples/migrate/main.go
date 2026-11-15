// Command migrate applies schema migrations to a mock database. It shows a
// tool built to be resumed and rolled back: every step asks the database
// whether its work is done, and says how to undo it.
//
// Apply the migrations, then undo them:
//
//	go run . plan
//	go run . run
//	go run . status
//	go run . rollback
//
// See a failure, and recover from it either way:
//
//	go run . run -fail 002-add-email-index
//	go run . run        # resumes: 001 is checked, not applied again
//	go run . rollback   # or undoes what was done, in reverse
//
// Stop it part-way with Ctrl-C, or kill it with kill -9 from another
// terminal, and run it again: it carries on from the journal.
//
//	go run . run -pace 5s
//
// A stopped or killed run leaves the database in maintenance mode, as status
// shows: only a failure runs the schema group's handler, which turns it off.
//
// To start again, remove the journal and the state directory.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ruffel/pipeline"
)

const usage = `usage: migrate <command> [flags]

commands:
  plan      print the plan, and what each step can check and undo
  run       apply the migrations, or carry on applying them
  rollback  undo what the journal says was done
  status    show the attempts so far, each step, and the database
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	commands := map[string]func([]string) error{
		"plan": plan, "run": run, "rollback": rollback, "status": status,
	}

	command, ok := commands[os.Args[1]]
	if !ok {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	if err := command(os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		os.Exit(1)
	}
}

// options are the flags every command takes.
type options struct {
	journal  string
	state    string
	to       int
	password string
}

func flags(name string) (*flag.FlagSet, *options) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	o := &options{}

	fs.StringVar(&o.journal, "journal", "migrate.jsonl", "the migration's journal")
	fs.StringVar(&o.state, "state", ".migrate-state", "directory for the mock database")
	fs.IntVar(&o.to, "to", 0, "the schema version to migrate to (default: the latest, or what the journal says)")
	fs.StringVar(&o.password, "password", "correct-horse-battery", "the database password; never written to the journal")

	return fs, o
}

// plan prints the plan: every step that may run, and whether it has a Check
// and an Undo, before anything changes.
func plan(args []string) error {
	fs, o := flags("plan")
	_ = fs.Parse(args)

	p, _, err := build(o, faults{})
	if err != nil {
		return err
	}

	fmt.Print(pipeline.Describe(p))

	return nil
}

// run applies the migrations. On a journal that already has attempts it
// resumes: the same call, doing only what is left.
func run(args []string) error {
	fs, o := flags("run")
	fail := fs.String("fail", "", "a migration whose Run fails, such as 002-add-email-index")
	pace := fs.Duration("pace", 400*time.Millisecond, "how long each migration takes")
	_ = fs.Parse(args)

	res, err := attempt(o, faults{fail: *fail, pace: *pace}, pipeline.Run)

	switch {
	case err == nil:
		return nil
	case errors.Is(err, pipeline.ErrAlreadyComplete):
		return fmt.Errorf("%s holds a finished migration; roll it back, or remove it and %s to start again", o.journal, o.state)
	case errors.Is(err, pipeline.ErrRollbackUnfinished):
		return errors.New("a rollback has not finished; run rollback again first")
	case errors.Is(err, pipeline.ErrInputsChanged):
		return fmt.Errorf("%s is a migration of another database; use its -state", o.journal)
	case res != nil && res.Outcome != pipeline.OutcomeFailed:
		return errors.New("stopped; run again to carry on, or roll back")
	case res != nil:
		return errors.New("failed; fix the cause and run again to carry on, or roll back")
	default:
		return err
	}
}

// rollback undoes what the journal's attempts did, by calling each step's
// Undo in reverse plan order. If an Undo fails, calling it again carries on
// from that step.
func rollback(args []string) error {
	fs, o := flags("rollback")
	_ = fs.Parse(args)

	res, err := attempt(o, faults{}, pipeline.Rollback)
	if err != nil && res != nil {
		return errors.New("the rollback did not finish; run rollback again to carry on")
	}

	return err
}

// attempt makes one attempt on the journal: do is pipeline.Run or
// pipeline.Rollback, which take the same plan, journal and options.
func attempt(
	o *options, f faults,
	do func(context.Context, *pipeline.Plan, *pipeline.Journal, ...pipeline.Option) (*pipeline.Result, error),
) (*pipeline.Result, error) {
	if err := os.MkdirAll(o.state, 0o700); err != nil {
		return nil, err
	}

	p, in, err := build(o, f)
	if err != nil {
		return nil, err
	}

	// One writer at a time: a second migrate on this journal fails here,
	// with an error that names the process holding it.
	j, err := pipeline.OpenJournal(o.journal)
	if err != nil {
		return nil, err
	}

	defer func() { _ = j.Close() }()

	// Ctrl-C lets the running step finish and starts nothing new; a second
	// Ctrl-C cancels the running step.
	stopper := pipeline.NewStopper()

	release := pipeline.HandleSignals(stopper, pipeline.SignalOptions{})
	defer release()

	return do(context.Background(), p, j,
		pipeline.WithInputs(in),
		pipeline.WithStopper(stopper),
		pipeline.WithObservers(pipeline.NewPlainObserver(os.Stdout)))
}

// build works out the inputs and builds the plan from them.
//
// A journal with attempts already says what was started, so a resume or a
// rollback needs no flags: the target is read back from the journal, and the
// plan comes out the same shape. Only the password has to be given again.
func build(o *options, f faults) (*pipeline.Plan, inputs, error) {
	file, err := filepath.Abs(filepath.Join(o.state, "database.json"))
	if err != nil {
		return nil, inputs{}, err
	}

	in := inputs{Database: file, Target: o.to, Password: pipeline.NewSecret(o.password)}

	st, err := pipeline.Inspect(o.journal)
	if err != nil {
		return nil, in, err
	}

	var started inputs

	found, err := st.DecodeInputs(&started)
	if err != nil {
		return nil, in, err
	}

	switch {
	case found && in.Target != 0 && in.Target != started.Target:
		return nil, in, fmt.Errorf("%s is a migration to version %d; remove it and %s to start another",
			o.journal, started.Target, o.state)
	case found:
		in.Target = started.Target
	case in.Target == 0:
		in.Target = len(migrations)
	}

	if in.Target < 1 || in.Target > len(migrations) {
		return nil, in, fmt.Errorf("-to must be between 1 and %d", len(migrations))
	}

	p, err := newPlan(&database{file: file}, in, f)

	return p, in, err
}

// status shows the attempts so far and where each step stands, from the
// journal alone, and what the database itself says. It works while a run is
// in progress.
func status(args []string) error {
	fs, o := flags("status")
	_ = fs.Parse(args)

	st, err := pipeline.Inspect(o.journal)
	if err != nil {
		return err
	}

	if len(st.Attempts) == 0 {
		fmt.Println("no migration started")

		return nil
	}

	for _, a := range st.Attempts {
		what := "run"
		if a.Rollback {
			what = "rollback"
		}

		outcome := string(a.Outcome)

		switch {
		case outcome != "":
		case st.Live:
			outcome = "in progress"
		default:
			outcome = "interrupted"
		}

		fmt.Printf("attempt %d  %-8s  %s\n", a.Attempt, what, outcome)
	}

	last := st.Attempts[len(st.Attempts)-1]

	switch {
	case st.Live:
		fmt.Println("\na migrate is working on it now")
	case last.Rollback && last.Outcome == pipeline.OutcomePassed:
		fmt.Println("\nrolled back; run applies the migrations again")
	case last.Rollback:
		fmt.Println("\nthe rollback did not finish; run rollback again")
	case last.Outcome == pipeline.OutcomePassed:
		fmt.Println("\nfinished; rollback undoes it")
	default:
		fmt.Println("\nunfinished; run carries on, rollback undoes it")
	}

	fmt.Println()

	if st.Plan != nil {
		show(st, *st.Plan)
	}

	db := database{file: filepath.Join(o.state, "database.json")}

	s, err := db.load()
	if err != nil {
		return err
	}

	applied := "none"
	if len(s.Applied) > 0 {
		applied = strings.Join(s.Applied, ", ")
	}

	fmt.Printf("\ndatabase: version %d (%s), maintenance mode %v\n", len(s.Applied), applied, s.Maintenance)

	return nil
}

// show prints where everything beneath n stands. A step a rollback undid is
// back to not-reached, and one whose Undo failed says so.
func show(st *pipeline.State, n pipeline.NodeSpec) {
	for _, c := range n.Children {
		el := st.Element(c.Path)

		note := ""

		switch {
		case el.UndoError != nil:
			note = " (undo failed: " + el.UndoError.Message + ")"
		case el.Error != nil && c.Kind != pipeline.KindGroup:
			note = " (" + el.Error.Message + ")"
		case el.Reason != "":
			note = " (" + string(el.Reason) + ")"
		}

		indent := strings.Repeat("  ", strings.Count(c.Path, "/")-1)

		fmt.Printf("  %-12s%s%s%s\n", el.Status, indent, c.Name, note)
		show(st, c)
	}
}
