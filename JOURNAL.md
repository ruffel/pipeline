# The Journal Format

A journal is a text file of events, one JSON object per line, in the order they
happened. It is the whole record of an operation: `Run` and `Rollback` append
to it, and resume, rollback, `Inspect` and `Follow` read it.

This page is for anything that reads events itself: an `Observer` of your own,
or a viewer in another language reading the file. A Go program that only wants
to know where a run stands doesn't need it: `Inspect` returns a typed `State`.

In Go an event is a `pipeline.Event`, whose fields have the names below with a
capital letter, except `ms`, which is `DurationMs`. An observer receives
exactly what the journal stores.

## A line

```json
{"v":1,"seq":14,"ts":"2026-11-15T13:14:01.551714Z","kind":"value","run":"59fa0f1c1fc9f348","path":"migrate/backup","key":"archive","value":"/var/backups/backup-20261115.json"}
```

Every event has these fields:

| Field  | Meaning                                                                    |
| ------ | -------------------------------------------------------------------------- |
| `v`    | The schema version that wrote the event, currently 1                       |
| `seq`  | A number that only increases through the file                              |
| `ts`   | When it was written, in UTC                                                |
| `kind` | What it records, from the table below                                      |
| `run`  | The attempt that wrote it: every call to `Run` or `Rollback` has its own   |
| `path` | The element it is about, such as `migrate/schema/001-create-users`         |

A field with nothing to say is left out of the line.

## Events

Durable events are synced to disk before the run moves on, and a run stops if
one can't be written. The rest are best-effort: a crash can lose the last few.

| `kind`           | Durable | About        | Its fields                                                                                                                 |
| ---------------- | ------- | ------------ | -------------------------------------------------------------------------------------------------------------------------- |
| `run.started`    | yes     | the root     | `attempt`, `plan`, `fingerprint`, `labels`, `inputs`, `inputsDigest`, `inputsChanged`, `forced`, `rollback`, `pid`, `host` |
| `run.finished`   | yes     | the root     | `outcome`, `error`, `ms`                                                                                                   |
| `group.started`  | yes     | a group      |                                                                                                                            |
| `group.finished` | yes     | a group      | `outcome`, `reason`, `error`, `ms`                                                                                         |
| `group.reset`    | yes     | a group      |                                                                                                                            |
| `step.started`   | yes     | a step, gate |                                                                                                                            |
| `step.running`   | yes     | a step       |                                                                                                                            |
| `step.finished`  | yes     | a step, gate | `outcome`, `reason`, `skip`, `error`, `ms`                                                                                 |
| `step.retry`     | no      | a step       | `try`, `error`                                                                                                             |
| `gate.waiting`   | yes     | a gate       | `prompt`, `deadline`                                                                                                       |
| `gate.released`  | yes     | a gate       | `by`, `who`, `uid`, `policy`                                                                                               |
| `value`          | yes     | a step       | `key`, `value`                                                                                                             |
| `item`           | yes     | a step       | `item`, `itemStatus`, `message`                                                                                            |
| `message`        | no      | a step       | `level`, `message`                                                                                                         |
| `progress`       | no      | a step       | `key`, `message`, `current`, `total`                                                                                       |
| `output`         | no      | a step       | `stream`, `line`, `continued`                                                                                              |
| `custom`         | no      | a step       | `type`, `data`                                                                                                             |
| `stop.requested` | yes     | the root     | `mode`, `by`, `who`, `uid`                                                                                                 |
| `plan.changed`   | yes     | the root     | `added`, `removed`                                                                                                         |
| `undo.started`   | yes     | a step       |                                                                                                                            |
| `undo.finished`  | yes     | a step       | `outcome`, `reason`, `error`, `ms`                                                                                         |

What each one means:

- **`run.started`** opens an attempt. `plan` is the whole tree, so a viewer can
  draw every element before any of them starts. `rollback` marks an attempt
  made by `Rollback`, and `forced` one made with `Force`. `inputs` is what
  `WithInputs` was given, without its secrets. `inputsChanged` lists the
  fields of `inputs` that differ from the attempt before, as JSON pointers,
  when a resume accepted a change.
- **`run.finished`** closes it. An attempt with no `run.finished` is still
  running, or its process died.
- **`group.reset`** is a recovery handler starting: everything in the group goes
  back to never started.
- **`step.running`** means `Check` found the work not done and `Run` is about to
  be called. A step with a `step.running` may have changed the system, which is
  what a rollback goes by.
- **`step.retry`** is a try that failed and will be tried again. `try` counts
  from 1.
- **`gate.waiting`** and **`gate.released`** bracket a gate's wait. A gate that
  a stop interrupts is never released.
- **`value`** is a `Record`, and **`item`** a result for one target of a step.
- **`message`** is an `Info` or a `Warn`. A warning from the run itself, such as
  an observer being dropped, has no `path`.
- **`output`** is one line a step wrote. A line too long for one event is split,
  and every piece but the first has `continued`.
