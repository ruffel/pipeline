package pipeline

import "fmt"

// PanicError is the error a step produces when its Run function panics. The
// executor recovers the panic, wraps it in a PanicError and reports it like
// any other step failure: the [StepFailedEvent] carries the PanicError and
// [Executor.Run] returns it wrapped with the step's location.
//
// The captured stack stays out of the message so observers print a single
// line. Callers that want it use errors.As:
//
//	var perr *pipeline.PanicError
//	if errors.As(err, &perr) {
//	    log.Printf("%s\n%s", perr, perr.Stack)
//	}
type PanicError struct {
	Value any    // The value passed to panic.
	Stack []byte // The goroutine stack at recovery, as returned by debug.Stack.
}

// Error implements error.
func (e *PanicError) Error() string {
	return fmt.Sprintf("panic: %v", e.Value)
}

// Unwrap returns the panic value when it is itself an error, so errors.Is and
// errors.As see through a panicked error. It returns nil otherwise.
func (e *PanicError) Unwrap() error {
	err, _ := e.Value.(error)

	return err
}
