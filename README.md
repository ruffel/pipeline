# pipeline

Durable, resumable pipelines for Go.

Plan it, run it, kill it, resume it, roll it back, and watch it from another
terminal, all recorded in one journal file, with no server. It is for tools
that operators run against real systems: installers, upgrades, migrations,
failovers, data repairs.

![A run that fails, is resumed, and is then rolled back](./.vhs/demo.gif)

The recording is [examples/01-quickstart](./examples/01-quickstart): a run fails
part-way, the next run carries on from the failure, a rollback undoes all of
it, and the journal remembers each attempt.

It sits between a script and a workflow engine:

- Scripts and `errgroup` keep no record, so they can't resume after a failure
  or roll back.
- Workflow engines such as Temporal are durable, but need a server or a
  database.
- pipeline is a library: your tool links it, and the record is a file.

[DESIGN.md](./DESIGN.md) says why it works the way it does, and what it
deliberately leaves out.

This page builds the program in the recording a step at a time, then goes
further:

1. [A first pipeline](#a-first-pipeline): a plan, two steps and a run.
2. [Make it resumable](#make-it-resumable): a journal file, and steps that can
   tell whether their work is done.
3. [Make it undoable](#make-it-undoable): steps that can reverse their work.
4. [A tool around it](#a-tool-around-it): `run`, `rollback` and `status`
   commands.
5. [Bigger plans](#bigger-plans): groups, parallel work, failure handlers and
   approval gates.
6. [Operating a run](#operating-a-run): stopping it, watching it and reading
   its status from another terminal.

The [reference](#reference) after that has the rules in full.

## Install

```bash
go get github.com/ruffel/pipeline
```

The styled terminal view is a separate module, so its dependencies stay opt-in:

```bash
go get github.com/ruffel/pipeline/observers/terminal
```

It needs Go 1.26. File journals need Linux, macOS or a BSD; elsewhere
`OpenJournal` returns an error.

## A first pipeline

A plan is a list of named steps, and the simplest step is a function:

```go
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/ruffel/pipeline"
)

func main() {
	plan, err := pipeline.NewPlan("publish",
		pipeline.Effect("style.css", pipeline.StepFunc(style)),
		pipeline.Effect("index.html", pipeline.StepFunc(index)),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	_, err = pipeline.Run(context.Background(), plan, pipeline.MemoryJournal(),
		pipeline.WithObservers(pipeline.NewPlainObserver(os.Stdout)))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func style(_ context.Context, sc *pipeline.StepContext) error {
	sc.Info("writing style.css")

	return os.WriteFile("style.css", []byte("h1 { color: teal }\n"), 0o600)
}

func index(_ context.Context, sc *pipeline.StepContext) error {
	sc.Info("writing index.html")

	return os.WriteFile("index.html", []byte("<h1>hello</h1>\n"), 0o600)
}
```

```text
$ go run .
▶ publish
    │ style.css: [INFO] writing style.css
  ✓ style.css
    │ index.html: [INFO] writing index.html
  ✓ index.html
✓ publish passed
```

- **`NewPlan`** checks the plan and returns it. Nothing has run yet.
- **`Effect`** marks a step that changes something. There are
  [two other kinds](#three-kinds-of-step).
- **`Run`** runs the steps in order, stops at the first one that fails, and
  returns its error.
- **The observer** shows the run. `NewPlainObserver` writes these lines, and
  there is a [styled terminal view and a JSON one](#observers).
- **`MemoryJournal`** keeps the record of the run in memory, so it is lost when
  the program ends. The next step changes that.

## Make it resumable

Give `Run` a file journal in place of the memory one. This replaces the call
to `Run`, and the error check after it stays:

```go
	j, err := pipeline.OpenJournal("site.jsonl")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	_, err = pipeline.Run(context.Background(), plan, j,
		pipeline.WithObservers(pipeline.NewPlainObserver(os.Stdout)))
	j.Close()
```

Every outcome is now written to `site.jsonl`, and synced, before the run moves
on. Calling `Run` again with the same plan and the same journal doesn't start
over. It starts a new **attempt**, which does only what is left.

To see that, make the second step fail on the first attempt (and import
`errors`):

```go
func index(_ context.Context, sc *pipeline.StepContext) error {
	if sc.Attempt() == 1 {
		return errors.New("disk full")
	}

	sc.Info("writing index.html")

	return os.WriteFile("index.html", []byte("<h1>hello</h1>\n"), 0o600)
}
```

```text
$ go run .
▶ publish
    │ style.css: [INFO] writing style.css
  ✓ style.css (10ms)
  ✗ index.html — disk full (4ms)
✗ publish failed — publish/index.html: disk full (23ms)
publish/index.html: disk full
exit status 1

$ go run .
▶ publish
  ⊘ style.css — earlier-attempt
    │ index.html: [INFO] writing index.html
  ✓ index.html (4ms)
✓ publish passed (12ms)
```

The second attempt skipped `style.css`, because the journal says it passed, and
ran `index.html`, which had failed. A third `go run .` is refused with
`ErrAlreadyComplete`, "the journal's latest attempt already passed": the
operation is finished.

It is the same when the process dies instead of failing. After a crash, a
`kill -9` or a reboot, the next run carries on from the journal. A step that
was running when the process died is run again, so **a step's `Run` must be
safe to call again** on work it has already partly done.

### Steps that check their own work

The journal knows what the run did. It can't know what has happened to the
system since: `style.css` is skipped even if somebody has deleted it. A step
that can tell whether its work is done says so with a `Check` method. A
function can't have one, so the step becomes a type:

```go
// file is a step that writes one file.
type file struct{ path, content string }

func (f file) Run(_ context.Context, sc *pipeline.StepContext) error {
	sc.Info("writing " + f.path)

	return os.WriteFile(f.path, []byte(f.content), 0o600)
}

// Check reports whether the file is already as it should be.
func (f file) Check(context.Context, *pipeline.StepContext) (bool, error) {
	got, err := os.ReadFile(f.path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}

	return string(got) == f.content, err
}
```

One type now serves both steps. Delete the two functions, the failure with
them, and import `io/fs`:

```go
	plan, err := pipeline.NewPlan("publish",
		pipeline.Effect("style.css", file{"style.css", "h1 { color: teal }\n"}),
		pipeline.Effect("index.html", file{"index.html", "<h1>hello</h1>\n"}),
	)
```

The journal from the last run is finished with, so remove it to start a new
operation. The two files are still there:

```text
$ rm site.jsonl
$ go run .
▶ publish
  ⊘ style.css — done (1ms)
  ⊘ index.html — done (1ms)
✓ publish passed (10ms)
```

Nothing was written, because each step asked the disk first. `Check` runs
before every `Run`, on a first attempt too, and it must not change anything. A
step whose file had gone would be run again, whatever a journal said.

Two things `Check` is not:

- **It is not a check of `Run`'s work.** Nothing calls it after a `Run` that
  returned nil: the step has passed. A step that must be verified once it is
  done verifies it at the end of its own `Run`, or is followed by an `Observe`
  step. And a passed `Effect` step with no `Check` is taken at the journal's
  word when a later attempt resumes.
- **`false` is not "don't know".** It sends the step on to `Run`. When `Check`
  can't reach the system, or finds something `Run` should not be let loose on,
  such as work half done by hand, it returns an error, and the step fails with
  nothing changed. The `Check` above returns the error from a file it could
  not read, and `false` only for one that is missing or stale, which writing
  it again puts right.

### Three kinds of step

`Effect` is one of three kinds. The kind says what a later attempt does with a
step that has already passed:

| Kind      | For                                       | Once it has passed, a later attempt…                     |
| --------- | ----------------------------------------- | -------------------------------------------------------- |
| `Effect`  | Work that changes the system once         | Checks it again if it has a `Check`, otherwise skips it  |
| `Ensure`  | A setting that can drift between attempts | Applies it again, until its group has passed             |
| `Observe` | Reading the system, such as health checks | Runs it again                                            |

Make a health check an `Observe` step, so that a resumed run makes it again.
Make a setting that something else can change back between attempts, such as a
maintenance mode, an `Ensure` step. An `Ensure` step is applied again only
while its group is unfinished: put one that must hold for the whole run at the
root of the plan. [Resume rules](#resume-rules) has every case.

## Make it undoable

A step that can reverse its work says how with an `Undo` method:

```go
// Undo removes the file. A file already gone is not an error: Undo can be
// called twice.
func (f file) Undo(_ context.Context, sc *pipeline.StepContext) error {
	sc.Info("removing " + f.path)

	err := os.Remove(f.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	return err
}
```

`Rollback` takes what `Run` takes, the same plan and the same journal, so one
argument can choose between them. This replaces the call to `Run` again:

```go
	do := pipeline.Run
	if len(os.Args) > 1 && os.Args[1] == "rollback" {
		do = pipeline.Rollback
	}

	_, err = do(context.Background(), plan, j,
		pipeline.WithObservers(pipeline.NewPlainObserver(os.Stdout)))
```

Remove `site.jsonl` and the two files to start from nothing, run it, and then
roll it back:

```text
$ go run . rollback
▶ publish (rollback)
    │ index.html: [INFO] removing index.html
  ↶ index.html undone (1ms)
    │ style.css: [INFO] removing style.css
  ↶ style.css undone (1ms)
✓ publish rolled back (11ms)
```

Nobody wrote a reverse plan. `Rollback` reads the journal, and:

- **It undoes the steps whose `Run` was called, last one first.** A step that
  `Check` found already done when it was first reached was never run, so it is
  left alone: a rollback undoes what the operation did, and nothing else. That
  is why this one started from nothing. Rolling back the operation before it,
  which wrote no files, removes none.
- **A step with no `Undo` is reported and stays done.** The rollback carries
  on, and still passes. Mark a step `Irreversible()` if a rollback must not
  go back past it.
- **It is an attempt like any other.** If an `Undo` fails, call `Rollback`
  again and it carries on from that step. Like `Run`, an `Undo` must be safe
  to call again.
- **After a rollback, `Run` does the work again.**

[Rollback rules](#rollback-rules) has the rest.

## A tool around it

A tool wants three commands: one to run or resume, one to roll back, and one
to say where things stand. Each is a few lines around the journal.

`run` is the same call for a first run and for a resume. What matters is what
it tells the operator when it doesn't pass:

```go
const journal = "site.jsonl"

func run(ctx context.Context, plan *pipeline.Plan, view pipeline.Observer) error {
	j, err := pipeline.OpenJournal(journal)
	if err != nil {
		return err // ErrJournalBusy: another process is running it
	}
	defer j.Close()

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
```

`Run` returns nil when everything passed. Otherwise it returns the failed
steps' errors, each in a `StepError` with its path, and `res.Outcome` says how
the attempt ended: `failed`, `stopped` or `cancelled`. `res` is nil when `Run`
refuses the journal before starting an attempt.

```go
func rollback(ctx context.Context, plan *pipeline.Plan, view pipeline.Observer) error {
	j, err := pipeline.OpenJournal(journal)
	if err != nil {
		return err
	}
	defer j.Close()

	_, err = pipeline.Rollback(ctx, plan, j, pipeline.WithObservers(view))

	return err // if an Undo failed, rollback again carries on from it
}
```

`status` needs no plan and no lock. `Inspect` reads the journal, even while a
run is writing it:

```go
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
```

The journal is the record that a resume and a rollback work from, so a tool
should never tell its operator to delete one that isn't finished with.

A `main` then only builds the plan and picks a command. `run` and `rollback`
open the journal themselves, so the earlier `OpenJournal` goes:

```go
func main() {
	plan, err := pipeline.NewPlan("publish",
		pipeline.Effect("style.css", file{"style.css", "h1 { color: teal }\n"}),
		pipeline.Effect("index.html", file{"index.html", "<h1>hello</h1>\n"}),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	ctx := context.Background()
	view := pipeline.NewPlainObserver(os.Stdout)

	switch {
	case len(os.Args) < 2:
		err = errors.New("usage: publish run|rollback|status")
	case os.Args[1] == "rollback":
		err = rollback(ctx, plan, view)
	case os.Args[1] == "status":
		err = status()
	default:
		err = run(ctx, plan, view)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
```

In `status`, a step reads `skipped` when a run found its work already done,
which is as good as `passed`:
[Reading the status](#reading-the-status) says how to tell.

[examples/01-quickstart](./examples/01-quickstart) is this program, complete, with
two more steps, the styled terminal view and a `-fail` flag. It is what the
recording at the top shows:

```bash
cd examples/01-quickstart
go run . run -fail index.html   # fails part-way
go run . run                    # resumes: only what is left
go run . rollback               # undoes it, last step first
go run . status
```

```text
attempt 1  run       failed
attempt 2  run       passed
attempt 3  rollback  passed

  not-reached  style.css
  not-reached  script.js
  not-reached  index.html
  not-reached  verify

rolled back: run publishes it again
```

## How it fits together

That is the whole model. Its parts have names:

- A **plan** is a tree of groups and steps, built once and validated by
  `NewPlan`. It is data: `Describe` renders it before anything changes, and it
  doesn't change during a run.
- Every element has a **path** built from its parents' names, such as
  `publish/index.html`. The path is its identity.
- A **step** is your code: a value with a `Run` method, and optionally `Check`
  and `Undo`.
- A **journal** is the record of one operation: an append-only file of events.
  Every outcome is written to it before the run moves on.
- Each call to `Run` or `Rollback` on a journal is an **attempt**. The first
  attempt starts the operation, and later ones resume it or undo it, by reading
  what the journal says earlier attempts did.
- **Observers** are told each event after it is stored. They show the run; they
  are never the record.

```text
 your tool ── Run / Rollback ──▶ journal file ◀── Follow / Inspect ── viewers
                                      ▲
            journal.control/ ◀── Approve / RequestStop ── other terminals
```

Everything else follows from the journal being the one record: resume reads
it, rollback reads it, and so does a viewer in another process.

## Bigger plans

A real plan has structure: checks first, work in batches, an approval before
the risky part. Steps go in groups, and a group is built by a function:

```go
func newPlan(c *cluster) (*pipeline.Plan, error) {
	return pipeline.NewPlan("upgrade",
		prechecks(c),
		pipeline.Effect("backup", backup{c}),
		nodes("canary", c, c.nodes[:1]),
		pipeline.Gate("continue", pipeline.GateOptions{
			Prompt:    "The canary is upgraded. Upgrade the rest?",
			Approvers: []pipeline.Approver{pipeline.StdinApprover()},
		}),
		nodes("rest", c, c.nodes[1:]),
		pipeline.Observe("verify", verify{c}),
	)
}

// prechecks only read the cluster, so a resumed run makes them again.
func prechecks(c *cluster) pipeline.GroupNode {
	return pipeline.Group("prechecks",
		pipeline.Observe("disk-space", diskSpace{c}),
		pipeline.Observe("ring-healthy", ringHealthy{c}),
	).Parallel(2)
}

// nodes upgrades a batch of nodes, two at a time.
func nodes(name string, c *cluster, batch []string) pipeline.GroupNode {
	groups := make([]pipeline.Element, 0, len(batch))

	for _, n := range batch {
		groups = append(groups, node(c, n))
	}

	return pipeline.Group(name, groups...).Parallel(2)
}

// node upgrades one node. If that fails, the node is put back on its old
// version, and a later attempt upgrades it from the start.
func node(c *cluster, name string) pipeline.GroupNode {
	return pipeline.Group(name,
		pipeline.Effect("drain", drain{c, name}),
		pipeline.Effect("packages", packages{c, name}).Retry(3, 10*time.Second),
		pipeline.Effect("restart", restart{c, name}).Timeout(2*time.Minute),
	).
		RecoverOnFailure(pipeline.Effect("restore", restore{c, name})).
		Labels(pipeline.L{"node": name})
}
```

`Describe` prints what that builds, before anything runs:

```text
upgrade
├── prechecks (parallel, limit 2)
│   ├── disk-space (observe)
│   └── ring-healthy (observe)
├── backup (effect, undo)
├── canary (parallel, limit 2)
│   └── node1
│       ├── drain (effect)
│       ├── packages (effect, 3 tries)
│       ├── restart (effect)
│       └── restore (effect, recovers the group on failure)
├── continue (gate)
├── rest (parallel, limit 2)
│   ├── node2
│   │   ├── drain (effect)
│   │   ├── packages (effect, 3 tries)
│   │   ├── restart (effect)
│   │   └── restore (effect, recovers the group on failure)
│   └── node3
│       ├── drain (effect)
│       ├── packages (effect, 3 tries)
│       ├── restart (effect)
│       └── restore (effect, recovers the group on failure)
└── verify (observe)
```

### Groups

A group runs its children in order, or `Parallel(n)` at most `n` at a time.
`Parallel(0)` sets no limit, and a child that is itself a group counts as one.
Groups nest to any depth.

When a child fails, a group starts nothing new and lets running children
finish. `CancelOnError` cancels them too, and `ContinueOnError` runs every
child and fails at the end.

On a resume, a group that passed is skipped whole, with one exception: a group
that holds nothing but `Observe` steps, like `prechecks`, always runs again.

### When a group fails

A group can name one step to run when it fails. Which one depends on what a
resume should do:

- **`OnFailure(step)`** cleans up around the group's work, such as turning
  monitoring back on. The work itself is left as it is, and a later attempt
  resumes the group **from where it failed**.
- **`RecoverOnFailure(step)`** takes the group back to where it started, such
  as putting a node back on its old version. The group is reset, as if none of
  it had run, and a later attempt runs the whole group **from the start**.

The handler runs when the group fails, not when it is stopped, and the group
still fails afterwards.

Take care when an `OnFailure` step reverses one of the group's own steps, such
as turning a maintenance mode off again. That step is still recorded as passed,
and if it is an `Effect` with no `Check`, a resumed attempt skips it. Make it
an `Ensure` step, which is applied again whenever its group is entered, or give
it a `Check`.

### Passing values between steps

`Record` stores a small value in the journal, and `Recorded` reads it by the
path of the step that stored it: from a later step, a later attempt, or an
`Undo`.

```go
func (b backup) Run(ctx context.Context, sc *pipeline.StepContext) error {
	archive := filepath.Join(b.c.backups, "upgrade.tar")
	if err := sc.Record("archive", archive); err != nil {
		return err
	}

	return b.c.archive(ctx, archive)
}

// Undo reads what its own Run recorded.
func (b backup) Undo(_ context.Context, sc *pipeline.StepContext) error {
	var archive string
	if found, err := sc.Recorded(sc.Path(), "archive", &archive); err != nil || !found {
		return err
	}

	return os.RemoveAll(archive)
}

// restore reads it from another step, by that step's path.
func (r restore) Run(ctx context.Context, sc *pipeline.StepContext) error {
	var archive string

	found, err := sc.Recorded("upgrade/backup", "archive", &archive)
	if err != nil {
		return err
	}

	if !found {
		return errors.New("no backup was recorded")
	}

	return r.c.restore(ctx, r.node, archive)
}
```

The value is stored before `Record` returns, so it is there even if the step
that recorded it is killed a moment later. Record what an `Undo` will need
before acting, not after.

`Record` is for small values. Everything one step records in one attempt may
come to `MaxRecordBytes`, 64 KiB: counted together and not for each key, over
every try of the step, and a key recorded again is counted again. A `Record`
that would go past it returns an error and stores nothing. Record where a
large result is kept, not the result.

### An order decided during the run

A plan is fixed when it is built, and sometimes the order of part of it is not
known until some of it has run: which node holds a service's master, and so
goes last, can depend on what earlier steps did. `OrderFrom` runs a group's
children in an order that a step records:

```go
pipeline.Effect("order", pipeline.StepFunc(func(_ context.Context, sc *pipeline.StepContext) error {
	// The node that holds the master goes last.
	return sc.Record("dc1", []string{"node2", "node3", "node1"})
})),
pipeline.Group("dc1", node(c, "node1"), node(c, "node2"), node(c, "node3")).
	OrderFrom("upgrade/order", "dc1"),
```

- **The children are the plan's.** The step decides only their order: a list
  of their names, each once. The fingerprint covers them as the plan lists
  them, so list them the same way on every attempt.
- **The order is settled when the group first starts,** and recorded with its
  start. A resume carries on in it, whatever the step has recorded since, and
  `State.Order` returns it.
- **A group back at never started reads it again:** after a recovery step
  resets it, a forced attempt, or a rollback that undoes everything in it.
- **With no order it can use, the group fails without starting.** None of its
  children runs, and its own failure handler has nothing to handle.
- **A rollback goes back the way the group ran,** not the way the plan lists
  it.
- **A settled order is the journal's.** A later build that takes the option
  off the group still carries on in it, until the group is back at never
  started. Meanwhile the group's children may not change, and it may not
  become parallel.
- **The journal is schema version 3** for an attempt whose plan has such a
  group, which v0.5 and earlier refuse to read. Before a tool starts using
  `OrderFrom`, upgrade everything else that reads its journal.

`NewPlan` refuses one that is parallel, or whose step may not have run when
it starts: a step after it, or alongside it in a parallel group.

### Gates

A `Gate` waits for an approval before the run goes on, as `continue` does
between the two batches above. It can also give up waiting:

```go
pipeline.Gate("continue", pipeline.GateOptions{
	Prompt:    "The canary is upgraded. Upgrade the rest?",
	Approvers: []pipeline.Approver{pipeline.StdinApprover()},
	Timeout:   30 * time.Minute,
	OnTimeout: pipeline.TimeoutStop,
})
```

`StdinApprover` asks on the terminal and `FileApprover` waits for a flag file.
`Approve` releases a gate from another process, by its path, so nothing has to
wait on a terminal. `Timeout` is optional. When it passes, `TimeoutStop` stops
the run, `TimeoutDecline` fails the gate, and `TimeoutContinue` releases it as
if approved. A gate released in an earlier attempt doesn't wait again.

### Settings on a step

- **`Retry(3, 10*time.Second)`** allows up to three tries within an attempt,
  ten seconds apart.
- **`Timeout(d)`** bounds each try, by cancelling the step's context. A try that
  returns after it has failed, whatever it returns.
- **`Irreversible()`** marks a step that can't be undone, such as a schema
  change. Once it has run, `Rollback` refuses: see
  [Rollback rules](#rollback-rules).
- **`Description(text)`** is a line for the views to show beside the name.
- **`Labels(l)`** are for observers and viewers of your own, to group or filter
  by node, region or tenant. A group's labels pass down to everything in it.
  They are in the plan that every viewer is given, and a step reads its own
  with `sc.Labels()`. The built-in views don't show them.

A step's `Run` can return `ErrSkipGroup` or `ErrSkipRun` to end its group, or
the run, early and as passed.

### Laying out a plan

A plan written as one nested expression gets hard to read. The plan above is
laid out like any other code:

- **One function for each group.** The top of the plan then reads as a table
  of contents, and each group's settings sit beside its few lines.
- **A loop for repeated structure,** with one function building the group for
  one node, datacenter or tenant.
- **A struct for each step,** holding what it works on. The plan names the
  step, and its `Check`, `Run` and `Undo` live together elsewhere.

Every builder method returns a changed copy and leaves the original alone, so
use what it returns: `g.Parallel(2)` on a line of its own changes nothing.

## Operating a run

### Stopping

- **`Stopper`** stops a run gracefully: running steps finish and nothing new
  starts. `HandleSignals` wires it to Ctrl-C: the first stops, the second
  cancels the running steps.
- **`RequestStop`** does either from another process.
- **Cancelling the context** cancels the running steps.

A stopped, cancelled or crashed attempt leaves the operation unfinished, and
the next `Run` resumes it. When `Run` returns after a stop or a cancel, its
error matches `ErrStopped`, and `res.Outcome` says which it was.

A stop takes effect between two steps, which can be the wrong place: a node
that has been stopped for its upgrade and not yet started again. Mark such a
sequence as one unit:

```go
pipeline.Group("node1", stop, install, start, verify).DrainOnStop()
```

- **A stop that finds the group started lets it finish.** The rest of its
  steps still start, the stop takes effect when the group ends, and nothing
  after it starts. A stop that comes before the group starts keeps it from
  starting, as it does anything else.
- **Only a stop is put off.** A step that fails, panics or passes its timeout
  ends the group there, and the run has failed, not stopped. A cancel cancels
  the running step. A step that fails once a stop is pending is not tried
  again, however many tries it has left, and one already waiting to be tried
  again stops waiting: either way the run has failed. So a stop can still
  leave the group part-way: finishing is what a healthy group does, and no
  protection against the process dying.
- **A group that fails is handled, stop or no stop.** The failure handlers of
  the groups around it still run, so a `RecoverOnFailure` step on a group
  around this one puts a failed node back instead of leaving it as it fell.
  The stop waits for them too: each needs a `Timeout`, and is not tried again
  under a stop. A cancel keeps them from starting. The run has failed, and a
  later attempt starts the node again from its first step.
- **Nothing in the group may hold a stop for ever, or end the group early.**
  It is sequential and stops at its first failure, every step in it has a
  `Timeout`, and it has no gate, failure handler or `DrainOnStop` group inside
  it. `NewPlan` reports one that breaks these. A step in it that returns
  `ErrSkipGroup` or `ErrSkipRun` fails.
- **`State.Draining()` lists the groups a stop is waiting for,** so a status
  command can say "stopping after node1" instead of "stopped".
- **A rollback takes no notice of it:** there a stop takes effect between any
  two `Undo`s.

An attempt whose plan has such a group is written as schema version 2. A
program built with a release before the option refuses that journal, where
it would otherwise resume it with a plan that stops in the middle of a node.

### Watching and controlling from elsewhere

Any process can read the journal while a run writes it:

- **`Follow`** delivers its events to observers as they are written, like
  `tail -f`. A viewer sees exactly what the runner's own observers saw. Set
  `FollowOptions.Result` and it hands back the `State` when it returns, so a
  viewer can exit with the run's outcome.
- **`Inspect`** replays it into a `State`: every attempt, and where each
  element stands.

An observer of your own, or a viewer in another language, reads the events
themselves. [JOURNAL.md](./JOURNAL.md) lists every kind of event and its fields.
[examples/06-web](./examples/06-web) is such a viewer: a web page built on
`Inspect`, `Follow`, `Approve` and `RequestStop`.

`Approve` and `RequestStop` leave their requests in a directory that `Run`
makes beside the journal, named after it with `.control` added, and return
once the journal shows the run acted on them.

The journal and that directory belong to the user who started the run, and
only that user can read or write them. Viewers and approvers run as the same
user. A journal has one writer at a time: a second `OpenJournal` fails with
`ErrJournalBusy`, and the error names the process that last wrote to it.

That lock is the journal's and no more. It is held while the journal is open,
not between attempts, so it reserves nothing for whoever ran the operation
last. It knows nothing of the system being worked on: two journals are two
operations, and nothing here keeps them off the same nodes. And it is a lock
on a file, between processes on one machine: whether it holds between machines
that share the file depends on the filesystem, and is not to be relied on. A
tool that needs any of these takes a lock of its own.

### Reading the status

`State.Attempts` lists every run and rollback with its outcome, and
`State.Element(path)` says where a group or a step stands:

| Status        | The element…                                                      |
| ------------- | ----------------------------------------------------------------- |
| `not-reached` | Has not been started by any attempt, or was undone by a rollback  |
| `started`     | Started and has not finished                                      |
| `passed`      | Finished its work                                                 |
| `skipped`     | Was not run, for the reason in `Reason`                           |
| `failed`      | Failed or was cancelled, with the error in `Error`                |
| `stopped`     | Is a group that a stop kept from starting some of its children    |

Three of them are easy to misread:

- **`started` is not the same as running.** When `State.Live` is true, a
  process has the journal open and is working on it. When it is false, the
  process that started the element died part-way, and the next `Run` picks it
  up. The same goes for an attempt whose `Outcome` is empty.
- **`skipped` usually means done.** The reasons `done`, `earlier-attempt` and
  `settled` say the work was already complete. Any other reason says the run or
  the group ended before the element was reached. `ElementState.Done` gives the
  answer either way.
- **`not-reached` after a rollback means undone.** A step that a rollback
  undoes goes back to `not-reached`, ready to run again. A step whose `Undo`
  failed keeps its status and has an `UndoError`, and a group around work that
  could not be undone reads `started`.

A run that passed may still have something to say. `State.Warnings()` lists
what its steps recorded as wrong without failing, with `RecordWarning` or as
items with the status `warned`, so a tool can exit differently for a run that
passed with warnings. A step that an earlier attempt finished with a warning
still has it after the attempts that skipped it.

`Inspect` gives the state. A tool that wants the events too, for a report or to
show the messages and output that the state does not keep, calls
`ReadEvents(path)`: it returns every event with the state they replay to, from
one reading of the journal, whether or not a run is still writing it.

What an operator can do next depends only on the latest attempt:

| Latest attempt             | Next                                                  |
| -------------------------- | ----------------------------------------------------- |
| A run that passed          | Nothing to do. `Rollback` undoes it                   |
| A run that didn't          | `Run` resumes it, or `Rollback` undoes it             |
| A rollback that passed     | It is undone. `Run` does the work again               |
| A rollback that didn't     | `Rollback` again. `Run` is refused until it passes    |

### Inputs

`WithInputs` records what the plan was built from: answers, flags, discovery
results. A resume must be given the same inputs, or `Run` returns
`ErrInputsChanged`. A tool can read them back with `Inspect` and
`State.DecodeInputs`, and build the same plan without asking again.
`WithNewInputs` accepts a change, but work already done under the old inputs
is not done again for it.

The usual fix after a failed run is to correct one input and run again. That
is safe if nothing has used the input yet, and `WithInputConsumers` says what
uses what:

```go
pipeline.WithInputConsumers(map[string][]string{
	"/hosts/0": {"install/node1"},
	"/hosts/1": {"install/node2"},
	"/license": {"install/activate"},
})
```

A field is named by its JSON pointer into the encoded inputs, and a pointer
covers everything beneath it. On resume, a changed field is accepted if none
of its consumers has started, and the attempt records which fields changed.
If a consumer has started, or the field has no consumer declared, `Run`
returns an `*InputChangeError`, which matches `ErrInputsChanged` and lists
each field, its old and new value, and the step that used it. A group counts
as started once anything in it has. For a rule that isn't a list of paths,
`WithInputChange` takes a function that is given the change and the journal's
state.

Fields of type `Secret` are never stored, so they are supplied on every
attempt, and their values are masked wherever a step writes them.
`WithRedactor` masks anything else.

#### Keeping an incompatible build out

A plan's fingerprint is its shape. A later build can keep every path and
still mean something else by what is recorded under one: a step that records
another value, a `Check` that takes something else for granted. Nothing in
the plan says so, and the tool is the only one that knows.

So the tool says it in its inputs: an identifier for the operation, and one
for each step whose record could be read differently, changed only when the
meaning changes.

```go
type inputs struct {
	Target    string    `json:"target"`
	Contracts contracts `json:"contracts"`
}

type contracts struct {
	Operation string            `json:"operation"`
	Steps     map[string]string `json:"steps,omitempty"` // by path
	Migration string            `json:"migration,omitempty"`
}
```

- **A build with other contracts is refused** before any `Check`, `Run` or
  `Undo` is called and before anything is written, whether it calls `Run`,
  `Run` with `Force`, or `Rollback`. The error is `ErrInputsChanged`. If the
  plan's shape differs as well, the shape is asked about first: the error is
  `ErrPlanChanged`, and the function below is not called, until
  `WithPlanChange` accepts the shape.
- **A transition the tool supports is its own to approve**, in a
  `WithInputChange` function. It is given the old and new inputs, the fields
  that differ and the journal's state, and its error comes back from `Run`.
  The attempt's `run.started` then records the new contracts, the name of the
  migration and the fields that changed, before any step starts. The old ones
  stay in the attempt before it.
- **A journal with no contracts is refused in the same way**, until the tool
  approves that one: nothing makes a matching identifier up.
- **It works both ways.** A build from before the tool had contracts brings
  none, and is refused a journal that has them. `WithNewInputs` is the way
  round all of this, so a tool that relies on it never gives that option to a
  resume.
- **The approval does not outrank the journal.** A rollback past an
  irreversible step, a step with no `Undo` under `WithStrictUndo` and a
  changed kind are refused whatever the function says.

The function is asked only when the inputs differ. A check to make on every
attempt is made by the tool, before it calls `Run`, from the journal it holds
open: `Journal.State()` is the journal as it stands, and `OpenJournal`'s lock
keeps every other writer out until the journal is closed. One worth making
under `WithPlanChange`: a step added to a group that had finished makes a
resume enter that group again, where its `Ensure` steps run and its `Check`s
are called. By the time `WithInputChange` is asked, the state it is given
already has the group as unfinished, so only the tool's own look at
`Journal.State()` shows that it had finished.

## Reference

### Resume rules

Call `Run` again with the same plan and the same journal. It starts a new
attempt that does only what is left:

1. **A group that passed in an earlier attempt is skipped whole.** Nothing in
   it runs, not even its `Ensure` and `Observe` steps. (A group with nothing but
   `Observe` steps beneath it is the exception: it always runs again.)
2. **In every other group, each step is decided by its kind and by what the
   journal says happened to it:**

| What the journal says  | `Effect` with a `Check`           | `Effect` without  | `Ensure`, `Observe` |
| ---------------------- | --------------------------------- | ----------------- | ------------------- |
| It passed              | `Check`, then `Run` if not done   | Skipped           | Runs again          |
| It failed              | `Check`, then `Run` if not done   | `Run`             | Runs again          |
| It was interrupted     | `Check`, then `Run` if not done   | `Run`             | Runs again          |
| It never started       | `Check`, then `Run` if not done   | `Run`             | Runs                |

An `Ensure` step with a `Check` is checked before each `Run`, like any other.
`WithResume(ResumeTrust)` skips passed `Effect` steps without calling `Check`.

Rule 1 comes first, so "runs again" in the table holds only while the step's
group is unfinished. An `Ensure` step in a group that has passed is not applied
again, however many attempts follow. A prelude group that turns off background
maintenance for an upgrade passes on the first attempt, and a resume after
something turns maintenance back on does not turn it off again. Put a setting
that must hold for the whole run at the root of the plan, which is unfinished
until the run passes, or in the group whose work needs it. `Describe` marks
each `Ensure` step inside a group with the group it holds until:

```text
upgrade
├── maintenance-off (ensure)
└── node1
    └── drained (ensure, until node1 passes)
```

The journal belongs to one operation and one plan:

- **A changed plan is refused** with `ErrPlanChanged`, if its paths or kinds
  differ. Descriptions, labels, limits, timeouts and retries can change, so a
  rebuilt tool can resume.
- **`WithPlanChange(MatchByPath)` accepts one**, for a fixed build that adds or
  removes steps. Elements are matched by path. One in both plans keeps its
  record, and a changed kind is still refused. A new one has never started, and
  a group that had settled is entered again for it. A removed one keeps its
  record and is listed in `State.Removed`; if it ran and had an `Undo`,
  `Rollback` refuses, because the code to undo it has gone. The journal then
  belongs to the new plan, so later attempts need no option.
- **A finished journal is refused** with `ErrAlreadyComplete`, once its latest
  attempt has passed. Start a new journal for a new operation, or give `Force`
  to run the whole plan again.
- **`Force` runs every step again**, and `Check` decides each one. An `Effect`
  step with no `Check` has nothing to stop it, so its `Run` is called again.
  To re-apply the settings and repeat the checks of a finished operation
  without redoing its work, don't force it: build a plan of only those
  `Ensure` and `Observe` steps and run it on a new journal.
- **If the journal can't be written, the run stops.**

### Rollback rules

`Rollback` undoes what the journal says was done. It takes every `Effect` and
`Ensure` step whose `Run` was called, whether that `Run` passed, failed or was
interrupted, and calls each one's `Undo`, one at a time, in reverse plan order.

- A step whose `Run` no attempt called isn't undone: one that `Check` found
  already done when it was first reached, for example. A step that an earlier
  attempt ran is undone, even if a later attempt skipped it as done.
- The steps of an `OrderFrom` group are undone in the reverse of the order
  they ran in, which is not the order the plan lists them in.
- A step with no `Undo` is skipped and reported, and the rollback still passes.
  The step stays done, so a later `Run` doesn't do it again. `Describe` shows
  which steps have an `Undo`, before anything runs.
- With `WithStrictUndo()`, `Rollback` refuses instead, before it undoes
  anything, if a step it would reach has no `Undo`. The error matches
  `ErrNoUndo` and names every such step.
- A step marked `Irreversible()` is a point of no return. Once any attempt has
  called its `Run`, whether it passed, failed or was interrupted, `Rollback`
  refuses with `ErrIrreversible`, naming the step, and undoes nothing: undoing
  the steps around it would not put the system back where it started. Until
  then a rollback goes ahead as usual. `State.PointOfNoReturn()` returns the
  step's path, so a status command can say that rolling back is no longer
  possible. `Run` still resumes the journal.
- The journal keeps what a step was when it ran. A later build that takes the
  marker off, gives the step an `Undo`, or takes the step out of the plan is
  refused all the same, and so is one that marks a step that had already run.
  Nothing clears a point of no return.
- A group's failure handler is undone like any other step, if it ran. A
  recovery handler's reset of its group doesn't take back an irreversible step
  that ran in it.
- What an undone step recorded is forgotten.
- `Rollback` needs the same plan and the same inputs as the run.
- It is an attempt of its own. If an `Undo` fails, `Run` is refused with
  `ErrRollbackUnfinished` until `Rollback` is called again and passes; it
  carries on where it stopped, calling again an `Undo` that was interrupted.
- After a rollback passes, `Run` runs what was undone again.

An `Undo` often needs something its `Run` knew, such as where it put a backup.
Record it before acting, and `Undo` can read it even if `Run` was killed
part-way: see [Passing values between steps](#passing-values-between-steps).

### What a step can do

A step is a value with a `Run` method. It can also have
`Check(ctx, sc) (bool, error)`, which says whether its work is already done and
must not change the system, and `Undo(ctx, sc) error`, which reverses it.

`Run` and `Undo` must be idempotent: a process can die after one of them
changed the system and before the journal said so, and the next attempt calls
it again.

Both are given a context, and that is the only way a step is stopped. A
timeout, a cancel and a second Ctrl-C all cancel it. A step that ignores its
context runs on regardless. When it does return, after its timeout or after a
cancel, it has failed, even if it returns nil.

Returning is all a cancel asks of a step. Work it started somewhere else, such
as a job on another machine, carries on unless the step stops it, and the
journal then says only that the step failed. Record how to find such work
before starting it, so that the next attempt's `Check` can ask after it.

`StepFunc` turns a plain function into a step with neither `Check` nor `Undo`.
`Check` and `Undo` are found on the value you give the plan, so a wrapper
around a step must forward them. `Describe` shows what each step has.

The `StepContext` is the step's way into the run:

- `Info`, `Warn`, `Progress`, `Stdout` and `Stderr` report what it is doing.
  They are best-effort: a crash can lose the last of them, and output past a
  step's allowance is dropped. Nothing a later attempt or an `Undo` needs
  goes here. Each `Progress` report says when its task was first reported, so
  a viewer that joins late can show how long it has run.
- `Record` stores a small value durably, and `Recorded` reads it from a later
  step, a later attempt or an `Undo`. `RecordOnce` stores one only if the step
  has none under that key yet, for a value that must be the first one seen,
  such as what a setting was before the step changed it.
- `Item` records a result for one target of a step that fans out, such as one
  host, and `PreviousItems` reads them back on a later attempt. It is a record
  and no more: the run doesn't know which targets a step has, a failed item
  doesn't fail the step, and what to do about each target next time is the
  step's to decide. `ItemWith` adds small data of the step's own to a result,
  such as the host it is about, so that a viewer need not parse it out of the
  ID or the message.
- `RecordWarning` records, durably, that something is wrong which does not
  fail the step, and an item can have the status `ItemWarned`. The run takes
  no notice of either. `State.Warnings()` lists those that stand, which is how
  a tool tells a run that passed from one that passed with warnings.
- `Inputs` returns what the run was given with `WithInputs`.
- `Attempt`, `Path` and `Labels` say which attempt this is, and the step's own
  path and labels.
- `Custom` sends an event of your own to observers, best-effort like a
  message.

Only the secrets among the inputs are known to the run. One that a step makes
or reads while it runs, such as a generated password or a token in a command's
output, is masked only where the `WithRedactor` function catches it: in
messages, output, errors, custom events and the data of an item. A value given to `Record`, an
item's ID, a label and a path are stored as they are, so none of them may be
secret.

### Observers

| Observer | Package                 | Output                                |
| -------- | ----------------------- | ------------------------------------- |
| Terminal | `observers/terminal`    | Styled boxes, colors, aligned columns |
| Plain    | `pipeline`              | Simple text lines                     |
| JSON     | `pipeline`              | JSON Lines, in the journal's format   |
| Custom   | `pipeline.ObserverFunc` | Any function                          |

Observers are called in journal order, after each event is stored, on the
goroutine that wrote it, so an observer that blocks stalls the run. Wrap one
that might, such as a socket or a pipe, in `NewAsyncObserver`. The exception is
a terminal view beside a `StdinApprover`: call it directly, so a gate's line is
printed before its question.

During a run, a panic in an observer is recovered and that observer is dropped.

### Testing

The `pipelinetest` package tests steps and plans without a real system:

- **`RunStep`** runs one step on its own and returns what it recorded.
- **`CheckIdempotent`** crashes a step after every event it writes, resumes it,
  and checks that it still passes.
- **`CrashAfter`** simulates a crash at any point of a run.
- **`FakeClock`** moves time on by hand, so retry backoffs and gate timeouts
  don't sleep.

`CheckIdempotent` and `CrashAfter` crash a run by making its journal refuse
every write after a chosen event. That finds the hazard that matters most, a
step that changed the system with nothing recorded. It is not a process dying:

- **The crash points are the run's writes to the journal.** A step that
  changes the system in several stages, with nothing written between them, is
  crashed before the first and after the last, never between two. Have it
  `Record` what it is about to do, or make the fake system fail at that stage.
- **A resumed run that passes has only passed.** A step that does its work
  twice without an error passes too. Check the fake system's final state: how
  many times, and what is there.
- **Nothing is killed, and no disk is involved.** The step's code runs on to
  its next write, which fails, and the journal is in memory. A test that kills
  a child process working on a file journal covers what these can't: this
  module's `TestResume_AProcessKilledAroundItsEffect` is one to copy. Even
  that is not a power cut, which leaves only what was synced.

## Examples

They are numbered in the order to read them. Each runs with `go run .` from
its own directory, and says at the top of its `main.go` what to try.

1. **[01-quickstart](./examples/01-quickstart)**: the program this page builds.
   Three steps with `Check` and `Undo`, and `run`, `rollback` and `status`
   commands
2. **[02-demo](./examples/02-demo)**: what a run shows. Groups, parallelism,
   progress, output, retries and a timeout, in each observer's format
3. **[03-runbook](./examples/03-runbook)**: a failover runbook with approval
   gates, descriptions, and resuming from the journal
4. **[04-detached](./examples/04-detached)**: a run that other terminals watch,
   approve and stop
5. **[05-migrate](./examples/05-migrate)**: a complete tool. A failure handler,
   a recorded value used by an `Undo`, inputs read back from the journal, a
   secret, and tests that crash its steps
6. **[06-web](./examples/06-web)**: a viewer in a browser, in a process of its
   own. Where every step stands, the events as they are written, and buttons
   to approve a gate or stop the run

## Stability

pipeline is pre-1.0, and its API can change between minor versions. The
journal's format carries a schema version: a release reads journals written by
every earlier schema version, and refuses a newer one. An attempt is written
as the oldest version that can say everything about it, so a journal that uses
nothing new stays readable by older releases.

## Upgrading from v0.4

v0.5.0 changes two things for a tool that already works:

- **A rollback can be refused where v0.4 went ahead.** A point of no return
  is now kept for good. v0.4 asked the latest plan, so a later build that took
  the `Irreversible()` marker off a step that had run could roll back past it,
  and so could one that removed the step and put it back. Both are now refused
  with `ErrIrreversible`. That goes for journals v0.4 wrote too: what a step
  was when it ran is read from the plans they already hold. `Run` resumes them
  as before.
- **A journal can be schema version 2.** A run whose plan has a `DrainOnStop`
  group is written as version 2, which v0.4 and earlier refuse to read. Every
  other journal is still version 1, and v0.4 reads it. So before a tool starts
  using `DrainOnStop`, upgrade everything else that reads its journal, such as
  a status command or a viewer.

Nothing was removed from the API, and journals written by v0.4 are read as
before.

## Upgrading from v0.3

v0.4.0 changes what two kinds of step are recorded as. Both used to pass:

- **A step that returns after its timeout fails**, whatever it returns. A step
  with a `Timeout` that ignores its context and finishes late now fails with an
  error matching `context.DeadlineExceeded`, and is tried again if it has tries
  left. The same goes for an `Undo`.
- **A step that returns after a cancel fails**, whatever it returns. A step
  that was running when the run was cancelled, or when its group cancelled it
  under `CancelOnError`, and then returns nil is recorded as failed, so a
  resume runs it again unless its `Check` finds the work done.

A step that watches its context and returns the context's error is unaffected.

`Describe` now marks an `Ensure` step inside a group, such as
`(ensure, until node1 passes)`, and the plain observer's lines for parallel
groups and rollbacks have changed. Update anything that compares that text.

Nothing was removed from the API, and journals written by v0.3 are read as
before.

## Upgrading from v0.2

v0.3.0 replaces the v0.2 API: `Pipeline`, `Stage` and `Executor` are now
`Plan`, `Group` and `Run`, steps are values with `Run` and optional `Check` and
`Undo`, and the `EmitX` helpers are `StepContext` methods.
