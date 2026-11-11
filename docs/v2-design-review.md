# pipeline v2 design: review and plan

Reviewed against `main` at `d13b76b` on 2026-11-11. The doc's baseline holds: `go test -race` passes on the
core, json, plain and terminal modules.

Revision 2 of the design, with the fixes below applied, is in `docs/v2-design.md`. It assumes my recommendation
for each decision in section 6 and marks where each one lands. Section references below (§) are to revision 1.

## Verdict

The direction is right and the doc is close to plannable, but I would not plan from it as it stands. Three things
are in the way:

1. Two statements conflict with the repo (the version number, and the per-PR CI rule), so Phase 0 and the
   "every PR passes `just`" rule can't be followed as written.
2. Seven rules in the doc's §2 give wrong behaviour in situations the two motivating consumers will hit. The worst
   one restarts every already-upgraded node on every resume, using the doc's own §3 example.
3. Seventeen open points decide the journal schema. That is the one part that is expensive to change once a
   consumer has journals on disk, so they need answers before Phase 1, not during it.

Sections 1 to 3 cover those in order. Section 4 is the v1 audit, section 5 the plan with the fixes folded in, and
section 6 the decisions I need from you.

## 1. Conflicts with the repo

### 1.1 `v0.2.0` is already taken, and Phase 0 can't be a `v0.1.x` patch

§5 releases v2 as `v0.2.0`, and §7 tags Phase 0 "as a v0.1.x patch". The latest tag on the remote is `v0.1.4`.
`main` is 15 commits past it, including four `feat(core)` changes (`MaxParallel`, `Description`, run ID, and
`StepSkippedEvent` for `ErrSkipStage`) that were agreed as v0.2.0 in August and never released.
`observers/terminal/go.mod` still requires the pseudo-version `v0.1.5-0.20260809134210-a3b3c34a82fd`.

**Fix.** Phase 0 lands A1 and A4 on `main` and releases what is there as **v0.2.0**. The redesign ships as
**v0.3.0**. I would also stop calling it "v2" in the repo, because in Go that reads as a `/v2` module path. I keep
the name below only to match the doc.

### 1.2 A breaking core PR can't pass CI

§7 requires every PR to pass `just` and `just check-clean`. CI also runs `just build-standalone`, which builds
`observers/terminal` with `GOWORK=off` against the core version its `go.mod` requires. A PR that changes a core
type the terminal observer uses fails either way:

- Port the terminal observer in the same PR, and the standalone build fails, because the published core it
  requires doesn't have the new API.
- Leave it alone, and the workspace lint and tests fail against HEAD core.

The terminal observer's public `Options` are 14 formatter hooks typed on v1 events, so the first v2 PR hits this.

