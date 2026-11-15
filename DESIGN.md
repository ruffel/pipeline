# Pipeline Design Decisions

## Why it exists

Go has a gap between a shell script and a workflow engine. Scripts and
`errgroup` keep no record, can't resume after a failure, and can't roll back.
Temporal and its kin are durable, but need a server or a database and a
deterministic-replay programming model. Ansible and Salt can't be embedded in a
Go program or unit tested there.

pipeline fills that gap for tools that operators run against real systems:
plan it, run it, kill it, resume it, roll it back, and watch it from anywhere,
all from one journal file, with no server.

It deliberately does not do:

- **Deterministic replay of code.** Steps are idempotent units, and the journal
  records outcomes, not instructions.
- **Distributed workers.** Remote work is the step's job (ssh, salt, an API).
- **Databases.** A journal is a file or memory.
- **Changing the plan mid-run, or loops.**
- **Launching processes, serving HTTP, or authentication.** The name on a stop
  or an approval is whatever the requester says it is.
- **A file journal on Windows.** The package builds there, and `OpenJournal`
  returns an error.

## The Plan Is Data

**Decision:** A plan is built once, validated by `NewPlan`, and does not change
during a run. Anything dynamic is worked out before building it.

The whole plan is known and printable (`Describe`) before anything changes, and
viewers in other processes can render it from the journal. A plan whose shape
depends on something only running code can discover is built after a read-only
discovery phase, whose result is an input (`WithInputs`), so a resume builds the
same plan even if the system has changed since.

## Identity Is the Path

**Decision:** An element's identity is the path of names from the root, such as
`upgrade/node1/packages`. Renaming a step changes its identity.

Names are unique among siblings and limited to letters, digits, `.`, `_` and
`-`, because paths are used in file names and command arguments.

## A Changed Plan Is Refused

**Decision:** `Run` refuses a journal whose attempts used a plan of a different
shape (`ErrPlanChanged`). The shape is the plan's fingerprint: its ordered paths
and each element's kind.

Descriptions, labels, parallel limits, timeouts, retries and gate options are
not part of the fingerprint, so a rebuilt tool that changed only those can
resume. Inputs are compared separately (`ErrInputsChanged`).

## The Journal Is the Source of Truth

**Decision:** Every lifecycle record (a run, group or step starting or
finishing, a recorded value, an item, a gate waiting or released, an undo, a
stop request) is written and synced before any observer is told. If a write
fails, the run stops.

Progress, messages, output, custom events and retries go into the same file
without a sync, so a crash can lose the last of them, never a lifecycle record.
One file means one sequence and one thing to follow. Output is bounded where it
is written (`MaxOutputLine`, `MaxStepOutput`), so the journal stays quick to
replay.

Resume, rollback, `Inspect` and every viewer read the journal through the same
replay, so they can't disagree about where a run stands.

There is no rotation or compaction: a consumer starts a new journal for a new
operation.

## Events Are Decoded Records

**Decision:** An event is a decoded journal record, and errors in it are data:
a kind, a message and, for a panic, a stack.

An observer in the runner gets exactly what a viewer replaying the journal with
`Follow` gets, so it behaves the same in both places. Custom events carry their
data as JSON for the same reason. They are best-effort records, so nothing that
resume or rollback needs may live only in one.

## Three Kinds of Step

**Decision:** Each step is an `Effect`, an `Ensure` or an `Observe`, chosen when
the plan is built.

- **Effect** changes the system once. Once it has passed, a resumed run checks
  it with its `Check`, or skips it if it has none.
- **Ensure** sets something that can drift between attempts (a service disabled,
  a maintenance mode), so its own passed outcome is never trusted: it is applied
  again whenever its group is entered.
- **Observe** only reads the system, so its result goes stale. It runs whenever
  its group is entered, fails a resumed run exactly as it fails a first run, and
  may not have a `Check` or an `Undo`.

A group is entered unless it is settled (below). The kinds differ in what a
passed outcome is worth. With a `Check`, an `Effect` and an `Ensure` step resume
alike: `Check`, then `Run` if the work isn't done.

## Check Always Runs Before Run

