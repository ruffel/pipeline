# pipeline

Durable, resumable runbooks for Go.

Plan it, run it, kill it, resume it, roll it back, and watch it from another
terminal, all from one journal file, with no server. It is for tools that
operators run against real systems: installers, upgrades, migrations,
failovers, data repairs.

![demo](https://vhs.charm.sh/vhs-6b0W5VnjsJ8GpqRxTUpyt9.gif)

## Install

```bash
go get github.com/ruffel/pipeline
```

## Quick Start

```go
plan, err := pipeline.NewPlan("deploy",
    pipeline.Group("build",
        pipeline.Effect("compile-api", compile("api")),
        pipeline.Effect("compile-worker", compile("worker")),
    ).Parallel(2),
    pipeline.Effect("push-image", pushImage{}).Retry(3, time.Second),
    pipeline.Observe("smoke-test", smokeTest{}).Timeout(time.Minute),
)
if err != nil {
    return err
}

_, err = pipeline.Run(ctx, plan, pipeline.MemoryJournal(),
    pipeline.WithObservers(terminal.New(os.Stdout)))
```

A step is anything with a `Run` method; `StepFunc` turns a function into one:

```go
func compile(service string) pipeline.StepFunc {
    return func(ctx context.Context, sc *pipeline.StepContext) error {
        sc.Info("compiling " + service)
        return exec.CommandContext(ctx, "go", "build", "./cmd/"+service).Run()
    }
}
```

A plan is data. `NewPlan` validates it, `Describe` prints it before anything
changes, and it doesn't change during a run. Every element has a path built
from its parents' names, such as `deploy/build/compile-api`, which is its
identity in the journal.

## Steps

Each step says how it behaves when a run is resumed:

| Kind      | For                                       | On a resumed run                                     |
| --------- | ----------------------------------------- | ---------------------------------------------------- |
| `Effect`  | Work that changes the system once         | Checked again if it has a `Check`, otherwise skipped |
| `Ensure`  | A setting that can drift between attempts | Applied again whenever its group is entered          |
| `Observe` | Reading the system, such as health checks | Run again whenever its group is entered              |

A step can also implement:

- **`Check(ctx, sc) (bool, error)`** — whether its work is already done. It
  runs before every `Run`, so a step whose work is done is skipped.
- **`Undo(ctx, sc) error`** — how to reverse its work, for `Rollback`.

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

The `StepContext` is the step's way into the run:

- `Info`, `Warn`, `Progress`, `Stdout` and `Stderr` report what it is doing.
- `Item` records a result for one target of a step that fans out, such as one
  host, and `PreviousItems` reads them back on a later attempt.
- `Record` stores a small value durably, and `Recorded` reads it from a later
  step, a later attempt or an `Undo`.
- `Custom` sends an event of your own to observers.

Steps are configured with `Retry`, `Timeout`, `Description` and `Labels`. A
step's `Run` can return `ErrSkipGroup` or `ErrSkipRun` to end its group, or the
run, early and as passed.

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

## Long-running pipelines

Give `Run` a file journal and the run survives its process:

```go
j, err := pipeline.OpenJournal("upgrade.jsonl")
if err != nil {
    return err
}
defer j.Close()

_, err = pipeline.Run(ctx, plan, j, pipeline.WithObservers(view))
```

Every outcome is written to the journal, and synced, before the run moves on.
If the journal can't be written, the run stops.

### Resuming

Run the same plan on the same journal and it resumes, doing only what is left:

- A group that finished and passed is skipped whole.
- An `Effect` step that finished has its `Check` run again, and its `Run` if the
  work is no longer done. Without a `Check` it is skipped.
- `Ensure` and `Observe` steps run again whenever their group is entered.
- A step that was interrupted, and whose `Check` now finds the work done, passes
  as recovered.

The journal holds the plan's fingerprint, and `Run` refuses a plan of a different
shape with `ErrPlanChanged`. Descriptions, labels, limits, timeouts and retries
can change between attempts, so a rebuilt tool can resume.

`WithInputs` records what the plan was built from (answers, flags, discovery
results), so a resume can read them back with `State.DecodeInputs` and build the
same plan without asking again. Fields of type `Secret` are never stored and
are masked wherever a step writes them. `WithRedactor` masks anything else.

### Stopping

- **`Stopper`** stops a run gracefully: running steps finish and nothing new
  starts. `HandleSignals` wires it to Ctrl-C: the first stops, the second
  cancels.
- **`RequestStop`** does the same from another process.
- **Cancelling the context** cancels the running steps.

A stopped, cancelled or crashed run is unfinished, and the next `Run` resumes it.

### Gates

A `Gate` waits for an approval before the run goes on:

```go
pipeline.Gate("continue-dc2", pipeline.GateOptions{
    Prompt:    "Upgrade datacenter DC2?",
    Approvers: []pipeline.Approver{pipeline.StdinApprover()},
    Timeout:   30 * time.Minute,
    OnTimeout: pipeline.TimeoutContinue,
})
```

`StdinApprover` asks on the terminal and `FileApprover` waits for a flag file.
`Approve` releases a gate from any process, so nothing has to wait on a
terminal. A gate released in an earlier attempt doesn't wait again.

### Watching from elsewhere

The journal is the record, so any process can read it while a run writes it:

- **`Follow`** delivers its events to observers as they are written, like
  `tail -f`. A viewer sees exactly what the runner's own observers saw.
- **`Inspect`** replays it into a `State`: every attempt, and where each element
  stands.

### Rolling back

`Rollback` undoes what the journal says was done, in reverse plan order, by
calling each step's `Undo`. Nobody writes the reverse plan. It is recorded as
an attempt of its own, and calling it again after a failure carries on where it
stopped.

## Observers

| Observer | Package                 | Output                                |
| -------- | ----------------------- | ------------------------------------- |
| Terminal | `observers/terminal`    | Styled boxes, colors, aligned columns |
| Plain    | `pipeline`              | Simple text lines                     |
| JSON     | `pipeline`              | JSON Lines, in the journal's format   |
| Custom   | `pipeline.ObserverFunc` | Any function                          |

The plain and JSON observers ship with the core module and add no dependencies.
The terminal observer is a separate module, so its styling dependencies stay
opt-in.

Observers are called in journal order, after each event is stored, on the
goroutine that wrote it. An observer that can block, such as one writing to a
socket, belongs behind a `NewAsyncObserver`. A panic in an observer is
recovered, and that observer is dropped.

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

## Upgrading from v0.2

v0.3.0 replaces the v0.2 API: `Pipeline`, `Stage` and `Executor` are now
`Plan`, `Group` and `Run`, steps are values with `Run` and optional `Check` and
`Undo`, and the `EmitX` helpers are `StepContext` methods. See
[DESIGN.md](./DESIGN.md) for why.
