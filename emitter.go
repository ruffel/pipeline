package pipeline

// Emitter allows steps to emit events during execution without direct access
// to the executor. An Emitter is stored in the step's context by the executor
// and accessed via the EmitX package-level helpers.
type Emitter struct{}