**Fix.** Build v2 beside v1, not on top of it. Incubate it in a subpackage of the root module (the name is yours;
`runbook` matches the doc's title). v1, the terminal observer, the examples and the README stay untouched and
green. PRs are additive and small from the first one, and the consumer can import the subpackage from day one. At
the end there is one mechanical flip: move the package to the root, delete v1, port the terminal observer in the
following PR. Whether to flip at all, or keep v1 as the light executor and v2 as a subpackage, can be decided then.
Nothing earlier depends on it.

### 1.3 What actually carries over from v1

§6 fixes A1 and A4 now "because this code carries over into v2". From the audit:

- `output_writer.go` carries over, behind `StepContext.Stdout()`.
- The JSON observer does not. §5 has it write the journal format, which replaces its envelope code. What carries
  over from A4 is a requirement: no value a step hands the engine may poison the record stream. That lands on
  `Record(key, value)`, which has no error return in §2.2. It needs one, for marshal failures and the size limit.
- Nothing in `executor.go` survives. This is a new engine: about 1,300 lines of non-test core are replaced, and
  about 3,100 lines of tests are ported or retired.
- Three v1 features have no v2 counterpart in the doc: `CustomEvent` (used by `examples/demo`), the debug message
  level, and message attributes (which no v1 helper can set anyway). The doc should say which are dropped.

A1 and A4 are still worth fixing in Phase 0, because v0.2.0 ships them.

## 2. Rules that give wrong behaviour

### 2.1 Resume restarts finished nodes (§2.4)

§2.4 decides step by step, and `ResumeVerify` (the default) re-runs any passed step whose `Check` now says "not
done". Take the node group from §3: `stop`, `packages`, `apply`, `start`, `verify`. Node 1 finished in attempt 1,
and attempt 2 resumes at node 5. The walk reaches node 1:

- `stop` passed. Its `Check` asks whether the service is stopped. It isn't, because `start` ran afterwards. So
  `stop` runs again, on a node that is upgraded and serving.
- `start` passed. Its `Check` now says not running, so it runs again.

Every resume therefore restarts every finished node, one at a time, before it reaches the node that failed. The
same happens to any pair of steps where the second reverses the first (drain and undrain, maintenance on and off).
Ensure steps have the same problem without any `Check`: node 1's "enter maintenance" re-applies on every attempt.

The hazard appears exactly when the consumer follows the doc's advice and gives steps a `Check`.

**Fix.** Decide per group before deciding per step. Call a group *settled* when it has at least one Effect or Gate
step beneath it and all of them have passed in the journal. On resume, a settled group is skipped as a unit and
nothing in it runs. The §2.4 table applies only inside groups that still have work. Consequences:

- A finished node group is not re-entered. Its Ensure, Effect and Observe steps all stay as they are.
- A group of only Observe steps (prechecks) is never settled, so it re-runs on every attempt, as the doc intends.
- A top-level Ensure re-applies, because the root is not settled until the run is.
- Inside the node that failed, the table still does the right thing: if someone restarted the service between
  attempts, `stop`'s `Check` catches it.

The modelling rule to document: steps that reverse each other belong in the same group.

§2.2 says the hyperstore upgrade's correctness relies on re-applying settings on every attempt. I need your read
on which settings, and at which level of the tree, to confirm this rule fits.

### 2.2 `Record` is persisted too late for `Undo` to use it (§2.2, §2.3, decision 4)

Recorded values are stored "with the step's outcome", in `step.finished`. Decision 4 has rollback call `Undo` on
interrupted steps. An interrupted step never wrote `step.finished`, so its `Undo` can't read what its own `Run`
recorded. Example: `packages` records the previous version, starts the upgrade, and the process dies. `Undo` needs
the previous version, and it isn't in the journal.

**Fix.** `Record` appends its own durable record when it is called, and returns an error. The latest value for a
path and key wins. `Recorded` can read it from any later attempt or rollback, whether or not the step finished. A
passed `Undo` clears that step's values. This also gives steps the idiom that removes most in-step crash hazards:
record the intent or the previous state before the change, and the result after it.

### 2.3 Rollback's set and order (§2.7, step 1)

The sentence "every Effect step that started (passed, or interrupted part-way), in reverse order of when it
started" has three problems:

- It leaves out steps that **failed**. Those can also have changed the system.
- "When it started" is ambiguous across attempts, and wrong for anything re-applied late. An Ensure step re-run in
  attempt 3 has the newest start, so its `Undo` (say, leave maintenance mode) runs first, before the node changes
  it was protecting are undone. Use **reverse plan order**. It is a valid dependency order by construction, and it
  is deterministic for parallel groups.
- Once `Check` runs after `step.started` (A6), "started" no longer means "`Run` was called". A crash during
  `Check` leaves a started step that changed nothing, and rollback would undo state that was there before the run.
  Write a durable marker between `Check` and `Run`, and have rollback take the steps that have it.

### 2.4 An interrupted Irreversible step (§2.7, step 2)

Rollback refuses only when an Irreversible step has *passed*. If the process died inside `commit-schema`, nobody
knows whether the point of no return was crossed, and rollback goes ahead and undoes the steps before it.

**Fix.** Refuse if an Irreversible step has started, unless its `Check` reports "not done". `Inspect` reports
three states: not reached, in doubt, passed.

### 2.5 `OnFailure` leaves the journal describing a state that no longer exists (§2.7)

Node 7 fails at `apply`. `recover` runs and puts the node back on the old version. The journal still says node 7's
`stop` and `packages` passed. On resume, any of those steps without a `Check` (or all of them under `ResumeTrust`)
is skipped, and the node is started on the old packages and reported as upgraded.

**Fix.** When an `OnFailure` step passes, the engine writes a durable record that resets the group's children to
"never started". Two more cases need a rule:

- Does `OnFailure` run when the group failed because of a cancel? I would say no. The operator asked to stop now.
- If the process dies inside `OnFailure`, resume finishes it before anything else in that group.

Worth considering: derive recovery the way rollback is derived. `Group(...).UndoOnFailure()` would run the group's
own `Undo`s in reverse plan order. That is one rollback mechanism at two scopes, not two mechanisms, and the reset
falls out of it.

### 2.6 Secret inputs (§2.1 against R12)

Inputs are "the answers, flags and defaults the plan was built from", and they are stored in `run.started`. An
installer's answers include passwords and licence keys. The redactor works on strings passing through, not on
structured inputs.

**Fix.** Inputs carry a per-field secret marking. A secret field is never written, is left out of the fingerprint,
and has to be supplied again on resume. Don't store a hash of it either: a hash of a low-entropy secret in a
readable file can be guessed offline. This is the one exception to "never asked again on resume", and the doc
should say so. Create the journal as `0600` and the control directory as `0700`.

### 2.7 The fingerprint is strictest when you most need to resume (§2.1, §2.4)

The fingerprint hashes descriptions, labels, group settings and inputs, and a mismatch refuses the run. The
likeliest reason to resume with a different binary is a hotfix for the step that just failed. That binary reworded
a description or lowered a concurrency limit, and it is refused in the middle of an upgrade.

**Fix.**

- Fingerprint only identity: the ordered paths, their kinds, and the Irreversible flags.
- Compare inputs separately, with their own error (`ErrInputsChanged`).
- State that discovery output is an input. Otherwise a node that died between attempts changes the plan.
- For `MatchByPath`, define what happens to a journal step that has no plan element. It can't be undone, so
  rollback must report it and refuse unless told otherwise.
- Show the resume flow in §3. The consumer has to call `Inspect` first, to get the inputs it needs to build the
  plan. The sketch only shows a first run.

## 3. Open points that decide the journal schema

1. **Timestamps.** The records in §4 have no time field. Every record needs one, for the audit trail, for a late
   viewer to show how long a running step has been going, and for gate deadlines.
2. **Group records.** There are no `group.*` kinds. `ErrSkipGroup`, `OnFailure`, group durations and the ported
   observers' stage lines all need them. Related: when an Effect step that returned `ErrSkipGroup` is skipped as
   passed on resume, the skip has to be replayed from its record, or the siblings it bypassed will run.
3. **Undo records.** Rollback is "recorded in the same journal", but no record kind marks an undo starting or
   finishing. Define the per-step states, including undone and undo-failed, and say what a later `Run` sees (I
   suggest undone means never started).
4. **`Check` on a first run.** The §2.4 table runs a never-started step without checking. §2.1 and §5 say `Check`
   is the run-time "already done" test that replaces `Condition`. Pick one. I would always check before `Run`,
   which also shrinks the table to two rows.
5. **Closed vocabularies.** Outcomes, skip reasons (already done, earlier attempt, group skipped) and item
   statuses should be fixed lists. Viewers switch on them.
6. **Errors as data.** A `Follow` viewer gets strings, and an in-process observer gets live errors, so
   `errors.Is(e.Err, context.Canceled)` works in one and not the other. That breaks R11. Classify the error before
   writing it (cancelled, timeout, panic, declined, other; message; stack) and hand in-process observers the same
   decoded record the journal got. One type serves as both record and event.
7. **Torn tail.** `Follow` copes with a half-written last line. `OpenJournal` has to as well: after a crash
   mid-append, the next append lands on the same line. Truncate to the last complete line while holding the lock,
   and treat an invalid line anywhere else as corruption. Fsync the directory when creating the journal.
8. **Size.** Decision 6 caps recorded values, but output is the bulk: hours of salt output, in the file that
   `Inspect` and every viewer replay from the start. Cap the line length at write time, well below 1 MiB. Give
   each step an output budget, with a "dropped N lines" marker. Put a replay-time target in Phase 2's exit
   criteria. The reader must not repeat A1 with a fixed-size scanner.
9. **Liveness.** `flock` can't report who holds it, so `ErrJournalBusy` "with the holder's PID" needs the PID
   written somewhere (a lock file, or `run.started`). `Inspect` can't tell "running" from "crashed" by replay
   alone, so it should probe the lock and say which. `Follow` needs the same probe, or a viewer attached to a
   crashed run waits forever.
10. **Stale control files.** A `stop` or `approve/...` file left behind by a dead runner acts on the next attempt.
    Requests should carry the run ID from the latest `run.started`. The runner deletes what it consumes and
    ignores requests for other runs. `Approve` fails if the gate isn't waiting, and `RequestStop` fails if nothing
    is running. Write the files by rename.
11. **Names.** Paths become file names under `approve/`. Names need a defined character set that excludes `/` and
    `..`, enforced when the plan is built.
12. **Gates.**
    - A graceful stop "lets running steps finish", and a waiting gate could hold a stop for six hours. A stop has
      to interrupt the wait and leave the gate unreleased. The same goes for a Retry backoff sleep.
    - After a crash, does the timeout restart, or continue from the original wait? I would restart it, so the run
      never continues on its own sooner than configured after a restart.
    - `StdinApprover` must withdraw on EOF or a read error, not decline. After SIGHUP the terminal is gone and
      reads fail. v1's runbook example treats that as "declined" (`examples/runbook/steps.go:29`).
13. **Fail-fast in a parallel group.** v1 cancels the running siblings. For node upgrades that kills an in-flight
    package install on node B because node A failed. The doc doesn't say what v2 does. I would default to "start
    nothing new and let running siblings finish", like a graceful stop, with cancelling as an option.
14. **Schema evolution.** "A reader refuses a version it doesn't know" isn't enough, because a newer library will
    append to a journal an older one started. State that readers accept every older version, that a file may mix
    versions, and that unknown fields are ignored.
15. **Observers.**
    - Decision 9 keeps observer panics fatal. v1's reason was that there was nowhere to report one. In v2 there
      is, and a nil dereference in a renderer three hours in would kill a detached upgrade mid-step. I would
      recover it, drop that observer, and write a warning record.
    - "Lifecycle events are never dropped" means the queue is unbounded or the run blocks. Say unbounded; the plan
      size bounds it.
    - `Close` needs a deadline. Today a wedged terminal blocks it forever (`async_observer.go:101`).
16. **Late viewers.** Replaying from 0 dumps hours of output. Starting from `Inspect`'s `seq` leaves an observer
    without the plan it builds its state from. `Follow` needs a history option: lifecycle records only, then
    everything live.
17. **Smaller points.**
    - A wrapper around a step hides its `Check` and `Undo`, because they are optional interfaces. `Describe`
      should print each step's capabilities, so the R1 printout shows what rollback can do. Plan validation should
      reject an Observe step that has an `Undo`.
    - Does `Retry` cover a failing `Check`? "Attempt" currently means both a run attempt and a retry attempt.
    - `Observe`, the step kind, sits next to `Observer`, the event sink. `Probe` would avoid the clash.
    - What `Force()` does to each step is not defined.
    - `flock` is Unix only. The package should still compile on Windows, with the file journal returning an
      error.
    - "The journal is an interface" conflicts with `Inspect`, `Follow`, `RequestStop` and `Approve` all taking a
      file path. Keep it concrete until a third implementation exists.
    - The fault-injecting journal and fake clock behind `CrashAfter` are needed for Phase 2's exit criterion, but
      `pipelinetest` only appears in Phase 6.

## 4. v1 audit

All seven bugs in the appendix reproduce on `d13b76b` exactly as described. I found six more.

| | Finding | Observed |
| --- | --- | --- |
| B1 | A `Condition` panic in a **parallel** stage kills the process. The caller can't recover it, and `Run` can return `nil` first. This is A2, but worse than the appendix shows. | Child process printed `Run returned: <nil>`, then exited 2 with `panic: boom` |
| B2 | An observer panic while a step's event is delivered is recovered as a step failure. One step gets `StepPassed` and then `StepFailed`. `DESIGN.md` says observer panics are fatal. | `started:a passed:a failed:a` |
| B3 | A1 triggers on ordinary `\r` progress output (curl, rsync, pip). There is no newline, so it is one line. | Stream died after 41,944 small writes |
| B4 | A4 triggers on `NaN` as well as on a func value. | 3 lines written, then silence |
| B5 | An emitter outlives its step, so events arrive after `StepPassed`. | `passed:a started:b msg:late` |
| B6 | With `ContinueOnError` and `MaxParallel`, queued steps still start after a cancel. This is A3's sibling. | `started:b failed:b:context canceled` |

What follows from these:

- **A1's fix** should break lines on `\r` as well as `\n`. Otherwise progress output still builds maximum-length
  lines.
- **B1** is a five-line fix (install the recover before calling `Condition`, `executor.go:263`). It is worth
  including in v0.2.0, because it can take down a consumer's process.
- **B2 and B5** become v2 rules: observer calls sit outside the step's recover, and a `StepContext` goes inert
  when its step returns. Both need acceptance tests, alongside A2, A3, A5, A6 and A7.

## 5. Plan

Changes from §7, apart from the fixes above:

- Phase 0 is the v0.2.0 release.
- The doc is amended before any v2 code, because sections 2 and 3 change the schema.
- The test harness is built with the engine, not in Phase 6.
- `Follow` moves up to sit with the file journal. It costs little once replay exists, a second reader pins the
  format early, and watching is a first-day need for an upgrade.
- Rollback goes last. Its semantics are the least certain and the installer can ship without it.
- There are two consumer milestones, to enforce the doc's own rule that the consumer drives the later phases.

| Step | Content | Done when |
| --- | --- | --- |
| 0. Release v0.2.0 | `fix(core): Keep draining OutputWriter past over-long lines` (A1, B3). `fix(json): Keep logging after an unserialisable payload` (A4, B4). Optionally `fix(core): Recover condition panics` (B1). Then `just prepare-release v0.2.0` and tag. | A1 and A4 tests are in the package as event-sequence tests. A standalone install of `observers/terminal@v0.2.0` works. |
| 1. Settle the design | Amend the doc for sections 1 to 3 and commit it. Record the decisions in `DESIGN.md`. | Nothing in section 6 is open. |
| 2. Engine in memory (doc Phase 1) | In the incubation subpackage. One record type used as both journal record and event; `MemoryJournal`; replay to state. Plan model: validation, names, serialisation, fingerprint, `Describe` with capabilities. Executor: groups, limits, failure policy, step kinds, `Check` before `Run`, panic handling, injectable clock. `StepContext`. Fault-injecting journal and fake clock. `AsyncObserver` policies. Plain and JSON observers. | v1 executor tests ported. A2 with B1, A3 with B6, A5, A6, B2 and B5 pass as v2 tests. |
| 3. Durable (doc Phase 2, plus `Follow`) | File journal: lock, torn tail, directory fsync, schema version, size caps, redaction, secret inputs. `Inspect` with liveness. Resume, including settled groups. Fingerprint, inputs and force rules. `Follow` with the history option. | `CrashAfter` at every boundary of a nested 10-step plan resumes correctly. One test kills a real subprocess with `SIGKILL` and resumes. A late viewer and a full replay end in the same state. Replay of a large journal meets the target. A7 passes. |
| **Milestone A** | The installer runs on steps 2 and 3. | Its feedback is folded in before step 4 starts. |
| 4. Stop, control, gates (doc Phase 3) | As the doc, plus the stale-control rules, a stop that interrupts gates and backoff, and `StdinApprover` on EOF. | The doc's criteria, plus a test for each addition. |
| 5. Recovery | `OnFailure` with the child reset. | `OnFailure` runs and is recorded. A resume after recovery re-runs the group. |
| **Milestone B** | The rolling upgrade runs on steps 2 to 5. | As Milestone A. |
| 6. Rollback (rest of doc Phase 4) | `Undo`, derived rollback in reverse plan order, three-state Irreversible, resuming a rollback. | The doc's criteria. An interrupted Irreversible step refuses rollback. `Undo` reads values recorded by an interrupted `Run`. |
| 7. Flip and finish (doc Phases 5 and 6) | Move to the root or keep the subpackage. Port the terminal observer (tree, item rows). `examples/detached`. Contract checks. README. Release v0.3.0. | The examples use only the new API. |

## 6. Decisions needed

1. Release `main` as v0.2.0 now and ship the redesign as v0.3.0? I recommend yes.
2. Incubate in a subpackage and flip at the end? I recommend yes.
3. Does the settled-group rule (2.1) fit what the hyperstore upgrade re-applies on every attempt?
4. Should `Check` always run before `Run`, including on a first attempt? I recommend yes.
5. `OnFailure`: a hand-written step that resets the group, a derived undo, or both?
6. Gate timeout after a crash: restart (my recommendation) or continue?
7. Parallel fail-fast: let running siblings finish (my recommendation) or cancel them?
8. Recover observer panics in v2? I recommend yes.
9. Drop `CustomEvent`, the debug level and message attributes, or carry them over? I would drop all three.
10. Include the B1 fix in v0.2.0? I recommend yes.

Not verified: I reasoned about the hyperstore installer and upgrade only from the doc's description. I have not
seen their code, so 2.1 and 2.5 in particular rest on the §3 sketch being representative.
