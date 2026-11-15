package pipeline

import (
	"encoding/json"
	"time"
)

// SchemaVersion is the journal format this package writes. Readers accept
// every version up to their own and refuse newer ones.
//
// Readers ignore event kinds and fields they don't know. So a new kind or
// field that only informs viewers, which an older reader can pass over
// without getting a run's state wrong, keeps the version. One that changes
// what [Replay] computes needs a new version, so that older readers refuse
// the journal instead of misreading it.
const SchemaVersion = 1

// EventKind says what an [Event] records.
type EventKind string

// The kinds of event. Lifecycle kinds are durable: the journal stores them
// before any observer sees them, and a run stops if it cannot. The rest are
// best-effort, and a crash can lose the last of them.
const (
	EventRunStarted  EventKind = "run.started"
	EventRunFinished EventKind = "run.finished"

	EventGroupStarted  EventKind = "group.started"
	EventGroupFinished EventKind = "group.finished"
	// EventGroupReset records a group's recovery handler starting: everything
	// in the group goes back to never started. See
	// [GroupNode.RecoverOnFailure].
	EventGroupReset EventKind = "group.reset"

	EventStepStarted EventKind = "step.started"
	// EventStepRunning records that Check found the work not done and Run is
	// about to be called. Rollback uses it to tell which steps changed anything.
	EventStepRunning  EventKind = "step.running"
	EventStepFinished EventKind = "step.finished"

	// EventGateWaiting records a gate starting to wait, with its deadline.
	EventGateWaiting EventKind = "gate.waiting"
	// EventGateReleased records what released a gate: an approval, or its
	// timeout under the policy given. A gate a stop interrupts is not
	// released.
	EventGateReleased EventKind = "gate.released"

	// EventValue is a value recorded by a step with Record.
	EventValue EventKind = "value"
	// EventItem is a step's result for one target.
	EventItem EventKind = "item"

	EventOutput   EventKind = "output"
	EventProgress EventKind = "progress"
	EventMessage  EventKind = "message"
	EventCustom   EventKind = "custom"
	// EventStepRetry records a failed try that will be retried.
	EventStepRetry EventKind = "step.retry"

	// EventStopRequested records that the run was asked to stop.
	EventStopRequested EventKind = "stop.requested"

	// EventUndoStarted and EventUndoFinished record a rollback undoing a
	// step. See [Rollback].
	EventUndoStarted  EventKind = "undo.started"
	EventUndoFinished EventKind = "undo.finished"
)

// Durable reports whether events of this kind must be stored before the run
// moves on.
func (k EventKind) Durable() bool {
	switch k {
	case EventRunStarted, EventRunFinished, EventGroupStarted, EventGroupFinished, EventGroupReset,
		EventStepStarted, EventStepRunning, EventStepFinished, EventValue, EventItem, EventStopRequested,
		EventGateWaiting, EventGateReleased, EventUndoStarted, EventUndoFinished:
		return true
	case EventOutput, EventProgress, EventMessage, EventCustom, EventStepRetry:
		return false
	default:
		return true
	}
}

// Outcome is how a run, group or step finished.
type Outcome string

// The outcomes.
const (
	OutcomePassed    Outcome = "passed"
	OutcomeSkipped   Outcome = "skipped"
	OutcomeFailed    Outcome = "failed"
	OutcomeStopped   Outcome = "stopped"
	OutcomeCancelled Outcome = "cancelled"
	// OutcomeUndone is a step a rollback undid.
	OutcomeUndone Outcome = "undone"
)

// Reason explains a skipped or recovered outcome.
type Reason string

// The reasons.
const (
	// ReasonDone means Check reported the work already done, and Run was not
	// called.
	ReasonDone Reason = "done"
	// ReasonEarlierAttempt means the step passed in an earlier attempt and was
	// trusted without checking.
	ReasonEarlierAttempt Reason = "earlier-attempt"
	// ReasonRecovered means Check found done the work of a Run that had been
	// interrupted or had failed.
	ReasonRecovered Reason = "recovered"
	// ReasonSettled means the group finished and passed in an earlier attempt.
	ReasonSettled Reason = "settled"
	// ReasonNotReached means the run ended before the element started.
	ReasonNotReached Reason = "not-reached"
	// ReasonGroupSkipped means a step in the element's group returned
	// ErrSkipGroup before the element started.
	ReasonGroupSkipped Reason = "group-skipped"
	// ReasonRunSkipped means a step returned ErrSkipRun before the element
	// started.
	ReasonRunSkipped Reason = "run-skipped"
	// ReasonNoUndo means a rollback left a step as it was because the step
	// has no Undo.
	ReasonNoUndo Reason = "no-undo"
)

