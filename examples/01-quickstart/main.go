// Command quickstart is the smallest tool that can be resumed and rolled
// back: three steps that each write a file, one that reads the result, and
// the run, rollback and status commands around them. It is the program the
// README builds up, a step at a time.
//
//	go run . plan
//	go run . run
//	go run . status
//	go run . rollback
//
// See a failure, then carry on or undo it:
//
//	go run . run -fail index.html
//	go run . run        # resumes: style.css and script.js are found done
//	go run . rollback   # removes the files, last one first
//
// Each step asks the disk whether its work is done, not only the journal:
// remove style.css after the failed run, and the next run writes it again.
//
// To start again, roll back and remove site.jsonl.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"

	"github.com/ruffel/pipeline"
	"github.com/ruffel/pipeline/observers/terminal"
)

// journal is the record of the operation: what resume, rollback and status
// all read.
const journal = "site.jsonl"

const usage = `usage: quickstart <command> [flags]

commands:
  plan      print the plan, and what each step can check and undo
  run       publish the site, or carry on publishing it
  rollback  undo what the journal says was done
  status    show the attempts so far and where each step stands
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	flags := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	fail := flags.String("fail", "", "a step whose Run fails, such as index.html")
	_ = flags.Parse(os.Args[2:])

	plan, err := newPlan(*fail)
	if err != nil {
		fmt.Fprintf(os.Stderr, "quickstart: %v\n", err)
		os.Exit(1)
	}

	ctx := context.Background()
	view := terminal.New(os.Stdout)

	switch os.Args[1] {
	case "plan":
		fmt.Print(pipeline.Describe(plan))
	case "run":
		err = run(ctx, plan, view)
	case "rollback":
		err = rollback(ctx, plan, view)
	case "status":
		err = status()
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "quickstart: %v\n", err)
		os.Exit(1)
	}
}

// newPlan is the whole operation. Every command builds it the same way,
// because a resume and a rollback need the plan the run had.
func newPlan(fail string) (*pipeline.Plan, error) {
	write := func(path, content string) pipeline.Element {
		f := file{path, content}
		if path == fail {
			return pipeline.Effect(path, broken{f})
		}

		return pipeline.Effect(path, f)
	}

	return pipeline.NewPlan("publish",
		write("style.css", "h1 { color: teal }\n"),
		write("script.js", "console.log('hello')\n"),
		write("index.html", "<h1>hello</h1>\n"),
		pipeline.Observe("verify", pipeline.StepFunc(verify)),
	)
}

// file is a step that writes one file.
type file struct{ path, content string }

func (f file) Run(_ context.Context, sc *pipeline.StepContext) error {
	sc.Info("writing " + f.path)

	return os.WriteFile(f.path, []byte(f.content), 0o600)
}

// Check reports whether the file is already as it should be. It runs before
// every Run, so a file removed or edited between attempts is written again.
func (f file) Check(context.Context, *pipeline.StepContext) (bool, error) {
	got, err := os.ReadFile(f.path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}

	return string(got) == f.content, err
}

// Undo removes the file. It can be called twice, or after a Run that died
// before it wrote anything, so a file already gone is not an error.
func (f file) Undo(_ context.Context, sc *pipeline.StepContext) error {
	sc.Info("removing " + f.path)

	err := os.Remove(f.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	return err
}

// broken is a file step whose Run fails, for -fail. Check and Undo are found
// on the value the plan is given, and embedding the file passes them on.
type broken struct{ file }

func (broken) Run(context.Context, *pipeline.StepContext) error {
	return errors.New("disk full (-fail)")
}

// verify reads the page back. It changes nothing, so it is an Observe step,
// and every attempt runs it again.
func verify(_ context.Context, sc *pipeline.StepContext) error {
	page, err := os.ReadFile("index.html")
	if err != nil {
		return err
	}

	sc.Info(fmt.Sprintf("index.html is %d bytes", len(page)))

	return nil
}

// run publishes the site. On a journal that already has attempts it resumes:
// the same call, doing only what is left.
func run(ctx context.Context, plan *pipeline.Plan, view pipeline.Observer) error {
	j, err := pipeline.OpenJournal(journal)
	if err != nil {
		return err // ErrJournalBusy: another process is running it
	}

	defer func() { _ = j.Close() }()

	// Ctrl-C stops the run once its running step finishes.
	stopper := pipeline.NewStopper()
	defer pipeline.HandleSignals(stopper, pipeline.SignalOptions{})()

	res, err := pipeline.Run(ctx, plan, j,
		pipeline.WithObservers(view), pipeline.WithStopper(stopper))

	switch {
	case err == nil:
		return nil
	case errors.Is(err, pipeline.ErrAlreadyComplete):
		return errors.New("already published: roll it back, or use a new journal to publish again")
	case errors.Is(err, pipeline.ErrRollbackUnfinished):
		return errors.New("a rollback has not finished: run rollback again")
	case errors.Is(err, pipeline.ErrStopped):
		return errors.New("stopped: run again to resume")
	case res != nil:
		return errors.New("failed: fix the cause and run again to resume, or roll back")
	default:
		return err // refused before anything ran: a changed plan, or a busy journal
	}
}

// rollback undoes what the journal says was done, by calling each step's
// Undo in reverse plan order. If an Undo fails, rollback again carries on
// from it.
func rollback(ctx context.Context, plan *pipeline.Plan, view pipeline.Observer) error {
	j, err := pipeline.OpenJournal(journal)
	if err != nil {
		return err
	}

	defer func() { _ = j.Close() }()

	_, err = pipeline.Rollback(ctx, plan, j, pipeline.WithObservers(view))

	return err
}

// status shows the attempts so far, where each step stands and what to do
// next, from the journal alone. It works while a run is in progress.
func status() error {
	st, err := pipeline.Inspect(journal)
	if err != nil {
		return err
	}

	if len(st.Attempts) == 0 {
		fmt.Println("nothing started: run publishes the site")

		return nil
	}

	last := len(st.Attempts) - 1

	for i, a := range st.Attempts {
		what := "run"
		if a.Rollback {
			what = "rollback"
		}

		// An attempt with no outcome is still running, or its process died.
		outcome := string(a.Outcome)

		switch {
		case outcome != "":
		case st.Live && i == last:
			outcome = "running"
		default:
			outcome = "interrupted"
		}

		fmt.Printf("attempt %d  %-8s  %s\n", a.Attempt, what, outcome)
	}

	fmt.Println()

	for _, step := range st.Plan.Children {
		fmt.Printf("  %-11s  %s\n", st.Element(step.Path).Status, step.Name)
	}

	fmt.Println()

	switch latest := st.Attempts[last]; {
	case st.Live:
		fmt.Println("running now")
	case latest.Rollback && latest.Outcome == pipeline.OutcomePassed:
		fmt.Println("rolled back: run publishes it again")
	case latest.Rollback:
		fmt.Println("the rollback did not finish: run rollback again")
	case latest.Outcome == pipeline.OutcomePassed:
		fmt.Println("published: rollback undoes it")
	default:
		fmt.Println("unfinished: run carries on, rollback undoes it")
	}

	return nil
}
