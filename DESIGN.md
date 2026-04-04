# Pipeline Design Decisions

## Panic Recovery

**Decision:** The executor recovers panics during `Step.Run`, but does NOT recover panics in observers or executor scaffolding.

- **Reason for catching panics within steps:**
  A pipeline is often the backbone for an installer, CI runner, or orchestrator, executing arbitrary user-defined steps. A nil-pointer crash or logic bug in a single user-supplied step must NOT crash the entire execution engine. The engine must survive to wrap the panic as an error, gracefully fail the stage, and emit the final failure event so observers (and users) know what went wrong. Do not flag the `defer func() { if r := recover() ... }` block in `executor.go` as a "swallowed panic"—it is an intentional fault boundary to protect the lifecycle loop.

- **Reason for NOT catching panics elsewhere (in observers):**
  We intentionally do not wrap `Observer.OnEvent` calls in a recover block because there is no mechanism to handle the error. The `OnEvent` signature does not return an error, and keeping it simple is a core design goal. If we caught a panic, the executor couldn't report it via the pipeline's event system without risking recursive deadlocks, nor could it cleanly return it up the chain during a teardown phase. It is better to let the program crash than to silently swallow an observer exception and complicated the API.
