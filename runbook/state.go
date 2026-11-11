package runbook

import (
	"encoding/json"
	"fmt"
	"maps"
	"time"
)

// Status is where an element stands after replaying a journal.
type Status string

// The statuses.
const (
	// StatusNotReached means no attempt has started the element.
	StatusNotReached Status = "not-reached"
	// StatusStarted means the element started and has not finished: it is
	// running, or it was interrupted by a crash.
	StatusStarted Status = "started"
	StatusPassed  Status = "passed"
	StatusSkipped Status = "skipped"
	StatusFailed  Status = "failed"
)

// State is a journal replayed: the plan, every attempt, and where each
// element, recorded value and item stands. Resume, rollback and viewers all
// read the same State, so they cannot disagree.
type State struct {
	// Plan is the plan of the latest attempt.
	Plan        *NodeSpec
	Fingerprint string
	// Labels are the run labels of the latest attempt.
	Labels   L
	Attempts []AttemptState
	// LastSeq is the sequence number of the last event replayed. A viewer
	// follows the journal from the event after it.
	LastSeq int64

	elements map[string]ElementState
	values   map[string]map[string]json.RawMessage
	items    map[string]map[string]ItemState
}

// AttemptState is one run of the plan.
type AttemptState struct {
	Run      string
	Attempt  int
	Started  time.Time
	Finished time.Time
	// Outcome is empty while the attempt is running, or if it was
	// interrupted.
	Outcome Outcome
}

// ElementState is where one group or step stands.
type ElementState struct {
	Status Status
	Reason Reason
	Error  *ErrorInfo
	// Run is the attempt that last started or finished the element.
	Run string
	// Done means the element's work is complete: it passed, or was skipped
	// because it was already done. A later failure clears it.
	Done bool
	// RunCalled means some attempt called the step's Run, so it may have
	// changed the system.
	RunCalled bool
	// Signal is the flow control the step's Run returned when it last
	// finished, if any.
	Signal Signal
}

// ItemState is the latest result recorded for one target of a step.
type ItemState struct {
	Status  ItemStatus
	Message string
	Run     string
}

// Replay computes the state recorded by events, which must be in journal
// order. It refuses events from a newer schema version, and events whose
// sequence numbers don't increase.
func Replay(events []Event) (*State, error) {
	s := &State{
		elements: map[string]ElementState{},
		values:   map[string]map[string]json.RawMessage{},
		items:    map[string]map[string]ItemState{},
	}

	for _, e := range events {
		if e.V > SchemaVersion {
			return nil, fmt.Errorf("runbook: event %d: schema version %d is newer than this reader's %d", e.Seq, e.V, SchemaVersion)
		}

		if e.Seq <= s.LastSeq {
			return nil, fmt.Errorf("runbook: event %d follows event %d: sequence numbers must increase", e.Seq, s.LastSeq)
		}

		s.LastSeq = e.Seq
		s.apply(e)
	}

	return s, nil
}

// Element returns where the element at path stands. An element no attempt
// has reached is [StatusNotReached].
func (s *State) Element(path string) ElementState {
	if el, ok := s.elements[path]; ok {
		return el
	}

	return ElementState{Status: StatusNotReached}
}

// Value returns the latest value recorded under path and key, from any
// attempt.
func (s *State) Value(path, key string) (json.RawMessage, bool) {
	v, ok := s.values[path][key]

	return v, ok
}

// Items returns the latest result for each target of the step at path.
func (s *State) Items(path string) map[string]ItemState {
	out := make(map[string]ItemState, len(s.items[path]))
	maps.Copy(out, s.items[path])

	return out
}

// Unfinished reports whether the latest attempt did anything but pass: it is
// still running, crashed, failed, was stopped or was cancelled. Any of these
// can leave the system part-way through the plan.
func (s *State) Unfinished() bool {
	if len(s.Attempts) == 0 {
		return false
	}

	return s.Attempts[len(s.Attempts)-1].Outcome != OutcomePassed
}

func (s *State) apply(e Event) {
	switch e.Kind {
	case EventRunStarted:
		s.Plan = e.Plan
		s.Fingerprint = e.Fingerprint
		s.Labels = e.Labels
		s.Attempts = append(s.Attempts, AttemptState{Run: e.Run, Attempt: e.Attempt, Started: e.TS})
	case EventRunFinished:
		if n := len(s.Attempts); n > 0 && s.Attempts[n-1].Run == e.Run {
			s.Attempts[n-1].Finished = e.TS
			s.Attempts[n-1].Outcome = e.Outcome
		}
	case EventGroupStarted, EventStepStarted:
		el := s.elements[e.Path]
		el.Status, el.Reason, el.Error, el.Run = StatusStarted, "", nil, e.Run
		s.elements[e.Path] = el
	case EventStepRunning:
		el := s.elements[e.Path]
		el.RunCalled = true
		s.elements[e.Path] = el
	case EventGroupFinished, EventStepFinished:
		s.finish(e)
	case EventValue:
		if s.values[e.Path] == nil {
			s.values[e.Path] = map[string]json.RawMessage{}
		}

		s.values[e.Path][e.Key] = e.Value
	case EventItem:
		if s.items[e.Path] == nil {
			s.items[e.Path] = map[string]ItemState{}
		}

		s.items[e.Path][e.Item] = ItemState{Status: e.Status, Message: e.Message, Run: e.Run}
	case EventOutput, EventProgress, EventMessage, EventCustom, EventStepRetry:
		// Best-effort chatter: viewers render it, but it changes no state.
	}
}

func (s *State) finish(e Event) {
	el := s.elements[e.Path]
	el.Reason, el.Error, el.Run = e.Reason, e.Error, e.Run

	// A signal belongs to the last time Run actually finished: a passed or
	// failed outcome sets or clears it, and a skip leaves it for resume.
	if e.Outcome == OutcomePassed || e.Outcome == OutcomeFailed {
		el.Signal = e.Signal
	}

	switch e.Outcome {
	case OutcomePassed:
		el.Status, el.Done = StatusPassed, true
	case OutcomeSkipped:
		el.Status = StatusSkipped
		// Skipped as already done keeps or sets Done; skipped because the run
		// or group ended early leaves it as it was.
		if e.Reason == ReasonDone || e.Reason == ReasonEarlierAttempt || e.Reason == ReasonSettled {
			el.Done = true
		}
	case OutcomeFailed, OutcomeStopped, OutcomeCancelled:
		el.Status, el.Done = StatusFailed, false
	}

	s.elements[e.Path] = el
}
