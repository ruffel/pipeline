# Pipeline Design Decisions

## Observer Failures

**Decision:** Observer failures are non-fatal and are not part of the
executor's error model.

The executor's return value should only be concerned if the pipeline work
succeeded? Output problems are a separate concern. A deploy step that completed
successfully should not look like a failed deploy because a log file filled the
disk after the fact.

For that reason, observers that care about durable or machine-consumed output
should expose their own error state.

Panics in observers remain fatal. This decision only covers ordinary write or
delivery failures.

## Terminal Events After Cancellation

**Decision:** If the run context has already been cancelled, terminal lifecycle
events are still delivered with cancellation stripped from the observer
context.

Users still need a final passed or failed event to render a summary, write a
last log line, or flush a UI. If terminal events inherited cancellation, the
observer would often see a cancelled context exactly when that final state was
most important. Non-terminal events keep the original cancellation state so
observers can still tell whether they were emitted before or after cancellation.

## Panic Recovery

**Decision:** The executor recovers panics during `Step.Run`, but does NOT recover panics in observers or executor scaffolding.

- **Reason for catching panics within steps:**
  A pipeline is often the backbone for an installer, CI runner, or orchestrator, executing arbitrary user-defined steps. A nil-pointer crash or logic bug in a single user-supplied step must NOT crash the entire execution engine. The engine must survive to wrap the panic as an error, gracefully fail the stage, and emit the final failure event so observers (and users) know what went wrong. Do not flag the `defer func() { if r := recover() ... }` block in `executor.go` as a "swallowed panic"—it is an intentional fault boundary to protect the lifecycle loop.

- **Reason for NOT catching panics elsewhere (in observers):**
  We intentionally do not wrap `Observer.OnEvent` calls in a recover block because there is no mechanism to handle the error. The `OnEvent` signature does not return an error, and keeping it simple is a core design goal. If we caught a panic, the executor couldn't report it via the pipeline's event system without risking recursive deadlocks, nor could it cleanly return it up the chain during a teardown phase. It is better to let the program crash than to silently swallow an observer exception and complicate the API.
