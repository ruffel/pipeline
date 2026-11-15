// Package pipeline runs durable, resumable plans: trees of groups and steps
// whose outcomes are recorded in a journal, so a run can be stopped, resumed
// after a crash, rolled back, and watched from another process.
//
// # Plans
//
// A plan is data. It is built once, validated by [NewPlan], and does not change
// during a run. [Describe] renders it before anything happens:
//
//	plan, err := pipeline.NewPlan("upgrade",
//	    pipeline.Group("prechecks",
//	        pipeline.Observe("ring", ring{}),
//	        pipeline.Observe("disk", disk{}),
//	    ).Parallel(8).ContinueOnError(),
//	    pipeline.Effect("backup", backup{}),
//	    pipeline.Group("node1",
//	        pipeline.Effect("stop", stop{node}),
//	        pipeline.Effect("packages", packages{node}),
//	        pipeline.Observe("verify", verify{node}),
//	    ).Labels(pipeline.L{"node": "node1"}),
//	)
//
// Every element has a path built from its name and its parents' names, such
// as "upgrade/node1/packages". The path is the element's identity.
//
// A [Group] runs its children in order, or [GroupNode.Parallel] several at a
// time, and decides what a failing child means ([GroupNode.ContinueOnError],
// [GroupNode.CancelOnError], [GroupNode.OnFailure],
// [GroupNode.RecoverOnFailure]). A [Gate] waits for an approval.
//
// # Steps
//
// A [Step] is a value with a Run method; [StepFunc] adapts a function. A step
// may also be a [Checker], which says whether its work is already done, and an
// [Undoer], which reverses it. Run must be idempotent: after a crash it may be
// called again on a system it has already partly changed.
//
// The plan says what kind each step is, which decides what a resumed run does
// with it: [Effect] for work that changes the system once, [Ensure] for a
// setting that can drift, [Observe] for a step that only reads the system.
//
// A step reports and records through its [StepContext]: messages, progress
// and output, values that later steps and attempts can read
// ([StepContext.Record]), and per-target results ([StepContext.Item]).
//
// # Running and resuming
//
// [Run] runs a plan against a [Journal], which is the record of one operation:
// [OpenJournal] for a file that outlives the process, [MemoryJournal] for
// tests and tools that don't need one. Every outcome is written to the
// journal before the run moves on.
//
// Each call to Run on a journal is an attempt. The first starts the operation.
// A later one resumes it, doing only what the journal says is left; Run's own
// documentation has the rules. [Rollback] is an attempt that undoes what
// earlier ones did, by calling each step's Undo in reverse plan order.
//
// [WithInputs] records what the plan was built from, so a resume can build
// the same plan again. A [Secret] among them is never stored.
//
// # Stopping, approving and watching
//
// A [Stopper] stops a run gracefully, and [HandleSignals] wires one to Ctrl-C.
// From another process, [RequestStop] stops a run and [Approve] releases a
// gate, through a directory beside the journal file.
//
// An [Observer] given with [WithObservers] is told each event after the
// journal has stored it: [NewPlainObserver] and [NewJSONObserver] are built
// in, and the observers/terminal module has a styled view. [Follow] delivers
// the same events to observers in another process, and [Inspect] replays a
// journal into a [State]: where every element stands.
//
// # Testing
//
// The pipelinetest package runs one step on its own, checks that a step
// survives a crash at any point, and simulates a crash anywhere in a run.
package pipeline
