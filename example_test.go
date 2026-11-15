package pipeline_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/ruffel/pipeline"
	"github.com/ruffel/pipeline/pipelinetest"
)

// shown prints a run as plain text. The clock stands still, so no step takes
// any time and the output has no durations in it.
func shown() []pipeline.Option {
	return []pipeline.Option{
		pipeline.WithObservers(pipeline.NewPlainObserver(os.Stdout)),
		pipeline.WithClock(pipelinetest.NewFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))),
	}
}

// A plan is a list of named steps, and the simplest step is a function.
func Example() {
	greet := func(_ context.Context, sc *pipeline.StepContext) error {
		sc.Info("hello")

		return nil
	}

	plan, err := pipeline.NewPlan("first",
		pipeline.Effect("greet", pipeline.StepFunc(greet)),
		pipeline.Observe("look", pipeline.StepFunc(greet)),
	)
	if err != nil {
		fmt.Println(err)

		return
	}

	_, err = pipeline.Run(context.Background(), plan, pipeline.MemoryJournal(), shown()...)
	fmt.Println("error:", err)

	// Output:
	// ▶ first
	//     │ greet: [INFO] hello
	//   ✓ greet
	//     │ look: [INFO] hello
	//   ✓ look
	// ✓ first passed
	// error: <nil>
}

// Calling Run again on the same journal starts a new attempt, which does
// only what is left.
func ExampleRun_resume() {
	upload := func(_ context.Context, sc *pipeline.StepContext) error {
		if sc.Attempt() == 1 {
			return errors.New("connection refused")
		}

		return nil
	}

	plan, err := pipeline.NewPlan("release",
		pipeline.Effect("build", pipeline.StepFunc(nothing)),
		pipeline.Effect("upload", pipeline.StepFunc(upload)),
	)
	if err != nil {
		fmt.Println(err)

		return
	}

	// A file journal from OpenJournal outlives the process. This one doesn't,
	// but it resumes in the same way.
	journal := pipeline.MemoryJournal()

	_, err = pipeline.Run(context.Background(), plan, journal, shown()...)
	fmt.Println("first attempt:", err)

	_, err = pipeline.Run(context.Background(), plan, journal, shown()...)
	fmt.Println("second attempt:", err)

	_, err = pipeline.Run(context.Background(), plan, journal, shown()...)
	fmt.Println("third attempt:", errors.Is(err, pipeline.ErrAlreadyComplete))

	// Output:
	// ▶ release
	//   ✓ build
	//   ✗ upload — connection refused
	// ✗ release failed — release/upload: connection refused
	// first attempt: release/upload: connection refused
	// ▶ release
	//   ⊘ build — earlier-attempt
	//   ✓ upload
	// ✓ release passed
	// second attempt: <nil>
	// third attempt: true
}

// A step with a Check is asked whether its work is already done before its
// Run is called, on a first attempt too.
func ExampleChecker() {
	disk := map[string]string{"style.css": "h1 { color: teal }"}

	plan, err := pipeline.NewPlan("publish",
		pipeline.Effect("style.css", file{disk, "style.css", "h1 { color: teal }"}),
		pipeline.Effect("index.html", file{disk, "index.html", "<h1>hello</h1>"}),
	)
	if err != nil {
		fmt.Println(err)

		return
	}

	_, err = pipeline.Run(context.Background(), plan, pipeline.MemoryJournal(), shown()...)
	fmt.Println("error:", err)

	// Output:
	// ▶ publish
	//   ⊘ style.css — done
	//     │ index.html: [INFO] writing index.html
	//   ✓ index.html
	// ✓ publish passed
	// error: <nil>
}