- **`custom`** carries a step's own event: `type` names it, and `data` is JSON.
- **`stop.requested`** records who asked a run to stop, and how.
- **`plan.changed`** follows the `run.started` of an attempt made with
  `WithPlanChange` and a plan of a different shape. `added` and `removed` are
  the paths the new plan has and the old one had, in plan order. It is there to
  be shown: the two plans in the `run.started` events say the same.
- **`undo.finished`** is a rollback's result for one step.

A step that is skipped without being started has a `step.finished` and no
`step.started`: one an earlier attempt finished, or one its group or the run
ended before reaching.

## Values

| Field        | On                             | Values                                                                                                      |
| ------------ | ------------------------------ | ----------------------------------------------------------------------------------------------------------- |
| `outcome`    | `run.finished`                 | `passed`, `failed`, `stopped`, `cancelled`                                                                  |
| `outcome`    | `group.finished`               | `passed`, `skipped`, `failed`, `stopped`                                                                    |
| `outcome`    | `step.finished`                | `passed`, `skipped`, `failed`, and `stopped` for a gate a stop interrupted                                  |
| `outcome`    | `undo.finished`                | `undone`, `skipped`, `failed`                                                                               |
| `reason`     | a skipped or recovered outcome | `done`, `earlier-attempt`, `recovered`, `settled`, `not-reached`, `group-skipped`, `run-skipped`, `no-undo` |
| `skip`       | `step.finished`                | `skip-group`, `skip-run`: what the step's `Run` asked for                                                   |
| `itemStatus` | `item`                         | `passed`, `failed`, `skipped`                                                                               |
| `level`      | `message`                      | `info`, `warn`                                                                                              |
| `stream`     | `output`                       | `stdout`, `stderr`                                                                                          |
| `mode`       | `stop.requested`               | `graceful`, `cancel`                                                                                        |
| `by`         | `stop.requested`               | `stopper`, `control` for `RequestStop`, `timeout` for a gate's timeout                                      |
| `by`         | `gate.released`                | The approver's name, `control` for `Approve`, `timeout` for a gate's timeout                                |
| `policy`     | `gate.released`                | `continue`, `decline`: the timeout policy applied, when a timeout released it                               |

`who` is whoever asked or approved, as they named themselves, and `uid` the
user that owned a request made from another process.

An `error` is an object, never a string:

```json
{"kind":"timeout","message":"context deadline exceeded"}
```

Its `kind` is `error`, `cancelled`, `timeout`, `panic` or `declined`, and a
panic also has its `stack`. A step cut short by a cancel finishes as `failed`
with an error of kind `cancelled`.

## The plan

`plan` in `run.started` is a tree of nodes. In Go it is a `pipeline.NodeSpec`.

| Field                    | On     | Meaning                                                            |
| ------------------------ | ------ | ------------------------------------------------------------------ |
| `name`, `path`           | all    | The element's name, and its path from the root                     |
| `kind`                   | all    | `group`, `effect`, `ensure`, `observe` or `gate`                   |
| `description`, `labels`  | all    | As given in the plan                                               |
| `children`               | groups | The group's elements, in order                                     |
| `parallel`, `limit`      | groups | Whether children run at once, and how many (0 is no limit)         |
| `failure`                | groups | `stop`, `cancel` or `continue`: what a failing child does          |
| `onFailure`, `recovers`  | groups | The failure handler's node, and whether it resets the group first  |
| `check`, `undo`          | steps  | Whether the step has a `Check` and an `Undo`                       |
| `irreversible`           | steps  | Whether the step is marked `Irreversible`: no rollback once it ran |
| `tries`, `backoff`       | steps  | How many tries, and the wait between them, in nanoseconds          |
| `timeout`                | steps  | The limit on each try, or on a gate's wait, in nanoseconds         |
| `prompt`, `onTimeout`    | gates  | What the gate asks, and its timeout policy                         |

## Reading one

- **Read it without locking.** A run appends whole lines. A last line with no
  line break is still being written, or was cut off by a crash: leave it.
- **Follow it** by reading on from the last `seq` you saw.
- **Ignore what you don't know.** A newer release may add kinds and fields that
  an older reader can pass over. A change that an older reader would get wrong
  raises `v`, and a reader should refuse a `v` newer than it knows.
- **An attempt is the events sharing a `run`,** from its `run.started` to its
  `run.finished`. Later attempts append to the same file.
- **The latest event about a path wins,** with five exceptions:
  - a `group.reset` forgets everything beneath the group, except that an
    `irreversible` step with a `step.running` has run;
  - an `undo.finished` with outcome `undone` takes its step back to never
    started;
  - a rollback that passes makes every gate wait again;
  - a `run.started` with `forced` means nothing done before it counts as done;
  - a `run.started` whose `plan` has paths the one before it lacked means the
    groups around each new path are no longer finished. Paths it no longer
    has keep what they had.
