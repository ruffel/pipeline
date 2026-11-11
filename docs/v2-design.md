# pipeline v2: durable runbooks for Go

Status: proposal, revision 2 (2026-11-11). This supersedes revision 1 and the earlier "design and improvement
plan" for `github.com/ruffel/pipeline`. Revision 2 folds in the review in `docs/v2-design-review.md`. §9 lists
what changed.

Baseline: `main` at `d13b76b` (2026-08-10). `go test -race ./...` passes on the core and the JSON, plain and
terminal observers.

"v2" is a working name for the redesign. It is not a `/v2` module path. It ships as `v0.3.0` (§5).

Progress (2026-11-11): step 0's fixes are merged (#85 A1 and B3, #86 A4 and B4, #87 B1), the minimum Go is
now 1.26 (#89), and the `v0.2.0` release is in progress (#90). The repo state described in §1 to §6 is the
`d13b76b` baseline.

## Open questions

Revision 2 assumes an answer to each of these. The text is marked **(open N)** where the answer lands, so a
different answer is a local edit. Rows marked **Decided** are settled.

| # | Question | Assumed here | Where |
| --- | --- | --- | --- |
| 1 | Release `main` as `v0.2.0` now, and the redesign as `v0.3.0`? | **Decided:** yes (#90) | §5, §7 |
| 2 | Build v2 in a subpackage beside v1 and flip at the end? | Yes, working name `runbook` | §7 |
| 3 | Does the settled-group rule fit what the hyperstore upgrade re-applies on every attempt? | Yes | §2.4 |
| 4 | Does `Check` run before `Run` on a first attempt too? | Yes | §2.4 |
| 5 | `OnFailure`: a hand-written step that resets the group, a derived undo, or both? | Hand-written now, derived with rollback | §2.7 |
| 6 | Gate timeout after a crash or stop: restart or continue? | Restart | §2.6 |
| 7 | When a child of a parallel group fails: let running siblings finish, or cancel them? | Let them finish | §2.1 |
| 8 | Recover observer panics? | Yes | §2.9 |
| 9 | Carry over `CustomEvent`, the debug level and message attributes? | No, dropped | §2.2, §5 |
| 10 | Fix B1 in `v0.2.0`? | **Decided:** yes (#87) | §6 |
| 11 | Rename the `Observe` step kind, which sits next to `Observer` the event sink? | Not renamed | §2.2 |

## 1. Why change it

pipeline today is an observable executor. It runs stages of steps and sends each change to observers. That's
useful, but on its own it doesn't sell itself: `errgroup` already runs work, and there are many ways to print
progress.

The gap in Go lies between a shell script and a workflow engine:

- Scripts and `errgroup` keep no record, can't resume after a failure, and can't roll back.
- Temporal, go-workflows and DBOS are durable, but they need a server or a database, and a deterministic-replay
  programming model.
- Ansible and Salt orchestration can't be embedded in a Go program or unit tested there.

v2 fills that gap. **Plan it, run it, kill it, resume it, roll it back, and watch it from anywhere, all from one
journal file, with no server.** The typical users are tools that operators run against real systems: installers,
upgrades, migrations, failovers, data repairs.

### Motivating consumers

These are the two consumers driving the design:

- **A cluster installer.** About 14 ordered steps from a topology file to a running cluster. Some steps fan out
  over nodes (one salt run across all of them). A failed run must resume, and there must be a rollback command.
- **A rolling upgrade.** It runs for hours, node by node, grouped region > datacenter > node, with an operator pause
  between datacenters that continues by itself after a configurable time. It has a point of no return, automatic
  recovery when a node fails, and must resume across a crash, a reboot or the tool replacing itself. Operators watch
  it from a terminal that may disconnect, and later perhaps from a web page.

### Requirements

| # | Requirement |
| --- | --- |
| R1 | The whole plan is known and printable before anything changes. |
| R2 | Each step's outcome is recorded durably before the run moves on. If the record can't be written, the run stops. |
| R3 | Resume after a failure, a stop, a crash or a reboot, carrying on from the first step that isn't done. |
| R4 | Steps that change the system once are skipped once done, after checking the live system. Steps that enforce a setting which can drift re-apply on every attempt that still has work in their group. Steps that only observe the system re-run on the same terms. A group that has finished is not entered again. |
| R5 | Values produced by one step (a backup path, a "committed" marker) are available to later steps, later attempts and rollback, from the moment they are recorded. |
| R6 | Nested structure (region > datacenter > node > step), with sequential and bounded-parallel groups. |
| R7 | Per-target results inside a step (one call fanning out over N hosts), which a re-run can read back. |
| R8 | A graceful stop (finish the running step, start nothing new), separate from cancelling. |
| R9 | Gates: wait for an approval from a terminal, a file or a command, with a timeout policy. Nothing waits on a terminal. |
| R10 | A rollback derived from what actually ran, with points of no return and recovery from a failed group. |
| R11 | Viewers in other processes (terminal, TUI, web, CI) that can attach late, and render the same state that resume uses. |
| R12 | Secrets never reach the journal or any observer. |
| R13 | Consumers can unit-test resume and rollback, including crashes at any point. |
| R14 | A run can be resumed by a rebuilt tool (a hotfix), and by a newer version of this library. |

## 2. The model

### 2.1 The plan is data

- A plan is a tree of **groups** and **steps**. A group is sequential, or parallel with a concurrency limit.
  Groups nest to any depth.
- A group has a **failure policy**:
  - By default, when a child fails the group starts no more children and lets the running ones finish. This is
    the same rule as a graceful stop (§2.5), applied to one group. **(open 7)**
  - `CancelOnError()` also cancels the running children. This is v1's behaviour.
  - `ContinueOnError()` runs every child and fails at the end. It is allowed on sequential groups too.
- Every element has a **path** (`upgrade/region1/DC2/node7/core-packages`) built from its name and its parents'
  names. The path is the element's stable identity in the journal. Names are unique among siblings.
- A **name** uses letters, digits, `.`, `_` and `-`, and starts with a letter or digit. Paths are used as file
  names (§2.5) and as command arguments, so `/` and `..` can't appear.
- Elements carry **labels** (`region`, `dc`, `node`) and a description. An element inherits its parents' labels,
  and its own value wins.
- The plan **doesn't change during a run**. Anything dynamic is worked out while building it. If its shape would
  depend on something only discoverable by running code, the consumer runs a read-only discovery phase first and
  feeds the result into the plan. The discovery result is an input (below), so a resume builds the same plan even
  if the system has changed since.
- The plan can be **serialised without its functions**: names, paths, kinds, labels, descriptions, group
  settings, and for each step whether it has a `Check`, whether it has an `Undo`, and whether it is Irreversible.
  It is stored in the journal, printed before the run (R1), and rendered by viewers. `Describe` prints those
  capabilities, so the printout says what a rollback can and can't undo before anything changes.
- The plan has a **fingerprint**: a hash of its identity only. That is the ordered paths, each element's kind, and
  the Irreversible flags. Descriptions, labels, limits, timeouts, retry settings and gate options are left out, so
  a rebuilt tool that changed only those can resume (R14).
- The consumer declares the plan's **inputs**: the answers, flags, defaults and discovery results the plan was
  built from, each with its origin. They are stored in the journal with their own digest, so a resume reuses them
  instead of asking again. Secret inputs are the one exception (§2.3).
- A plan contains the steps that **might** run. Whether each one does something is decided at run time (by `Check`,
  §2.2) and recorded as the outcome.
- `NewPlan` **validates** the tree and returns an error. It rejects: a bad or duplicate name; an Observe step with
  a `Check` or an `Undo`; an Irreversible step with an `Undo`; a negative limit; an `OnFailure` step whose name
  clashes with one of the group's children.

### 2.2 Steps

A step is a value implementing `Run`, and optionally `Check` and `Undo`:

```go
type Step interface {
    Run(ctx context.Context, s *StepContext) error
}
type Checker interface { // optional
    Check(ctx context.Context, s *StepContext) (done bool, err error)
}
type Undoer interface { // optional
    Undo(ctx context.Context, s *StepContext) error
}
```

`pipeline.StepFunc` adapts a plain function, for steps that need neither `Check` nor `Undo`.

There are three kinds of step, chosen when the plan is built:

- **Effect** (`pipeline.Effect(name, step)`) changes the system. Once its outcome is recorded as passed, it is
  skipped, or verified with `Check`, on later attempts.
- **Ensure** (`pipeline.Ensure(name, step)`) changes the system, but is never trusted from the journal, because
  what it sets can drift between attempts (a service disabled, a maintenance mode, a configuration pushed). It
  re-applies on every attempt in which its group is entered (§2.4). It can have an `Undo`, and rollback treats it
  like an Effect step. This kind came out of the audit of the motivating rolling upgrade, whose correctness relies
  on re-applying such settings.
- **Observe** (`pipeline.Observe(name, step)`) only reads the system (pre-checks, health checks). It runs on every
  attempt in which its group is entered, because its result goes stale. It has no `Check` and no `Undo`.
  **(open 11)**

Per-step settings:

- `Retry(n, backoff)` allows up to `n` **tries** within one attempt. A try is `Check` and then `Run`, so a `Run`
  that failed after doing the work passes on the next try. "Attempt" always means a run of the plan, never a try.
- `Timeout(d)` bounds each try, and bounds `Undo`.
- `Irreversible()` marks a point of no return (§2.7).

`StepContext` gives a step:

- `Progress(key, msg, cur, total)`, `Info`, `Warn`, and `Stdout()`/`Stderr()` writers for subprocess output.
  v1's custom events, debug level and message attributes are dropped. **(open 9)**
- `Item(id, status, msg)`: a result per target (R7). The status is `passed`, `failed` or `skipped`.
  `PreviousItems()` returns the results recorded for this step by earlier attempts, so a re-run can retry only the
  failed or unfinished targets.
- `Record(key, value) error`: a small JSON value, durable by the time the call returns (R5).
  `Recorded(path, key, &v) (found bool, err error)` reads the latest value recorded under that path and key by any
  step, in this or an earlier attempt, whether or not that step finished. Values must not be secret (R12). There
  is a size limit per step. `Record` returns an error when the value can't be encoded or the limit is reached.
- `Inputs(&v)`, `Attempt()`, `Path()`, `Labels()`.

A `StepContext` stops working when its step returns. Later calls do nothing, and `Record` returns an error. (In
v1 an emitter outlives its step, B5 in §6.)

The contracts, documented and enforced by `pipelinetest` (§2.10):

- `Run` and `Undo` must be idempotent. `Run` may be repeated after a crash part-way through it.
- `Undo` must cope with a `Run` that was interrupted part-way. It can read whatever that `Run` recorded before it
  stopped.
- `Undo` undoes only what `Run` changed. If `Run` finds the state already in place, it records that, and `Undo`
  leaves it alone. The engine never calls `Undo` for a step whose `Run` it never called (§2.7).
- `Check` is read-only. It may fail, and an error from `Check` fails the try, exactly like an error from `Run`.
- A wrapper around a step must forward `Check` and `Undo`, or the engine can't see them. `Describe` shows what the
  engine found.

`Record` being durable at once gives steps the idiom that removes most crash hazards inside a step: record the
intent or the previous state before the change, and the result after it.

### 2.3 The journal is the source of truth

- A journal is an **append-only JSON Lines file**, one per operation (for example `<staging>/upgrade.journal`),
  created with mode `0600`. `OpenJournal` takes an exclusive `flock` on it. A second writer gets `ErrJournalBusy`,
  with the PID from the holder's `run.started` record. (`flock` itself can't say who holds it.)
- **Durable records** (R2) are written and fsynced **before** any observer is told. Every record about the
  lifecycle of a run is durable:
  - a run starting, with the plan, its fingerprint, the inputs and their digest, the attempt number, the run ID,
    the mode (run or rollback), run labels, PID and host
  - a group starting, finishing, and being reset (§2.7)
  - a step starting, its `Run` being called, and the step finishing (passed, skipped or failed, with a reason or
    an error)
  - a recorded value
  - per-target results
  - a gate waiting, and a gate released (by what)
  - an undo starting and finishing
  - a stop requested
  - a run finishing (passed, failed, stopped, cancelled)

  If a write fails, the run stops with that error. It never carries on unrecorded.
- **Best-effort records** (progress, info and warnings, output lines, retries) go into the same file **without**
  fsync. A crash can lose the last of these, never a durable record.
- Every line carries `v` (the schema version), `seq` (a sequence number that only ever increases within the file),
  `ts` (UTC wall-clock time) and `kind`.
- **Schema versions.** A reader accepts every version up to its own, and refuses a newer one. A file may mix
  versions, because a newer library can resume a journal an older one started (R14). Unknown fields are ignored.
  A new durable kind needs a new version.
- **Fixed vocabularies.** Viewers switch on these, so they are closed lists:
  - step and group outcome: `passed`, `skipped`, `failed`
  - run outcome: `passed`, `failed`, `stopped`, `cancelled`
  - undo outcome: `undone`, `skipped`, `failed`
  - reason: `done` (`Check` said so), `earlier-attempt` (trusted from the journal), `recovered`, `settled`,
    `group-skipped`, `run-skipped`, `no-undo`
  - error kind: `error`, `cancelled`, `stopped`, `timeout`, `panic`, `declined`
  - item status: `passed`, `failed`, `skipped`
- **Errors are data.** A record holds an error as its kind, its message and, for a panic, the stack. In-process
  observers receive the same decoded record that went to the journal, not the live error, so an observer behaves
  the same in the runner and in a viewer (R11). The error returned by `Run` is the live one.
- **Opening after a crash.** A crash can leave half a line at the end. `OpenJournal` truncates the file to its last
  complete line while holding the lock, and continues `seq` from there. An invalid line anywhere else is
  `ErrJournalCorrupt`. Creating a journal also fsyncs its directory.
- **Size.** Output is the bulk of a journal, so it is bounded where it is written:
  - An output line longer than the cap (proposed: 16 KiB) is split, and the pieces are marked as continuations.
    `\r` ends a line as `\n` does, so progress output doesn't build one endless line (B3 in §6).
  - Each step has an output budget (proposed: 16 MiB). Past it, lines are counted and dropped, and a warning
    record says how many.
  - Readers never assume a maximum line length.
- **The state is computed by replaying the journal.** `Inspect(path)` reads it without a lock and returns:
  - the plan and inputs, every attempt, each step's latest state and recorded values, and item results
  - whether a run is unfinished (started and not finished, or stopped)
  - whether a runner is **live**. `Inspect` probes the lock for this, because replay alone can't tell "running"
    from "crashed". `OpenJournal` retries briefly, so a probe never causes a false `ErrJournalBusy`.
  - where the run stands on each Irreversible step: not reached, in doubt, or passed (§2.7)

  Resume, rollback, `status` and every viewer use this one replay, so they can't disagree (R11).
- **Redaction** (R12): `WithRedactor(func(string) string)` runs on every string before it is written to the
  journal or given to an observer. The error returned by `Run` wraps the original, so `errors.Is` and `errors.As`
  still work on it. Recorded values are not redacted; they must simply never be secret.
- **Secret inputs** (R12). An input field of type `pipeline.Secret` is never written to the journal, in any form,
  and is left out of the inputs digest. It comes back empty from `Inspect`, and the consumer supplies it again on
  every attempt. The engine also masks every literal occurrence of an input secret in the strings it writes.
- The journal is a concrete type. `MemoryJournal()` gives the same behaviour without a file, so small tools and
  tests can use v2 without any of the durability.
- The file journal is Unix only. The package compiles on Windows, where `OpenJournal` returns an error.

### 2.4 Running and resuming

`Run(ctx, plan, journal, opts...)` starts a run, or resumes one if the journal already has attempts. It walks the
plan in order and decides in two stages: first per group, then per step.

**Groups.** A group is **settled** when it has finished and passed in some attempt, and nothing since has reset it
(§2.7) or undone one of its steps. A settled group is skipped as a unit: none of its steps run, and none of their
`Check`s. A group with no Effect, Ensure or Gate step beneath it never settles. **(open 3)**

The reason is steps that reverse each other. A node group is `stop`, `packages`, `apply`, `start`, `verify`. Once
it has finished, `stop`'s `Check` reports "not stopped", because `start` ran afterwards. Deciding step by step
would run `stop` again, and then `start`, on every finished node at every resume. The consequences of the rule:

- A finished node group is not entered again. Its Ensure steps don't re-apply and its health checks don't re-run.
- A group of only Observe steps (prechecks) is entered on every attempt, as intended.
- A top-level Ensure step re-applies on every attempt, because the root is not settled until the run is.
- The modelling rule for consumers: steps that reverse each other belong in the same group. A check that must be
  fresh belongs where the work still is, for example a cluster health check at the start of each node group.

**Steps.** Inside a group that is entered, each step is decided from its latest outcome in the journal:

| Journal state for the step | Effect | Ensure | Observe | Gate |
| --- | --- | --- | --- | --- |
| Passed, or skipped as done | With a `Check` under `ResumeVerify` (the default): `Check`, and `Run` only if it isn't done. With no `Check`, or under `ResumeTrust`: skip | `Check` if it has one, and `Run` if it isn't done. With no `Check`: `Run` | `Run` | Skip |
| Anything else (never started, interrupted, failed) | `Check` if it has one, and `Run` if it isn't done. With no `Check`: `Run` | The same | `Run` | Wait |

- A step with a `Check` is always checked before `Run`, on a first attempt too. **(open 4)** When `Check` reports
  done for a step whose `Run` was never called, the step is recorded as skipped, with reason `done`. When `Run`
  had been called and was interrupted or failed, it is recorded as passed, with reason `recovered`.
- The engine writes a durable record between `Check` and `Run` (`step.running`). It is how rollback knows whose
  `Run` was called (§2.7).
- A failed step runs again with `PreviousItems()` available.

`ResumeVerify` is the default because the audit of the motivating installer found several "already done" guards that
were wrong. Some were set before the work ran, some were held in memory, and some couldn't tell an error from
"absent". Trusting the journal alone would carry that class of bug forward. `ResumeVerify` applies inside groups
that are still in progress; a settled group is trusted.

**Flow control.** A step can return `ErrSkipGroup` or `ErrSkipRun`.

- The step itself passes, and its record carries the signal.
- The children that have not started are recorded as skipped, with reason `group-skipped` or `run-skipped`. In a
  parallel group the running ones finish.
- A run ended by `ErrSkipRun` counts as passed.
- When a step that signalled is skipped on a later attempt, the signal is replayed from its record. Otherwise the
  siblings it bypassed would run.

Whole-run rules:

- If the plan's fingerprint differs from the journal's, the run is refused (`ErrPlanChanged`), unless
  `WithPlanChange(MatchByPath)` is given. Then steps are matched by path, new steps run, and journal steps with no
  plan element are reported.
- If the inputs differ from the journal's, the run is refused (`ErrInputsChanged`), unless `WithNewInputs()` is
  given. The new inputs are then recorded with the attempt.
- If the journal's last run passed, the run is refused (`ErrAlreadyComplete`), unless `Force()`. A forced run is
  recorded as such. It treats the journal as empty when deciding what runs: nothing is settled, nothing is
  trusted, and gates wait again. `Check` still decides whether `Run` is needed, and recorded values stay readable.
- If the journal has an unfinished rollback, the run is refused until the rollback finishes.

A resume needs the stored inputs before it can build the plan. The consumer calls `Inspect` first, takes the
inputs from it if there are any, asks its questions otherwise, and then builds the plan (§3).

A run started on a fresh journal checks nothing first. The consumer uses `Inspect` beforehand when it needs to,
for example to refuse an unrelated operation while another one is unfinished (§2.5).

### 2.5 Stopping, cancelling and crashes

| Event | Behaviour |
| --- | --- |
| Graceful stop: `Stopper.Stop()`, or `RequestStop(ctx, path, false)` from another process | No new step starts. Running steps finish, and their outcomes are recorded. Parallel steps still queued are not started. A waiting gate stops waiting and stays unreleased. A step waiting between tries is recorded as failed with its last error. The run finishes with outcome `stopped`, and `Run` returns `ErrStopped`. |
| Cancel: the context, or `RequestStop(ctx, path, true)` | Running steps' contexts are cancelled. Their outcomes are recorded as failed, with error kind `cancelled`. Outcome `cancelled`. |
| The journal can't be written | Running steps' contexts are cancelled and `Run` returns the write error. Nothing more is recorded, so a later replay shows those steps as interrupted. |
| Crash, kill, reboot | Nothing more is written. A later replay shows the step started and never finished, and resume handles it (§2.4). The kernel releases the `flock`. |

**Control from another process** works through files in `<journal>.control/`: `stop`, `cancel`, and
`approve/<gate path>`. The runner creates the directory with mode `0700` and polls it. `RequestStop` and `Approve`
write the files, so a CLI subcommand, a viewer or a web backend can stop or approve without a socket.

- Each request names the run it is for (the run ID from the latest `run.started`) and who is asking. It is written
  under a temporary name and renamed into place.
- The runner acts on a request for its own run, records it, and deletes the file. It deletes a request for any
  other run without acting. A file left behind by a dead runner therefore can't stop or approve a later attempt.
- `RequestStop` and `Approve` return when the journal shows the request was recorded. If a new attempt starts
  first (a re-exec), they issue the request again for that attempt. `RequestStop` returns `ErrNotRunning` when no
  runner is live. `Approve` returns `ErrGateNotWaiting` unless the journal shows that gate waiting.
- Whoever can write to the directory can stop and approve. That is the access control. The name in a request is
  self-declared; the record also carries the UID that owns the file.

A stopped run still counts as **unfinished**: `Inspect(...).Unfinished` is true. Consumers must check this before
starting other operations that would conflict. A stopped upgrade has left a half-upgraded cluster, and the library
can't know which other operations conflict with that.

`HandleSignals(ctx, stopper, cancel, stopSigs, detachSigs)` is an explicit helper, never installed automatically:

- The first signal in `stopSigs` stops gracefully, the second cancels.
- A signal in `detachSigs` (typically SIGHUP) is only logged. After it the terminal is gone: writes to it fail, and
  reads return an error or EOF (§2.6).
- SIGPIPE is handled, so a write to a closed stdout or stderr returns `EPIPE` instead of killing the process (Go's
  default for file descriptors 1 and 2).

### 2.6 Gates

```go
pipeline.Gate("continue-DC2", pipeline.GateOptions{
    Prompt:    "Upgrade datacenter DC2?",
    Approvers: []pipeline.Approver{pipeline.ControlApprover(), pipeline.FileApprover(legacyPath, time.Second)},
    Timeout:   6 * time.Hour,
    OnTimeout: pipeline.Continue, // or Decline (fails the run) or Stop (graceful stop)
})
```

- A gate is a step that waits. The first approver to answer wins. `ControlApprover` (the control files from §2.5)
  is always included.
- `StdinApprover` exists for interactive tools, but only works when the runner has a terminal. Detached runners use
  the control files, and an attached viewer translates a key press into `Approve`.
- An approver that can no longer answer **withdraws**; it does not decline. `StdinApprover` withdraws on EOF or a
  read error, which is what a lost terminal produces. (v1's runbook example treats that as "declined".) A panic in
  an approver is recovered, and that approver withdraws.
- The journal records the gate waiting, with its deadline, and how it was released: approved, by whom or which
  source, or timed out with the policy applied. A resumed run that reaches a gate already released doesn't wait
  again.
- A stop interrupts the wait (§2.5). The gate stays unreleased, and a resume waits again.
- The timeout **restarts** when a run resumes at a gate that was waiting, so a run never continues on its own
  sooner than configured after a restart. **(open 6)** The wait itself is timed on the monotonic clock, so a clock
  step during an install doesn't release a gate.

### 2.7 Rollback

Rollback is **derived**: nobody writes a reverse pipeline by hand. `Rollback(ctx, plan, journal, opts...)`:

1. Replays the journal and takes every Effect and Ensure step whose `Run` was called and which has not been undone
   since. It doesn't matter whether that `Run` passed, failed or was interrupted. A step that was skipped because
   `Check` found the state already in place is not taken: the run didn't make that change.
2. Refuses if the run may be past a point of no return:
   - `ErrIrreversible` if any `Irreversible()` step has passed.
   - `ErrIrreversibleInDoubt` if the `Run` of one was called and did not pass. Its `Check`, when it has one,
     settles the doubt. Without a `Check`, the caller has to pass `AssumeNotCommitted(path)` to go on.
3. Walks the plan in **reverse plan order** and calls `Undo` on each of those steps that has one. Steps without
   `Undo` are skipped and reported. Plan order is a valid dependency order by construction, and it stays
   well-defined across attempts and for parallel groups, where "when it started" is not. Undo runs one step at a
   time.
4. Is recorded in the same journal as a rollback run, so `Inspect` shows it, and running `Rollback` again after a
   failure carries on where it stopped.

A finished rollback returns every step it undid to "never started", and clears the values those steps recorded.
Gates are unreleased and groups are no longer settled. A step it could not undo keeps its outcome. `Run` is
allowed again after that.

Under `MatchByPath`, a journal step with no plan element has no `Undo` to call. If its `Run` was called, rollback
refuses with `ErrOrphanedSteps` unless `SkipOrphans()` is given.

How fine-grained rollback is depends on where `Undo` lives:

- **On each step**, for steps with a natural inverse.
- **On a restore point**: a backup or snapshot step whose `Undo` restores everything done since. The steps after it
  have no `Undo`. A coarse "restore from backup" rollback is expressed this way.
- **Nowhere**, behind an `Irreversible` step.

**`OnFailure` is separate.** `Group(...).OnFailure(step)` runs a recovery step automatically when the group fails,
during the original run. An example is recovering one node after its upgrade fails. It is recorded in the journal,
and its outcome is reported alongside the failure. It isn't part of `Rollback`. **(open 5)**

- The handler is an Effect step with a path of its own under the group.
- When the handler passes, the engine records a **reset** of the group. Its children go back to "never started",
  which also takes them out of what a later rollback undoes, and their recorded values are cleared. Without this
  the journal would still say `packages` passed on a node the handler had just put back on the old version, and a
  resume would skip it.
- The group still counts as failed. The run fails or continues according to the parent's failure policy.
- The handler does not run once a stop or cancel has been requested. A handler is a step, and no step starts
  after that. The group is left failed, and the next resume enters it again.
- A group whose handler started and has not passed owes a recovery. A resume runs the handler before it runs the
  group's children.

A derived form, `Group(...).UndoOnFailure()`, would run the group's own `Undo`s in reverse plan order: the same
mechanism as `Rollback`, scoped to one group. It arrives with rollback (§7), because it needs `Undo`.

### 2.8 How the tool interacts with people

The library supports three kinds of interaction, and deliberately nothing else:

- **Questions** are answered before the run and become plan inputs, stored with their origin. They are never asked
  during a run, and never asked again on resume. The exception is a secret, which is never stored and so has to be
  supplied on every attempt (§2.3).
- **Approvals** during a run are gates (§2.6). They carry no answer, just "go", and work without a terminal.
- **Decisions after a failure** ("roll back or retry?") are not prompts. The run ends with an outcome and the
  operator runs `Rollback`, or runs `Run` again to resume.

### 2.9 Watching

- **An event is the journal record, decoded.** In-process observers (the terminal, plain and JSON observers,
  ported) receive it after the durable write. `Follow` delivers the same values. Events carry `seq`, `ts`, `path`,
  `labels`, and item results.
- **A panic in an observer is recovered.** That observer is dropped for the rest of the run and a warning is
  recorded. **(open 8)** v1 left these fatal because there was nowhere to report one. Here the journal is the
  record and an observer is a view of it, so a rendering bug three hours in should not interrupt an upgrade. The
  observer call sits outside the step's own recover, so an observer panic is never reported as a step failure (B2
  in §6).
- `AsyncObserver` has a delivery policy for each kind of event:
  - Lifecycle events are never dropped. Their queue is unbounded; the size of the plan bounds it in practice.
  - Progress keeps only the latest value for each key.
  - Output and messages drop the oldest, and say how many were dropped.

  Anything that writes to a terminal, socket or pipe must go behind it. `emit` calls observers synchronously, so a
  blocked writer would stall every step. `Close` takes a context, so a writer that never returns can't keep the
  process alive after the run.
- **Out-of-process viewers** call `Follow(ctx, path, opts, observers...)`. It replays the journal and then follows
  it like `tail -f`, by polling, and feeds the same observers.
  - `opts.From` is the `seq` to start from.
  - `opts.History` chooses what is replayed from before the call: everything, or lifecycle records only. A viewer
    that attaches late wants the second, so it gets the plan and every step's state without hours of old output.
  - `Follow` returns when the run has finished and no runner is live. With `opts.KeepFollowing` it waits for the
    next attempt.
  - If the run is unfinished and no runner is live, `Follow` delivers what there is and returns `ErrRunnerGone`.
    Without this a viewer attached to a crashed run waits forever.

  A terminal attached from a new session, a TUI and a web backend therefore render exactly what the runner sees.
- The terminal observer renders nested groups as a tree, with item results as rows under their step.
- **Console compatibility is the consumer's job.** A consumer replacing a tool whose console text is asserted by
  tests or parsed by other tools writes its own observer to reproduce those lines. The library doesn't try to
  produce anyone's legacy format.

### 2.10 Testing support (`pipelinetest`)

The harness is built with the engine (§7), because every later phase is tested with it. The engine takes its
clock as an option for the same reason.

- An in-memory journal and a fake clock.
- `CrashAfter(path, point)` simulates a crash after a given step reaches a given point (started, finished, or a
  given item). From that record on, the journal accepts nothing and the run ends as it does when the journal can't
  be written (§2.5). The test then resumes from the same journal and asserts the outcome.
- `CrashBetween(path, a, b)` simulates a crash inside a step, between two of its own `Record` calls or item
  results. Many real hazards sit inside a step, not between steps: a table dropped before its re-import, or a
  "done" flag written before the work.
- Helpers that assert the journal's sequence of records and the replayed state.
- `NewStepContext` builds a `StepContext` for testing one step on its own, and captures what it records.
- A contract check, which runs each step's `Run` twice, and its `Undo` after a `Run` interrupted at each of its
  `StepContext` calls, to catch steps that aren't idempotent.

The library's own tests add one thing the simulation can't cover: a test that kills a real runner process with
`SIGKILL` and resumes from the file journal. It exercises the lock release, the half-written last line and the
fsync ordering together.

### 2.11 The runner/viewer pattern (documentation and example)

Add a README section, "Long-running pipelines", and an example, `examples/detached`:

- A runner process with no terminal opens the journal and runs the plan.
- `watch` is a separate command that `Follow`s the journal.
- `stop` and `approve` write control files.

The example shows that killing the viewer doesn't affect the run, that a viewer started late sees everything, and
that a viewer reports it when the runner has died. The README also documents Go's signal defaults (§2.5). How the
runner is launched (a systemd unit, a supervisor) is up to the consumer.

### 2.12 Non-goals

- **Deterministic replay of code.** Steps are idempotent units, and the journal records outcomes, not instructions.
- **Distributed workers.** Remote work is the step's job (ssh, salt, an API).
- **Databases.** The journal has a file form and a memory form, and nothing else is planned.
- **Changing the plan mid-run, or loops.**
- **Launching or daemonising processes, serving HTTP, authentication.** The name on a stop or an approval is
  whatever the requester says it is.
- **Locks across different operations.** The journal lock covers only its own journal.
- **A file journal on Windows.**

## 3. API sketch

```go
func buildPlan(in Inputs) (*pipeline.Plan, error) {
    return pipeline.NewPlan("upgrade", in, // the inputs are declared with the plan, and validated with it
        pipeline.Group("prechecks", checks...).Parallel(8).ContinueOnError(),
        pipeline.Effect("backup", backup{}),                     // Undo restores the backup
        pipeline.Effect("commit-schema", commit{}).Irreversible(),
        pipeline.Group("region1",
            pipeline.Effect("schema", schema{}),
            pipeline.Group("DC1", nodeGroups(in, "DC1")...).Labels(pipeline.L{"dc": "DC1"}),
            pipeline.Gate("continue-DC2", gateOpts),
            pipeline.Group("DC2", nodeGroups(in, "DC2")...).Labels(pipeline.L{"dc": "DC2"}),
        ),
    )
}

func nodeGroup(n Node) pipeline.Element {
    return pipeline.Group(n.Name,
        pipeline.Observe("cluster-healthy", clusterHealth{}), // fresh before each node is touched
        pipeline.Effect("stop", stop{n}),
        pipeline.Effect("packages", packages{n}),
        pipeline.Effect("apply", apply{n}),
        pipeline.Effect("start", start{n}),
        pipeline.Observe("verify", verify{n}),
    ).Labels(pipeline.L{"node": n.Name}).OnFailure(pipeline.Effect("recover", recoverNode{n}))
}

type Inputs struct {
    Nodes         []Node          // discovery output: reused on resume, not rediscovered
    TargetVersion string
    AdminPassword pipeline.Secret // never stored; supplied on every attempt
}

// First run or resume: the stored inputs decide.
st, err := pipeline.Inspect(path) // lock-free; a missing journal is an empty state
var in Inputs
if st.HasRun() {
    err = st.Inputs(&in) // secrets come back empty
} else {
    in = askQuestionsAndDiscover()
}
in.AdminPassword = readSecret()
plan, err := buildPlan(in)

j, err := pipeline.OpenJournal(path) // exclusive; ErrJournalBusy
defer j.Close()

fmt.Print(pipeline.Describe(plan)) // R1: everything that may run, and what can be undone

term := pipeline.NewAsyncObserver(termObserver, pipeline.DefaultPolicies)
defer term.Close(closeCtx)

res, err := pipeline.Run(ctx, plan, j,
    pipeline.WithObservers(fileLog, term),
    pipeline.WithStopper(stopper),
    pipeline.WithRedactor(redact),
    pipeline.WithResume(pipeline.ResumeVerify), // the default; shown for clarity
    pipeline.WithRunLabels(pipeline.L{"operation": opID}),
)
// errors.Is(err, pipeline.ErrStopped), ErrPlanChanged, ErrInputsChanged, ErrAlreadyComplete; res.Outcome

// From other processes:
st, _ = pipeline.Inspect(path)             // st.Live, st.Unfinished, st.Irreversible
_ = pipeline.RequestStop(ctx, path, false) // returns once the runner has recorded it
_ = pipeline.Approve(ctx, path, "upgrade/region1/continue-DC2", "alice")
err = pipeline.Follow(ctx, path, pipeline.FollowOptions{History: pipeline.LifecycleOnly}, termObserver)
// errors.Is(err, pipeline.ErrRunnerGone)

res, err = pipeline.Rollback(ctx, plan, j) // derived; ErrIrreversible, ErrIrreversibleInDoubt
```

Inside a step:

```go
func (p packages) Run(ctx context.Context, s *pipeline.StepContext) error {
    prev, err := p.node.InstalledVersion(ctx)
    if err != nil {
        return err
    }
    if err := s.Record("previous-version", prev); err != nil { // durable before anything changes
        return err
    }
    return p.node.Install(ctx, p.target, s.Stdout())
}

func (p packages) Undo(ctx context.Context, s *pipeline.StepContext) error {
    var prev string
    if ok, err := s.Recorded(s.Path(), "previous-version", &prev); err != nil || !ok {
        return err // nothing recorded: Run never got as far as changing anything
    }
    return p.node.Install(ctx, prev, s.Stdout())
}
```

## 4. Journal format (illustrative)

```text
{"v":1,"seq":1,"ts":"2026-11-11T09:00:00.000Z","kind":"run.started","run":"r3","attempt":3,"mode":"run","op":"upgrade","pid":4121,"host":"admin1","fingerprint":"sha256:…","plan":{…},"inputs":{…},"inputs_digest":"sha256:…","labels":{"operation":"u-42"}}
{"v":1,"seq":2,"ts":"…","kind":"group.started","path":"upgrade/prechecks"}
{"v":1,"seq":3,"ts":"…","kind":"step.started","path":"upgrade/prechecks/ring"}
{"v":1,"seq":4,"ts":"…","kind":"step.running","path":"upgrade/prechecks/ring"}
{"v":1,"seq":5,"ts":"…","kind":"output","path":"upgrade/prechecks/ring","stream":"stdout","line":"UN 10.0.0.1 …"}
{"v":1,"seq":6,"ts":"…","kind":"step.finished","path":"upgrade/prechecks/ring","outcome":"passed","ms":1840}
{"v":1,"seq":7,"ts":"…","kind":"group.finished","path":"upgrade/prechecks","outcome":"passed","ms":1903}
{"v":1,"seq":8,"ts":"…","kind":"step.finished","path":"upgrade/backup","outcome":"skipped","reason":"earlier-attempt"}
{"v":1,"seq":9,"ts":"…","kind":"group.finished","path":"upgrade/region1/DC1","outcome":"skipped","reason":"settled"}
{"v":1,"seq":14,"ts":"…","kind":"value","path":"upgrade/region1/DC2/node7/packages","key":"previous-version","value":"8.1.2"}
{"v":1,"seq":17,"ts":"…","kind":"item","path":"upgrade/stage-packages","item":"node7","status":"failed","message":"…"}
{"v":1,"seq":21,"ts":"…","kind":"step.finished","path":"upgrade/region1/DC2/node7/apply","outcome":"failed","error":{"kind":"timeout","message":"…"},"ms":600000}
{"v":1,"seq":25,"ts":"…","kind":"group.reset","path":"upgrade/region1/DC2/node7","by":"upgrade/region1/DC2/node7/recover"}
{"v":1,"seq":30,"ts":"…","kind":"gate.released","path":"upgrade/region1/continue-DC2","by":"timeout","policy":"continue"}
{"v":1,"seq":31,"ts":"…","kind":"stop.requested","mode":"graceful","by":"control","who":"alice","uid":1000}
{"v":1,"seq":40,"ts":"…","kind":"run.finished","outcome":"stopped"}
```

The `run.*`, `group.*`, `step.started`, `step.running`, `step.finished`, `undo.*`, `value`, `item`, `gate.*` and
`stop.*` kinds are fsynced. The other kinds (`output`, `progress`, `message`, `step.retry`) are best-effort.

A gate writes `step.started`, `gate.waiting`, `gate.released` and `step.finished`, so every element has the same
start and finish records whatever its kind.

## 5. What changes from v1

| v1 | v2 |
| --- | --- |
| `Pipeline` > `Stage` > `Step`, two fixed levels | `Plan` > nested `Group` > `Step` |
| `Location{Pipeline, Stage, Step}` | `Path` (a list of names) plus `Labels` |
| `StepFn func(ctx) error` | `Step` value with `Run`, optional `Check` and `Undo`; `Effect`, `Ensure` or `Observe` |
| `Condition` (skip with a reason, can't fail) | Removed. Decisions made when building the plan leave the step out; run-time "already done" is `Check` |
| `ErrSkipStage`, `ErrSkipPipeline` | `ErrSkipGroup` and `ErrSkipRun`, recorded as outcomes in the journal. A run ended by `ErrSkipRun` counts as passed |
| `EmitX(ctx, …)` helpers | `StepContext` methods (`Progress`, `Item`, `Record` …) |
| `EmitCustom`, the debug level, message attributes | Dropped **(open 9)** |
| `WithRunID`, `RunIDFrom` | Run ID and run labels in `run.started` and on every event |
| Events are the only record | The journal is the record. An event is a decoded record, carries `seq` and `ts`, and can be replayed with `Follow` |
| Event errors are live `error` values | Errors are data: kind, message, stack |
| Observers: terminal, plain, JSON | Ported to paths, labels and items. JSON writes the journal format. Async policies per kind of event |
| An observer panic is fatal (in practice, sometimes a step failure: B2) | Recovered; the observer is dropped **(open 8)** |
| A failing step in a parallel stage cancels its siblings | Running siblings finish; cancelling is an option **(open 7)** |
| `WithRetry`, `WithTimeout` | Per-step settings |
| No resume, rollback, stop or gates | §2.4 to §2.7 |

The API breaks, which is fine before 1.0. **(open 1)** `v0.2.0` is already spoken for: `main` carries the feature
set agreed for it in August (`MaxParallel`, `Description`, run ID, skip events for `ErrSkipStage`), unreleased.
So `v0.2.0` is `main` plus the Phase 0 fixes, and v2 is released as `v0.3.0`. A `/v2` module path isn't needed
before 1.0.

## 6. v1 bugs (confirmed against `d13b76b`)

Each was reproduced by the tests in the appendix. A1 to A7 are from revision 1. B1 to B6 were found in the
review.

**Fix in v1 now**, and release as `v0.2.0`:

- **A1 (high): `OutputWriter` kills the subprocess when a line is over 1 MiB.** `exec.Cmd` returned
  `signal: broken pipe`, and no output after that line arrived. Cause: on `bufio.ErrTooLong` the read side of the
  pipe is closed. Fix: never stop draining. Split, or truncate with a marker, any line over the limit, and warn once
  per such line. This code carries over into v2, behind `StepContext.Stdout()`.
- **B3: A1 triggers on ordinary progress output.** Tools that redraw a line with `\r` (curl, rsync, pip) never
  write a newline, so their progress builds one line until it passes the limit. Fix, with A1: end a line at `\r`
  as well as `\n`.
- **A4 (medium): one value the JSON observer can't serialise stops all further logging.** An unsupported value in a
  `CustomEvent` stops it writing the rest of the run, and `Err()` reports the marshal error. Fix: when marshalling
  fails, write the envelope with `marshalError` in place of the bad value. Only failures of the writer itself set
  `Err()`. The observer's code doesn't carry over, but the requirement does: in v2 it is why `Record` returns an
  error.
- **B4: A4 triggers on `NaN`**, an ordinary float, as well as on a func value. Same fix.
- **B1 (high): a panic in a `Condition` inside a parallel stage kills the process.** It happens on one of the
  executor's goroutines, so the caller can't recover it, and `Run` can return `nil` before the process dies. Fix:
  install the step's recover before calling `Condition` (`executor.go:263`). **(open 10)**

**Acceptance tests for the v2 executor**, which is being rewritten anyway:

- **A2: a panic in a `Condition` escapes `Run`.** In v2, a panic in `Check`, `Run`, `Undo` or an approver fails
  that step, and is recorded. Nothing escapes `Run`, and nothing kills the process from a parallel group (B1).
- **A3: with fail-fast and `MaxParallel`, steps still queued start after a sibling has failed.** In v2 they aren't
  started, and the replayed state shows them as not reached.
- **B6: with `ContinueOnError` and `MaxParallel`, steps still queued start after a cancel.** Same rule as A3.
- **A5: the error from a panic has no location and no type.** In v2 it is a `PanicError{Value, Stack}`, wrapped with
  the step's path, and the stack is recorded separately from the message.
- **A6: a `Condition` can emit events before its step starts.** This goes away in v2: `Check` runs after
  `step.started`.
- **A7: steps not reached after a failure or a cancel get no events.** In v2, replaying the plan and the journal
  shows every step as passed, skipped, failed or not reached.
- **B2: an observer panic is recovered as a step failure.** One step gets `StepPassed` and then `StepFailed`. In
  v2 the observer call is outside the step's recover (§2.9).
- **B5: an emitter outlives its step**, so events arrive after `StepPassed`. In v2 a `StepContext` stops working
  when its step returns (§2.2).

## 7. Phases

The rule: **the first real consumer drives the work after step 3.** Don't ship a feature it hasn't exercised.

**Where the code goes. (open 2)** v2 is built in a subpackage of the root module, beside v1 (working name
`runbook`), and moves to the root in one mechanical change at the end. A breaking change to the root package
can't pass CI one PR at a time: `just build-standalone` builds `observers/terminal` against the core version its
`go.mod` requires, so a PR that ports the terminal observer fails that build, and a PR that doesn't fails the
workspace lint. In a subpackage every PR is additive, v1 and its examples stay green, and the consumer can import
the new engine from the first PR. Whether to flip at all, or keep both, is decided at step 7.

| Step | Content | Done when |
| --- | --- | --- |
| 0. Release `v0.2.0` | `fix(core): Keep draining OutputWriter past over-long lines` (A1, B3). `fix(json): Keep logging after an unserialisable payload` (A4, B4). `fix(core): Recover condition panics` (B1). Then `just prepare-release v0.2.0` and tag. | The appendix tests for A1, A4, B1, B3 and B4 are in the package as event-sequence tests. A standalone install of `observers/terminal@v0.2.0` works. |
| 1. Settle the design | Answer the open questions. Commit this doc. Record §8 in `DESIGN.md`. | No question is open. |
| 2. Engine in memory | The record type, used as journal record and as event. `MemoryJournal` and replay. Plan model: validation, names, serialisation, fingerprint, `Describe`. Executor: groups, limits, failure policy, step kinds, `Check` before `Run`, tries, panics, clock. `StepContext`, including `Record` and items. The fault-injecting journal and fake clock. `AsyncObserver` policies. Plain and JSON observers. | v1 executor tests ported. A2 with B1, A3 with B6, A5, A6, B2 and B5 pass as v2 tests. |
| 3. Durable | File journal: lock, half-written last line, directory fsync, schema version, size bounds, redaction, secret inputs. `Inspect`, with liveness. Resume: settled groups, the step table, fingerprint, inputs and force rules. `Follow`. | `CrashAfter` at every boundary of a nested 10-step plan resumes correctly. Recorded values survive a resume. The `SIGKILL` test passes. A viewer started late and one started from the beginning end with identical state. Replaying a 1 GiB journal meets a target set from measurement. A7 passes. |
| **Milestone A** | The installer runs on steps 2 and 3. | What it found is folded into this doc before step 4 starts. |
| 4. Stop, control, gates | Graceful stop and cancel, control files with run scoping and acknowledgement, `RequestStop`, `Approve`, gates and approvers with timeout policies, `HandleSignals`. | A stop during a step finishes that step and resumes cleanly. A stop releases a waiting gate without approving it. A gate is released by a control file, by a timeout under each policy, and by stdin. A stale control file is ignored. `StdinApprover` withdraws on EOF. |
| 5. Recovery | `OnFailure`, with the group reset. | `OnFailure` runs and is recorded. A resume after it runs the group's steps again. A resume after a crash inside it finishes it first. |
| **Milestone B** | The rolling upgrade runs on steps 2 to 5. | As Milestone A. |
| 6. Rollback | `Undo`, derived `Rollback` in reverse plan order, `Irreversible` with its three states, resuming a rollback, `UndoOnFailure`. | A rollback after a crash part-way through it completes. `ErrIrreversible` is returned once the point of no return has passed, and `ErrIrreversibleInDoubt` when it was interrupted. `Undo` reads a value recorded by an interrupted `Run`. A step skipped by `Check` is not undone. |
| 7. Flip and finish | Move the package to the root, or keep the subpackage. Port the terminal observer (tree, item rows). `examples/detached`, README section, `pipelinetest` contract checks, `examples/runbook` moved onto gates and the journal. Release `v0.3.0`. | The examples use only v2 APIs. Killing the viewer doesn't affect the runner. |

Every PR: `just` (tests with `-race`, plus lint over the core and `observers/terminal`), `just build-standalone`,
and `just check-clean` leaves no diff. PR titles follow `CONTRIBUTING.md`.

## 8. Decisions to record in `DESIGN.md`

Decisions 1 to 9 are from revision 1. "Changed" means revision 2 proposes a different rule. Decisions 10 onward are
new in revision 2.

1. **One journal file for durable and best-effort records** (one sequence, one thing to follow), with fsync only
   after durable records. The alternative, a separate output file, makes `Follow` and `seq` harder. Output is
   bounded where it is written (decision 6).
2. **Step identity is the path of names.** Renaming a step changes its identity, and the plan fingerprint catches
   it. Consider an explicit `.ID()` override if renames turn out to matter in practice.
3. **When the plan has changed, refuse by default.** Matching by path is an explicit option. *Changed:* "changed"
   means its identity (paths, kinds, Irreversible flags). Wording, labels and limits can change freely, and inputs
   are compared separately.
4. **Rollback calls `Undo` on every step whose `Run` was called**, including interrupted and failed ones. That is
   why `Undo` must cope with a `Run` that stopped part-way. *Changed:* revision 1 covered passed and interrupted
   steps only, and keyed on the step having started.
5. **A parallel limit applies to a group's direct children.** A child that is itself a group counts as one slot.
6. **Size limits on what a step writes**: recorded values (for example 64 KiB per step), the length of an output
   line, and the output of a step. *Changed:* revision 1 limited recorded values only.
7. **No rotation or compaction in v2.** There is one journal per operation, and a consumer starts a new journal
   for a new operation.
8. **Observe steps fail a resumed run exactly as they fail a first run**, when their group is entered. Stale
   health doesn't get a pass. *Changed:* a settled group is not entered (decision 10).
9. **Panics are recovered in every user callback** (`Check`, `Run`, `Undo`, approvers). *Changed:* and in
   observers, which are then dropped. v1's reasoning no longer holds once the journal is the record.
10. **A group that has finished and passed is skipped as a unit on resume.** Deciding step by step re-runs steps
    that a later step in the same group reversed.
11. **`Check` always runs before `Run`.** There is one rule for first attempts, resumes and tries.
12. **`Record` is durable when it returns.** `Undo` of an interrupted step depends on it.
13. **Rollback walks the plan in reverse plan order**, not in reverse order of start times.
14. **An Irreversible step that was interrupted or failed blocks rollback** until its `Check` or the operator says
    it did not commit.
15. **A passed `OnFailure` handler resets its group.** The journal must not describe a state the handler removed.
16. **Secret inputs are never stored**, not even as a hash, and are supplied on every attempt.
17. **An event is a decoded journal record, and errors in it are data.** In-process observers and viewers see the
    same thing.
18. **Every lifecycle record is durable**, including group records. Chatter is best-effort.
19. **A failing child lets its running siblings finish.** Cancelling them is opt-in.
20. **A gate's timeout restarts after a crash or stop.**
21. **Control requests are scoped to a run and acknowledged through the journal.**
22. **v2 is built beside v1 and flipped once**, and is released as `v0.3.0`.

## 9. Changes since revision 1

| Section | Change | Review item |
| --- | --- | --- |
| §1 | R4 and R5 reworded; R14 added | 2.1, 2.2, 2.7 |
| §2.1 | Group failure policy stated; name rules; label inheritance; capabilities in the serialised plan; fingerprint narrowed to identity; inputs get their own digest; discovery output is an input; validation rules | 2.7, 3.11, 3.13, 3.17 |
| §2.2 | `Retry` and `Timeout` defined, "try" against "attempt"; item statuses fixed; `Record` durable at once and returns an error; `StepContext` stops working after the step; three contracts added; custom events dropped | 1.3, 2.2, 3.5, 3.17, B5 |
| §2.3 | Timestamps; group, undo, value and `step.running` records; closed vocabularies; errors as data; schema version rules; half-written last line; size bounds; liveness and PID; secret inputs; concrete journal type; Windows | 2.6, 3.1 to 3.3, 3.5 to 3.9, 3.14, 3.17 |
| §2.4 | Settled groups; step table rewritten with `Check` always first; flow-control signals replayed; `ErrInputsChanged`; `Force()` defined; resume flow described | 2.1, 2.7, 3.2, 3.4, 3.17 |
| §2.5 | Stop interrupts gates and backoff; journal write failure row; control requests scoped, acknowledged and owned | 3.10, 3.12 |
| §2.6 | Approvers withdraw; timeout restarts; monotonic timing | 3.12 |
| §2.7 | Rollback set, order and refusal rules rewritten; state after a rollback; orphaned steps; `OnFailure` reset and its edge cases; `UndoOnFailure` | 2.3, 2.4, 2.5, 3.3 |
| §2.9 | Event is the record; observer panics recovered; unbounded lifecycle queue; `Close` with a context; `Follow` history, ending and `ErrRunnerGone` | 3.6, 3.9, 3.15, 3.16 |
| §2.10 | Harness built with the engine; how `CrashAfter` works; `NewStepContext`; the `SIGKILL` test | 3.17 |
| §3, §4 | Sketch and format updated to match; resume flow and a step using `Record` added | 2.2, 2.7 |
| §5 | Release as `v0.3.0`; new rows | 1.1, 1.3 |
| §6 | B1 to B6 added; B1, B3 and B4 fixed in `v0.2.0` | 4 |
| §7 | Incubation subpackage; steps reordered; two consumer milestones; `build-standalone` in the per-PR rule | 1.2, 5 |
| §8 | Decisions 3, 4, 6, 8 and 9 changed; 10 to 22 added | |

## Appendix: reproduction tests

These were run against `d13b76b` as an external module, with `replace github.com/ruffel/pipeline => <checkout>`, on
2026-11-11 (go1.25.6, darwin/arm64). The comment above each test gives the behaviour observed. Port the A1, A4, B1,
B3 and B4 tests into v1's package tests in step 0, as assertions on the sequence of events. Rewrite the others
against the v2 API, with the v2 behaviour in §6 as the expected result.

```go
package verify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ruffel/pipeline"
	jsonobs "github.com/ruffel/pipeline/observers/json"
)

type rec struct {
	mu  sync.Mutex
	evs []string
}

func (r *rec) OnEvent(_ context.Context, e pipeline.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch v := e.(type) {
	case pipeline.StepStartedEvent:
		r.evs = append(r.evs, "started:"+v.Step)
	case pipeline.StepFailedEvent:
		r.evs = append(r.evs, "failed:"+v.Step+":"+strings.SplitN(v.Err.Error(), "\n", 2)[0])
	case pipeline.StepPassedEvent:
		r.evs = append(r.evs, "passed:"+v.Step)
	case pipeline.StepSkippedEvent:
		r.evs = append(r.evs, "skipped:"+v.Step)
	case pipeline.PipelinePassedEvent:
		r.evs = append(r.evs, "pipeline-passed")
	case pipeline.PipelineFailedEvent:
		r.evs = append(r.evs, "pipeline-failed")
	case pipeline.OutputEvent:
		l := v.Line
		if len(l) > 20 {
			l = fmt.Sprintf("<%d bytes>", len(l))
		}
		r.evs = append(r.evs, "out:"+l)
	case pipeline.MessageEvent:
		m := v.Message
		if len(m) > 60 {
			m = m[:60] + "…"
		}
		r.evs = append(r.evs, "msg:"+m)
	}
}

func ok(context.Context) error { return nil }

// ---- the doc's appendix ----------------------------------------------------

// A2: observed "condition panic escaped Executor.Run: boom".
func TestA2ConditionPanic(t *testing.T) {
	p := pipeline.NewPipeline("p", pipeline.NewStage("s",
		pipeline.NewStep("a", ok).WithCondition(func(context.Context) string { panic("boom") })))
	defer func() {
		if r := recover(); r != nil {
			t.Logf("RESULT condition panic escaped Executor.Run: %v", r)
		}
	}()
	t.Logf("RESULT Run returned %v", pipeline.NewExecutor().Run(context.Background(), p))
}

// A3: observed [started:fail failed:fail:x started:b failed:b:context canceled
// started:c failed:c:context canceled pipeline-failed].
func TestA3FailFastQueuedStepsStart(t *testing.T) {
	r := &rec{}
	p := pipeline.NewPipeline("p", pipeline.NewParallelStage("s",
		pipeline.NewStep("fail", func(context.Context) error { return errors.New("x") }),
		pipeline.NewStep("b", func(ctx context.Context) error { return ctx.Err() }),
		pipeline.NewStep("c", func(ctx context.Context) error { return ctx.Err() }),
	).WithMaxParallel(1))
	_ = pipeline.NewExecutor(r).Run(context.Background(), p)
	t.Logf("RESULT events: %v", r.evs)
}

// A5: observed "panic in step \"a\": boom" (29 lines, the stack is in the message)
// vs "stage \"s\" step \"a\": plain".
func TestA5PanicErrorLocation(t *testing.T) {
	p := pipeline.NewPipeline("p", pipeline.NewStage("s",
		pipeline.NewStep("a", func(context.Context) error { panic("boom") })))
	err := pipeline.NewExecutor().Run(context.Background(), p)
	t.Logf("RESULT panic err first line: %q (total %d lines)", strings.SplitN(err.Error(), "\n", 2)[0], strings.Count(err.Error(), "\n")+1)
	p2 := pipeline.NewPipeline("p", pipeline.NewStage("s",
		pipeline.NewStep("a", func(context.Context) error { return errors.New("plain") })))
	t.Logf("RESULT normal err: %q", pipeline.NewExecutor().Run(context.Background(), p2))
}

// A1 (direct): observed both writes fail with "io: read/write on closed pipe";
// "after" never emitted.
func TestA1OutputWriterLongLineDirect(t *testing.T) {
	r := &rec{}
	ctx := pipeline.WithEmitter(context.Background(), pipeline.NewEmitter(r.OnEvent, pipeline.Location{Step: "s"}))
	w := pipeline.OutputWriter(ctx, pipeline.Stdout)
	_, err1 := w.Write([]byte("before\n" + strings.Repeat("a", 2<<20) + "\n"))
	_, err2 := w.Write([]byte("after\n"))
	_ = w.Close()
	t.Logf("RESULT write1 err=%v write2 err=%v events=%v", err1, err2, r.evs)
}

// A1 (exec): observed cmd err "signal: broken pipe"; "after" never emitted.
func TestA1OutputWriterLongLineExec(t *testing.T) {
	r := &rec{}
	ctx := pipeline.WithEmitter(context.Background(), pipeline.NewEmitter(r.OnEvent, pipeline.Location{Step: "s"}))
	w := pipeline.OutputWriter(ctx, pipeline.Stdout)
	cmd := exec.CommandContext(ctx, "sh", "-c",
		`echo before; head -c 2097152 /dev/zero | tr '\0' a; echo; echo after; exit 0`)
	cmd.Stdout = w
	err := cmd.Run()
	_ = w.Close()
	t.Logf("RESULT cmd err=%v events=%v", err, r.evs)
}

// A4: observed 3 of 9 lines written, Err() ends "json: unsupported type: func()".
func TestA4JSONObserverStopsAfterBadPayload(t *testing.T) {
	var buf bytes.Buffer
	o := jsonobs.New(&buf)
	p := pipeline.NewPipeline("p", pipeline.NewStage("s",
		pipeline.NewStep("a", func(ctx context.Context) error {
			pipeline.EmitCustom(ctx, "x.bad", map[string]any{"f": func() {}})
			return nil
		}),
		pipeline.NewStep("b", ok)))
	_ = pipeline.NewExecutor(o).Run(context.Background(), p)
	t.Logf("RESULT json lines written=%d (a healthy run writes 9) Err()=%v", strings.Count(buf.String(), "\n"), o.Err())
}

// A7: observed err=context canceled, events=[started:a passed:a pipeline-failed];
// b and c have no events.
func TestA7CancelLeavesStepsUnresolved(t *testing.T) {
	r := &rec{}
	ctx, cancel := context.WithCancel(context.Background())
	p := pipeline.NewPipeline("p", pipeline.NewStage("s",
		pipeline.NewStep("a", func(context.Context) error { cancel(); return nil }),
		pipeline.NewStep("b", ok), pipeline.NewStep("c", ok)))
	err := pipeline.NewExecutor(r).Run(ctx, p)
	t.Logf("RESULT err=%v events=%v", err, r.evs)
}

// A6: observed [msg:from-condition started:a passed:a pipeline-passed].
func TestA6ConditionEmitsBeforeStart(t *testing.T) {
	r := &rec{}
	p := pipeline.NewPipeline("p", pipeline.NewStage("s",
		pipeline.NewStep("a", ok).WithCondition(func(ctx context.Context) string {
			pipeline.EmitInfo(ctx, "from-condition")
			return ""
		})))
	_ = pipeline.NewExecutor(r).Run(context.Background(), p)
	t.Logf("RESULT events=%v", r.evs)
}

// ---- extra probes ----------------------------------------------------------

// B1: a Condition panic inside a PARALLEL stage happens on an executor goroutine,
// so the caller cannot recover it. Run as a child process.
func TestB1ConditionPanicParallelChild(t *testing.T) {
	if os.Getenv("B1_CHILD") != "1" {
		t.Skip("child only")
	}
	p := pipeline.NewPipeline("p", pipeline.NewParallelStage("s",
		pipeline.NewStep("a", ok).WithCondition(func(context.Context) string { panic("boom") }),
		pipeline.NewStep("b", ok)))
	defer func() {
		if r := recover(); r != nil {
			fmt.Println("CHILD recovered in caller:", r)
		}
	}()
	fmt.Println("CHILD Run returned:", pipeline.NewExecutor().Run(context.Background(), p))
}

// B1: observed the child print "Run returned: <nil>", then die with "panic: boom"
// (exit status 2). Run can report success before the process is killed.
func TestB1ConditionPanicParallel(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run", "^TestB1ConditionPanicParallelChild$", "-test.v")
	cmd.Env = append(os.Environ(), "B1_CHILD=1")
	out, err := cmd.CombinedOutput()
	lines := strings.Split(string(out), "\n")
	keep := []string{}
	for _, l := range lines {
		if strings.HasPrefix(l, "CHILD") || strings.HasPrefix(l, "panic:") || strings.Contains(l, "goroutine ") && strings.Contains(l, "[running]") {
			keep = append(keep, l)
		}
	}
	t.Logf("RESULT child exit err=%v; %v", err, keep)
}

// B2: DESIGN.md says observer panics are fatal. One raised while a step's StepPassed
// is delivered is inside the step's recover. Observed Run err
// "panic in step \"a\": observer bug", events=[started:a passed:a failed:a pipeline-failed].
func TestB2ObserverPanicSwallowedAsStepFailure(t *testing.T) {
	r := &rec{}
	bad := pipeline.ObserverFunc(func(_ context.Context, e pipeline.Event) {
		if _, isPassed := e.(pipeline.StepPassedEvent); isPassed {
			panic("observer bug")
		}
	})
	p := pipeline.NewPipeline("p", pipeline.NewStage("s", pipeline.NewStep("a", ok)))
	var err error
	func() {
		defer func() {
			if rv := recover(); rv != nil {
				err = fmt.Errorf("ESCAPED: %v", rv)
			}
		}()
		err = pipeline.NewExecutor(r, bad).Run(context.Background(), p)
	}()
	first := ""
	if err != nil {
		first = strings.SplitN(err.Error(), "\n", 2)[0]
	}
	t.Logf("RESULT step returned nil, Run err=%q events=%v", first, r.evs)
}

// B3: carriage-return progress (curl, rsync, pip, docker pull) never emits a newline,
// so it accumulates into one "line" and trips A1. Observed the stream close after
// 41,944 writes; "done" never emitted.
func TestB3CarriageReturnProgress(t *testing.T) {
	r := &rec{}
	ctx := pipeline.WithEmitter(context.Background(), pipeline.NewEmitter(r.OnEvent, pipeline.Location{Step: "s"}))
	w := pipeline.OutputWriter(ctx, pipeline.Stdout)
	var werr error
	n := 0
	for i := 0; i < 60000 && werr == nil; i++ { // 60k updates * ~20 bytes ≈ 1.2 MiB
		_, werr = fmt.Fprintf(w, "\rdownloading %6d/60000", i)
		n++
	}
	_, lerr := w.Write([]byte("\ndone\n"))
	_ = w.Close()
	t.Logf("RESULT progress writes before failure=%d werr=%v final write err=%v events=%v", n, werr, lerr, r.evs)
}

// Documented v1 behaviour, kept as evidence for the delivery policies in section 2.9:
// the terminal lifecycle event can be the one AsyncObserver drops.
// Observed dropped=3 delivered=[started:a].
func TestAsyncDropsTerminalEvent(t *testing.T) {
	r := &rec{}
	release := make(chan struct{})
	slow := pipeline.ObserverFunc(func(ctx context.Context, e pipeline.Event) {
		<-release
		r.OnEvent(ctx, e)
	})
	a := pipeline.NewAsyncObserver(slow, 2)
	p := pipeline.NewPipeline("p", pipeline.NewStage("s",
		pipeline.NewStep("a", func(context.Context) error { return errors.New("x") })))
	_ = pipeline.NewExecutor(a).Run(context.Background(), p)
	close(release)
	a.Close()
	t.Logf("RESULT dropped=%d delivered=%v", a.Dropped(), r.evs)
}

// B4: NaN is an ordinary float a step might report; it has the same effect as the func
// value in A4. Observed 3 of 9 lines written.
func TestB4JSONObserverNaN(t *testing.T) {
	var buf bytes.Buffer
	o := jsonobs.New(&buf)
	p := pipeline.NewPipeline("p", pipeline.NewStage("s",
		pipeline.NewStep("a", func(ctx context.Context) error {
			pipeline.EmitCustom(ctx, "metrics", map[string]any{"ratio": math.NaN()})
			return nil
		}),
		pipeline.NewStep("b", ok)))
	_ = pipeline.NewExecutor(o).Run(context.Background(), p)
	t.Logf("RESULT json lines written=%d Err()=%v", strings.Count(buf.String(), "\n"), o.Err())
}

// B5: an emitter outlives its step. Observed
// [started:a passed:a started:b msg:late passed:b pipeline-passed].
func TestB5LateEmitAfterStepPassed(t *testing.T) {
	r := &rec{}
	var wg sync.WaitGroup
	p := pipeline.NewPipeline("p", pipeline.NewStage("s",
		pipeline.NewStep("a", func(ctx context.Context) error {
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(20 * time.Millisecond)
				pipeline.EmitInfo(ctx, "late")
			}()
			return nil
		}),
		pipeline.NewStep("b", func(context.Context) error { wg.Wait(); return nil })))
	_ = pipeline.NewExecutor(r).Run(context.Background(), p)
	t.Logf("RESULT events=%v", r.evs)
}

// B6: ContinueOnError + MaxParallel: queued steps still start after a cancel.
// Observed "started:b failed:b:context canceled" after the cancel in a.
func TestB6BestEffortQueuedStartAfterCancel(t *testing.T) {
	r := &rec{}
	ctx, cancel := context.WithCancel(context.Background())
	p := pipeline.NewPipeline("p", pipeline.NewParallelStage("s",
		pipeline.NewStep("a", func(context.Context) error { cancel(); return nil }),
		pipeline.NewStep("b", func(ctx context.Context) error { return ctx.Err() }),
		pipeline.NewStep("c", func(ctx context.Context) error { return ctx.Err() }),
	).WithMaxParallel(1).WithContinueOnError(true))
	_ = pipeline.NewExecutor(r).Run(ctx, p)
	t.Logf("RESULT events=%v", r.evs)
}
```
