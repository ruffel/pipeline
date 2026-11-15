# pipeline

Durable, resumable pipelines for Go.

Plan it, run it, kill it, resume it, roll it back, and watch it from another
terminal, all recorded in one journal file, with no server. It is for tools
that operators run against real systems: installers, upgrades, migrations,
failovers, data repairs.

![demo](./.vhs/demo.gif)

It sits between a script and a workflow engine:

- Scripts and `errgroup` keep no record, so they can't resume after a failure
  or roll back.
- Workflow engines such as Temporal are durable, but need a server or a
  database.
- pipeline is a library: your tool links it, and the record is a file.

[DESIGN.md](./DESIGN.md) says why it works the way it does, and what it
deliberately leaves out.

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

## Quick Start

```go
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/ruffel/pipeline"
)

func main() {
	plan, err := pipeline.NewPlan("deploy",
		pipeline.Group("build",
			pipeline.Effect("compile-api", compile("api")),
			pipeline.Effect("compile-worker", compile("worker")),
		).Parallel(2),
		pipeline.Effect("push-image", compile("image")).Retry(3, time.Second),
		pipeline.Observe("smoke-test", compile("smoke")).Timeout(time.Minute),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	_, err = pipeline.Run(context.Background(), plan, pipeline.MemoryJournal(),
		pipeline.WithObservers(pipeline.NewPlainObserver(os.Stdout)))
	if err != nil {
		os.Exit(1)
	}
}

// compile stands in for real work.
func compile(what string) pipeline.StepFunc {
	return func(_ context.Context, sc *pipeline.StepContext) error {
		sc.Info("building " + what)

		return nil
	}
}
```