**Decision:** A step's `Check`, when it has one, runs before every `Run`: on a
first attempt, on a resume, and on every retry.

One rule covers every case. A `Run` that failed after doing its work passes on
the next try, and a step interrupted by a crash passes as recovered when its
`Check` finds the work done.

## Run and Undo Must Be Idempotent

**Decision:** `Run` may be called again on a system it has already partly
changed, and `Undo` must cope with a `Run` that stopped part-way.

A process can die after `Run` changed the system and before the journal said so.
`pipelinetest.CheckIdempotent` checks a step against this by crashing it after
every event it writes.

## Record Is Durable When It Returns

**Decision:** `StepContext.Record` syncs its value before it returns.

That gives steps the idiom that removes most crash hazards: record the intent or
the previous state before a change, and the result after it. `Undo` of an
interrupted step depends on reading what its `Run` recorded.

## A Settled Group Is Skipped Whole

**Decision:** On resume, a group that finished and passed in an earlier attempt
is settled, and is skipped as a unit: none of its steps run, nor their `Check`s.
That includes its `Ensure` and `Observe` steps.

Deciding step by step would re-run steps that a later step in the same group
reversed. A group with only `Observe` steps beneath it never settles. Steps can
read where an earlier attempt got to with `StepContext.Prior`, but the plan's
shape cannot depend on it: reconciling with a finished group is a step at the
level still in progress, not a reason to re-enter the group.

## Failures Let Running Siblings Finish

**Decision:** When a child fails, its group starts nothing new and lets running
children finish. Cancelling them is opt-in (`CancelOnError`).

This is the same rule as a graceful stop, applied to one group. A step cancelled
part-way leaves the system in a state its `Check` has to untangle, so the
default avoids it.

A parallel limit applies to a group's direct children: a child that is itself a
group takes one slot.

## Recovery Resets Its Group; Cleanup Never Does

**Decision:** A `RecoverOnFailure` step resets its group when it starts. An
`OnFailure` step leaves the group's record alone. A handler that fails is not
run again on resume.

The journal must not describe a state a recovery handler removed, and must not
forget work a cleanup handler left in place. The reset is recorded when recovery
starts, not when it passes: once recovery has begun, the group's state is
unknown, so the only safe resume is from its first step.

## Rollback Is Derived From the Journal

**Decision:** `Rollback` calls `Undo` on every `Effect` and `Ensure` step whose
`Run` was called, including steps that failed or were interrupted, in reverse
plan order.

Nobody writes the reverse plan by hand. A step whose `Run` was never called (its
`Check` found the work done) is not undone, because it changed nothing. Plan
order is a valid dependency order by construction, and it stays well-defined
across attempts and in parallel groups, where "when it started" is not. A
rollback is an attempt of its own, and running
it again after a failure carries on without undoing anything twice.

## Gates Wait Through the Journal

**Decision:** A gate records that it is waiting, with its deadline, and what
released it. Its timeout restarts after a crash or a stop.

A run never goes on by itself sooner than its timeout after it starts waiting.
`Approve` releases a gate from any process, so nothing has to wait on a
terminal.

## Control Requests Are Acknowledged Through the Journal

**Decision:** `RequestStop` and `Approve` write a request into a directory beside
the journal that only the run's user can write to, and return once the journal
shows the run acted on it.

Requests are scoped to the run in progress. The journal is how the requester
knows it worked, with no socket or server between them. The journal and the
directory are readable and writable only by the run's user, so that is who can
watch, approve and stop.

## Secrets Never Reach the Journal

**Decision:** An input of type `Secret` is never stored, not even as a hash, and
is supplied again on every attempt. Its value is masked wherever a step writes
it.

`WithRedactor` masks anything else before it reaches the journal or an observer.
Names, paths, labels, item IDs and recorded values are not redacted: they must
simply never be secret.

## Panics Are Recovered Everywhere

**Decision:** A panic in a step's `Check`, `Run` or `Undo`, an approver, or an
observer is recovered.

A panic in a step fails that step, with its stack in the journal, so one bad step
can't take down a run that may have been going for hours. A panicking observer
is dropped and a warning recorded: the journal is the record, so losing a view
loses nothing the run depends on, and an observer can never fail the run.