// Skip is the flow control a step's Run returned: [ErrSkipGroup] or
// [ErrSkipRun]. It is recorded on the step's step.finished event, so a
// resumed run can apply it again without running the step.
type Skip string

// The skips.
const (
	SkipGroup Skip = "skip-group"
	SkipRun   Skip = "skip-run"
)

// ErrorKind classifies an error recorded in an [Event].
type ErrorKind string

// The error kinds.
const (
	ErrorPlain     ErrorKind = "error"
	ErrorCancelled ErrorKind = "cancelled"
	ErrorTimeout   ErrorKind = "timeout"
	ErrorPanic     ErrorKind = "panic"
	// ErrorDeclined is a gate that timed out under [TimeoutDecline].
	ErrorDeclined ErrorKind = "declined"
)

// ErrorInfo is an error as data, the same in the journal, in observers and in
// viewers in other processes.
type ErrorInfo struct {
	Kind    ErrorKind `json:"kind"`
	Message string    `json:"message"`
	// Stack is the goroutine stack of a panic.
	Stack string `json:"stack,omitempty"`
}

// Level is the severity of a message event.
type Level string

// The message levels.
const (
	LevelInfo Level = "info"
	LevelWarn Level = "warn"
)

// ItemStatus is a step's result for one target.
type ItemStatus string

// The item statuses.
const (
	ItemPassed  ItemStatus = "passed"
	ItemFailed  ItemStatus = "failed"
	ItemSkipped ItemStatus = "skipped"
)

// Event is one line of the journal, and what observers receive. Which fields
// are set depends on Kind.
type Event struct {
	V    int       `json:"v"`
	Seq  int64     `json:"seq"`
	TS   time.Time `json:"ts"`
	Kind EventKind `json:"kind"`
	// Run identifies the attempt that wrote the event.
	Run  string `json:"run,omitempty"`
	Path string `json:"path,omitempty"`

	// run.started.
	Attempt     int       `json:"attempt,omitempty"`
	Plan        *NodeSpec `json:"plan,omitempty"`
	Fingerprint string    `json:"fingerprint,omitempty"`
	Labels      L         `json:"labels,omitempty"`
	// Inputs is what the attempt was given with WithInputs, with every
	// Secret left out. InputsDigest identifies it: a hash of Inputs without
	// its null and empty members.
	Inputs       json.RawMessage `json:"inputs,omitempty"`
	InputsDigest string          `json:"inputsDigest,omitempty"`
	// Forced marks an attempt made with Force.
	Forced bool `json:"forced,omitempty"`
	// Rollback marks an attempt made by [Rollback].
	Rollback bool `json:"rollback,omitempty"`
	// PID and Host say which process wrote the run.
	PID  int    `json:"pid,omitempty"`
	Host string `json:"host,omitempty"`

	// *.finished and step.retry.
	Outcome    Outcome    `json:"outcome,omitempty"`
	Reason     Reason     `json:"reason,omitempty"`
	Skip       Skip       `json:"skip,omitempty"`
	Error      *ErrorInfo `json:"error,omitempty"`
	DurationMs int64      `json:"ms,omitempty"`
	Try        int        `json:"try,omitempty"`

	// value and progress.
	Key   string          `json:"key,omitempty"`
	Value json.RawMessage `json:"value,omitempty"`

	// item, message and progress.
	Item    string     `json:"item,omitempty"`
	Status  ItemStatus `json:"status,omitempty"`
	Level   Level      `json:"level,omitempty"`
	Message string     `json:"message,omitempty"`
	Current int        `json:"current,omitempty"`
	Total   int        `json:"total,omitempty"`

	// output. Continued marks every piece of a split long line but the first.
	Stream    string `json:"stream,omitempty"`
	Line      string `json:"line,omitempty"`
	Continued bool   `json:"continued,omitempty"`

	// custom.
	Type string          `json:"type,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`

	// gate.waiting: what the gate asks, and when it times out, if it does.
	Prompt   string    `json:"prompt,omitempty"`
	Deadline time.Time `json:"deadline,omitzero"`
	// gate.released: the timeout policy applied, if it timed out. By and Who
	// below say what released it otherwise.
	Policy TimeoutPolicy `json:"policy,omitempty"`

	// stop.requested and gate.released: how the run was asked to stop, how,
	// and by whom. UID is the user that owned a control file's request.
	Mode StopMode `json:"mode,omitempty"`
	By   string   `json:"by,omitempty"`
	Who  string   `json:"who,omitempty"`
	UID  *int     `json:"uid,omitempty"`
}