// Rollback undoes the steps whose Run was called, last one first. A step
// that Check found already done was never run, so it is left alone.
func ExampleRollback() {
	disk := map[string]string{"style.css": "h1 { color: teal }"}

	plan, err := pipeline.NewPlan("publish",
		pipeline.Effect("style.css", file{disk, "style.css", "h1 { color: teal }"}),
		pipeline.Effect("script.js", file{disk, "script.js", "console.log('hello')"}),
		pipeline.Effect("index.html", file{disk, "index.html", "<h1>hello</h1>"}),
	)
	if err != nil {
		fmt.Println(err)

		return
	}

	journal := pipeline.MemoryJournal()

	if _, err := pipeline.Run(context.Background(), plan, journal); err != nil {
		fmt.Println(err)

		return
	}

	_, err = pipeline.Rollback(context.Background(), plan, journal, shown()...)
	fmt.Println("error:", err)
	fmt.Println("files left:", len(disk))

	// Output:
	// ▶ publish (rollback)
	//     │ index.html: [INFO] removing index.html
	//   ↶ index.html undone
	//     │ script.js: [INFO] removing script.js
	//   ↶ script.js undone
	// ✓ publish rolled back
	// error: <nil>
	// files left: 1
}

// Record stores a value in the journal, and Recorded reads it by the path of
// the step that stored it.
func ExampleStepContext_Record() {
	backup := func(_ context.Context, sc *pipeline.StepContext) error {
		return sc.Record("archive", "/var/backups/upgrade.tar")
	}

	restore := func(_ context.Context, sc *pipeline.StepContext) error {
		var archive string

		found, err := sc.Recorded("upgrade/backup", "archive", &archive)
		if err != nil || !found {
			return fmt.Errorf("no backup was recorded: %w", err)
		}

		sc.Info("restoring from " + archive)

		return nil
	}

	plan, err := pipeline.NewPlan("upgrade",
		pipeline.Effect("backup", pipeline.StepFunc(backup)),
		pipeline.Effect("restore", pipeline.StepFunc(restore)),
	)
	if err != nil {
		fmt.Println(err)

		return
	}

	_, err = pipeline.Run(context.Background(), plan, pipeline.MemoryJournal(), shown()...)
	fmt.Println("error:", err)

	// Output:
	// ▶ upgrade
	//   ✓ backup
	//     │ restore: [INFO] restoring from /var/backups/upgrade.tar
	//   ✓ restore
	// ✓ upgrade passed
	// error: <nil>
}

// Describe prints a plan before anything runs: its groups, each step's kind,
// and what each step can check and undo.
func ExampleDescribe() {
	node := func(name string) pipeline.GroupNode {
		return pipeline.Group(name,
			pipeline.Effect("drain", pipeline.StepFunc(nothing)),
			pipeline.Effect("packages", pipeline.StepFunc(nothing)).Retry(3, 10*time.Second),
		).RecoverOnFailure(pipeline.Effect("restore", pipeline.StepFunc(nothing)))
	}

	plan, err := pipeline.NewPlan("upgrade",
		pipeline.Group("prechecks",
			pipeline.Observe("disk-space", pipeline.StepFunc(nothing)),
			pipeline.Observe("ring-healthy", pipeline.StepFunc(nothing)),
		).Parallel(2),
		pipeline.Effect("backup", file{nil, "backup.tar", ""}),
		node("node1"),
		pipeline.Gate("continue", pipeline.GateOptions{Prompt: "Upgrade the rest?"}),
		pipeline.Group("rest", node("node2"), node("node3")).Parallel(2),
	)
	if err != nil {
		fmt.Println(err)

		return
	}

	fmt.Print(pipeline.Describe(plan))

	// Output:
	// upgrade
	// ├── prechecks (parallel, limit 2)
	// │   ├── disk-space (observe)
	// │   └── ring-healthy (observe)
	// ├── backup (effect, check, undo)
	// ├── node1
	// │   ├── drain (effect)
	// │   ├── packages (effect, 3 tries)
	// │   └── restore (effect, recovers the group on failure)
	// ├── continue (gate)
	// └── rest (parallel, limit 2)
	//     ├── node2
	//     │   ├── drain (effect)
	//     │   ├── packages (effect, 3 tries)
	//     │   └── restore (effect, recovers the group on failure)
	//     └── node3
	//         ├── drain (effect)
	//         ├── packages (effect, 3 tries)
	//         └── restore (effect, recovers the group on failure)
}