`MemoryJournal` keeps the record in memory, which is enough to try it. Give
`Run` a [file journal](#long-running-pipelines) and the run can be resumed.

## How it fits together

- A **plan** is a tree of groups and steps, built once and validated by
  `NewPlan`. It is data: `Describe` prints it before anything changes, and it
  doesn't change during a run.
- Every element has a **path** built from its parents' names, such as
  `deploy/build/compile-api`. The path is its identity.
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

## Steps

Each step says how it behaves when an operation is resumed:

| Kind      | For                                       | Once it has passed, a later attempt…                     |
| --------- | ----------------------------------------- | -------------------------------------------------------- |
| `Effect`  | Work that changes the system once         | Checks it again if it has a `Check`, otherwise skips it  |
| `Ensure`  | A setting that can drift between attempts | Applies it again                                         |
| `Observe` | Reading the system, such as health checks | Runs it again                                            |

A step can also implement:

- **`Check(ctx, sc) (bool, error)`**: whether its work is already done. It runs
  before every `Run`, on first attempts too, and a step whose work is done is
  skipped. It must not change the system.
- **`Undo(ctx, sc) error`**: how to reverse its work, for `Rollback`.

`Run` must be idempotent: a process can die after `Run` changed the system and
before the journal said so, and the next attempt calls it again.

```go
type pushImage struct{ ref string }

func (p pushImage) Check(ctx context.Context, sc *pipeline.StepContext) (bool, error) {
	return registry.Has(ctx, p.ref)
}

func (p pushImage) Run(ctx context.Context, sc *pipeline.StepContext) error {
	return registry.Push(ctx, p.ref)
}

func (p pushImage) Undo(ctx context.Context, sc *pipeline.StepContext) error {
	return registry.Delete(ctx, p.ref)
}
```

`StepFunc` turns a plain function into a step with neither `Check` nor `Undo`.
`Check` and `Undo` are found on the value you give the plan, so a wrapper
around a step must forward them. `Describe` shows what each step has.

The `StepContext` is the step's way into the run:

- `Info`, `Warn`, `Progress`, `Stdout` and `Stderr` report what it is doing.
- `Record` stores a small value durably, and `Recorded` reads it from a later
  step, a later attempt or an `Undo`.
- `Item` records a result for one target of a step that fans out, such as one
  host, and `PreviousItems` reads them back on a later attempt.
- `Inputs` returns what the run was given with `WithInputs`.
- `Custom` sends an event of your own to observers.

Steps are configured with `Retry`, `Timeout`, `Description` and `Labels`.
`Retry(3, time.Second)` allows up to three tries within an attempt, a second
apart. A step's `Run` can return `ErrSkipGroup` or `ErrSkipRun` to end its
group, or the run, early and as passed.

## Groups

A group runs its children in order, or `Parallel(n)` at most `n` at a time.
Groups nest to any depth, and their labels pass down to their children.

When a child fails, a group starts nothing new and lets running children finish.
`CancelOnError` cancels them too, and `ContinueOnError` runs every child and
fails at the end.

A group can name one step to run when it fails:

- **`OnFailure(step)`** cleans up around the group's work, such as turning
  monitoring back on. A later attempt resumes the group from where it failed.
- **`RecoverOnFailure(step)`** takes the group back to where it started, such as
  putting a node back on its old version. A later attempt runs the whole group
  again.

## Laying out a plan

A plan written as one nested expression gets hard to read. Lay it out like
any other code:

```go
func newPlan(c *cluster) (*pipeline.Plan, error) {
	return pipeline.NewPlan("upgrade",
		prechecks(c),
		pipeline.Effect("backup", backup{c}),
		nodes(c),
		pipeline.Observe("verify", verify{c}),
	)
}

// nodes upgrades every node, two at a time.
func nodes(c *cluster) pipeline.GroupNode {
	groups := make([]pipeline.Element, 0, len(c.nodes))

	for _, name := range c.nodes {
		groups = append(groups, node(c, name))
	}

	return pipeline.Group("nodes", groups...).Parallel(2)
}

func node(c *cluster, name string) pipeline.GroupNode {
	return pipeline.Group(name,
		pipeline.Effect("packages", packages{c, name}),
		pipeline.Effect("restart", restart{c, name}),
	).Labels(pipeline.L{"node": name})
}
```

- **One function for each group.** The top of the plan then reads as a table
  of contents, and each group's settings sit beside its few lines.
- **A loop for repeated structure,** with one function building the group for
  one node, datacenter or tenant.
- **A struct for each step,** holding what it works on. The plan names the
  step, and its `Check`, `Run` and `Undo` live together elsewhere.

Every builder method returns a changed copy and leaves the original alone, so
use what it returns: `g.Parallel(2)` on a line of its own changes nothing.

## Long-running pipelines

Give `Run` a file journal and the operation survives its process:

```go
j, err := pipeline.OpenJournal("deploy.jsonl")
if err != nil {
	return err
}
defer j.Close()

res, err := pipeline.Run(ctx, plan, j, pipeline.WithObservers(view))
```

Every outcome is written to the journal, and synced, before the run moves on.
If the journal can't be written, the run stops. A journal has one writer at a
time: a second `OpenJournal` fails with `ErrJournalBusy`.

`Run` returns nil when everything passed. Otherwise it returns the failed
steps' errors, each in a `StepError` with its path, and `res.Outcome` says how
the attempt ended: `failed`, `stopped` or `cancelled`.

### Resuming

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

The journal belongs to one operation and one plan:

- **A changed plan is refused** with `ErrPlanChanged`, if its paths or kinds
  differ. Descriptions, labels, limits, timeouts and retries can change, so a
  rebuilt tool can resume.
- **A finished journal is refused** with `ErrAlreadyComplete`, once its latest
  attempt has passed. Start a new journal for a new operation, or give `Force`
  to run the whole plan again.

### Inputs

`WithInputs` records what the plan was built from: answers, flags, discovery
results. A resume must be given the same inputs, or `Run` returns
`ErrInputsChanged`. A tool can read them back with `Inspect` and
`State.DecodeInputs`, and build the same plan without asking again.

Fields of type `Secret` are never stored, so they are supplied on every
attempt, and their values are masked wherever a step writes them.
`WithRedactor` masks anything else.

### Stopping

- **`Stopper`** stops a run gracefully: running steps finish and nothing new
  starts. `HandleSignals` wires it to Ctrl-C: the first stops, the second
  cancels the running steps.
- **`RequestStop`** does either from another process.
- **Cancelling the context** cancels the running steps.

A stopped, cancelled or crashed attempt leaves the operation unfinished, and
the next `Run` resumes it. When `Run` returns after a stop or a cancel, its
error matches `ErrStopped`, and `res.Outcome` says which it was.

### Gates

A `Gate` waits for an approval before the run goes on:

```go
pipeline.Gate("continue-dc2", pipeline.GateOptions{
	Prompt:    "Upgrade datacenter DC2?",
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

### Watching and controlling from elsewhere

Any process can read the journal while a run writes it:

- **`Follow`** delivers its events to observers as they are written, like
  `tail -f`. A viewer sees exactly what the runner's own observers saw.
- **`Inspect`** replays it into a `State`: every attempt, where each element
  stands, and whether a process is running it now (`Live`).

An observer of your own, or a viewer in another language, reads the events
themselves. [JOURNAL.md](./JOURNAL.md) lists every kind of event and its fields.

`Approve` and `RequestStop` leave their requests in a directory that `Run`
makes beside the journal, named after it with `.control` added, and return
once the journal shows the run acted on them.

The journal and that directory belong to the user who started the run, and
only that user can read or write them. Viewers and approvers run as the same
user.

### Rolling back

`Rollback` undoes what the journal says was done. It takes every `Effect` and
`Ensure` step whose `Run` was called, whether that `Run` passed, failed or was
interrupted, and calls each one's `Undo`, one at a time, in reverse plan order.
Nobody writes the reverse plan.

- A step that `Check` found already done was never run, so it isn't undone.
- A step with no `Undo` is skipped and reported, and the rollback still passes.
  `Describe` shows which steps have one, before anything runs.
- `Rollback` needs the same plan and the same inputs as the run.
- It is an attempt of its own. If an `Undo` fails, `Run` is refused with
  `ErrRollbackUnfinished` until `Rollback` is called again and passes; it
  carries on where it stopped.
- After a rollback passes, `Run` runs what was undone again.

An `Undo` often needs something its `Run` knew. Record it before acting, and
`Undo` can read it even if `Run` was killed part-way:

```go
type backup struct{ dir string }

func (b backup) Run(ctx context.Context, sc *pipeline.StepContext) error {
	archive := filepath.Join(b.dir, "backup.tar")
	if err := sc.Record("archive", archive); err != nil {
		return err
	}

	return writeArchive(ctx, archive)
}

func (b backup) Undo(_ context.Context, sc *pipeline.StepContext) error {
	var archive string
	if found, err := sc.Recorded(sc.Path(), "archive", &archive); err != nil || !found {
		return err
	}

	return os.RemoveAll(archive)
}
```

### Putting it together

A tool with a `run` and a `rollback` command needs little more than this:

```go
func run(ctx context.Context, plan *pipeline.Plan, view pipeline.Observer) error {
	j, err := pipeline.OpenJournal("deploy.jsonl")
	if err != nil {
		return err // ErrJournalBusy: another process is running it
	}
	defer j.Close()

	// Ctrl-C stops the run once its running steps finish.
	stopper := pipeline.NewStopper()
	defer pipeline.HandleSignals(stopper, pipeline.SignalOptions{})()

	_, err = pipeline.Run(ctx, plan, j,
		pipeline.WithObservers(view), pipeline.WithStopper(stopper))

	switch {
	case err == nil:
		return nil
	case errors.Is(err, pipeline.ErrAlreadyComplete):
		return errors.New("already deployed: remove deploy.jsonl to start again")
	case errors.Is(err, pipeline.ErrRollbackUnfinished):
		return errors.New("a rollback has not finished: run rollback again")
	case errors.Is(err, pipeline.ErrStopped):
		return errors.New("stopped: run again to resume")
	default:
		return fmt.Errorf("%w\nfix the cause and run again to resume, or roll back", err)
	}
}

func rollback(ctx context.Context, plan *pipeline.Plan, view pipeline.Observer) error {
	j, err := pipeline.OpenJournal("deploy.jsonl")
	if err != nil {
		return err
	}
	defer j.Close()

	_, err = pipeline.Rollback(ctx, plan, j, pipeline.WithObservers(view))

	return err // if an Undo failed, rollback again carries on from it
}
```

## Observers

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

## Testing

The `pipelinetest` package tests steps and plans without a real system:

- **`RunStep`** runs one step on its own and returns what it recorded.
- **`CheckIdempotent`** crashes a step after every event it writes, resumes it,
  and checks that it still passes.
- **`CrashAfter`** simulates a crash at any point of a run.
- **`FakeClock`** moves time on by hand, so retry backoffs and gate timeouts
  don't sleep.

## Examples

- **[demo](./examples/demo)**: groups, parallelism, progress, output, retries
  and a timeout, in each observer's format
- **[migrate](./examples/migrate)**: a tool with `run`, `rollback` and `status`
  commands: steps with `Check` and `Undo`, a failure handler, inputs read back
  from the journal, and tests that crash its steps
- **[runbook](./examples/runbook)**: a failover runbook with approval gates,
  descriptions, and resuming from the journal
- **[detached](./examples/detached)**: a run that other terminals watch,
  approve and stop

## Stability

pipeline is pre-1.0, and its API can change between minor versions. The
journal's format carries a schema version: a release reads journals written by
every earlier schema version, and refuses a newer one.

## Upgrading from v0.2

v0.3.0 replaces the v0.2 API: `Pipeline`, `Stage` and `Executor` are now
`Plan`, `Group` and `Run`, steps are values with `Run` and optional `Check` and
`Undo`, and the `EmitX` helpers are `StepContext` methods.
