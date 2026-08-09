// Package pipeline provides observable task pipelines: define stages and
// steps, and an [Executor] handles sequencing, parallelism and flow control
// while emitting structured events to registered [Observer] implementations.
//
// The package separates three concerns:
//
//   - Definition: [Pipeline], [Stage] and [Step] describe the work, built
//     via [NewPipeline], [NewStage], [NewParallelStage] and [NewStep].
//   - Execution: [Executor.Run] validates and runs the pipeline, applying
//     conditions, sentinel flow control ([ErrSkipStage], [ErrSkipPipeline])
//     and per-step middleware ([WithRetry], [WithTimeout]).
//   - Observation: every lifecycle transition and in-flight signal is
//     delivered as an [Event] to observers, decoupling execution from
//     presentation. Ready-made observers live in the observers/ submodules,
//     and steps emit their own events via the EmitX helpers.
package pipeline