// A gate waits for one of its approvers before the run goes on.
func ExampleGate() {
	plan, err := pipeline.NewPlan("upgrade",
		pipeline.Effect("canary", pipeline.StepFunc(nothing)),
		pipeline.Gate("continue", pipeline.GateOptions{
			Prompt:    "The canary is upgraded. Upgrade the rest?",
			Approvers: []pipeline.Approver{always("ada")},
		}),
		pipeline.Effect("rest", pipeline.StepFunc(nothing)),
	)
	if err != nil {
		fmt.Println(err)

		return
	}

	_, err = pipeline.Run(context.Background(), plan, pipeline.MemoryJournal(), shown()...)
	fmt.Println("error:", err)

	// Output:
	// ▶ upgrade
	//   ✓ canary
	//   ⏸ continue — The canary is upgraded. Upgrade the rest?
	//   ▶ continue approved by ada (example)
	//   ✓ continue
	//   ✓ rest
	// ✓ upgrade passed
	// error: <nil>
}

// A group's failure handler runs when the group fails. RecoverOnFailure
// resets the group, so the next attempt runs it from the start.
func ExampleGroupNode_RecoverOnFailure() {
	packages := func(_ context.Context, sc *pipeline.StepContext) error {
		if sc.Attempt() == 1 {
			return errors.New("dpkg was interrupted")
		}

		return nil
	}

	plan, err := pipeline.NewPlan("upgrade",
		pipeline.Group("node1",
			pipeline.Effect("drain", pipeline.StepFunc(nothing)),
			pipeline.Effect("packages", pipeline.StepFunc(packages)),
		).RecoverOnFailure(pipeline.Effect("restore", pipeline.StepFunc(nothing))),
	)
	if err != nil {
		fmt.Println(err)

		return
	}

	journal := pipeline.MemoryJournal()

	_, _ = pipeline.Run(context.Background(), plan, journal, shown()...)
	_, err = pipeline.Run(context.Background(), plan, journal, shown()...)
	fmt.Println("error:", err)

	// Output:
	// ▶ upgrade
	//   ▶ node1
	//     ✓ drain
	//     ✗ packages — dpkg was interrupted
	//   ↺ node1 reset: a resume runs it again from the start
	//     ✓ restore
	//   ✗ node1 — upgrade/node1/packages: dpkg was interrupted
	// ✗ upgrade failed — upgrade/node1/packages: dpkg was interrupted
	// ▶ upgrade
	//   ▶ node1
	//     ✓ drain
	//     ✓ packages
	//   ✓ node1
	// ✓ upgrade passed
	// error: <nil>
}

// file is a step that writes one file, to a map standing in for a disk.
type file struct {
	disk          map[string]string
	path, content string
}

func (f file) Run(_ context.Context, sc *pipeline.StepContext) error {
	sc.Info("writing " + f.path)

	f.disk[f.path] = f.content

	return nil
}

// Check reports whether the file is already as it should be.
func (f file) Check(context.Context, *pipeline.StepContext) (bool, error) {
	return f.disk[f.path] == f.content, nil
}

// Undo removes the file. Removing one already gone is not an error, so Undo
// can be called twice.
func (f file) Undo(_ context.Context, sc *pipeline.StepContext) error {
	sc.Info("removing " + f.path)

	delete(f.disk, f.path)

	return nil
}

// nothing stands in for real work.
func nothing(context.Context, *pipeline.StepContext) error { return nil }

// always is an approver that approves at once, in the given name.
type always string

func (a always) Name() string { return "example" }

func (a always) Wait(context.Context, pipeline.GateInfo) (string, error) {
	return string(a), nil
}
