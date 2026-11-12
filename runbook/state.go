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
	// StatusStopped means a stop kept some of the group's children from
	// starting, and none of those that ran failed.
	StatusStopped Status = "stopped"
)

// State is a journal replayed: the plan, every attempt, and where each
// element, recorded value and item stands. Resume, rollback and viewers all
// read the same State, so they cannot disagree.
type State struct {
	// Plan is the plan of the latest attempt.
	Plan        *NodeSpec
	Fingerprint string
	// Labels are the run labels of the latest attempt.
	Labels L
	// Inputs is what the latest attempt was given with WithInputs, encoded,
	// with every Secret left out. InputsDigest identifies it.
	Inputs       json.RawMessage
	InputsDigest string
	Attempts     []AttemptState
	// LastSeq is the sequence number of the last event replayed. A viewer
	// follows the journal from the event after it.
	LastSeq int64
	// Live reports whether a process held the journal open for writing when
	// [Inspect] read it. It is false for states from other sources.
	Live bool

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
	// Stop is the stop the attempt was asked for, if any.
	Stop *StopRequest
}

// ElementState is where one group or step stands.
type ElementState struct {
	Status Status
	Reason Reason
	Error  *ErrorInfo
	// Run is the attempt that last started or finished the element.
	Run string
	// Done means the element's work is complete: it passed, was skipped
	// because it was already done, or is a gate that was released and not
	// declined. A later failure clears it.
	Done bool
	// RunCalled means some attempt called the step's Run, so it may have
	// changed the system.
	RunCalled bool
	// Signal is the flow control the step's Run returned when it last
	// finished, if any.
	Signal Signal
	// Waiting means a gate is waiting for approval: it has started waiting
	// and has not been released or finished. If its runner has gone, it is
	// waiting for nothing; see [State.Live].
	Waiting bool
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
	s := newState()

	for _, e := range events {
		if err := s.add(e); err != nil {
			return nil, err
		}
	}

	return s, nil
}

func newState() *State {
	return &State{
		elements: map[string]ElementState{},
		values:   map[string]map[string]json.RawMessage{},
		items:    map[string]map[string]ItemState{},
	}
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

// DecodeInputs decodes into v the inputs the latest attempt recorded, and
// reports whether there were any. A tool resuming a run calls it on the
// state from [Inspect] to get back the answers the plan was built from,
// instead of asking again. Every [Secret] comes back empty and has to be
// supplied again.
func (s *State) DecodeInputs(v any) (bool, error) {
	if len(s.Inputs) == 0 {
		return false, nil
	}

	if err := json.Unmarshal(s.Inputs, v); err != nil {
		return true, fmt.Errorf("runbook: decode inputs: %w", err)
	}

	return true, nil
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

// add replays one more event, which must follow the last one.
func (s *State) add(e Event) error {
	if err := s.follows(e.V, e.Seq); err != nil {
		return err
	}

	s.LastSeq = e.Seq
	s.apply(e)

	return nil
}

// follows checks an event's version and place in the sequence.
func (s *State) follows(v int, seq int64) error {
	if v > SchemaVersion {
		return fmt.Errorf("runbook: event %d: schema version %d is newer than this reader's %d", seq, v, SchemaVersion)
	}

	if seq <= s.LastSeq {
		return fmt.Errorf("runbook: event %d follows event %d: sequence numbers must increase", seq, s.LastSeq)
	}

	return nil
}

// addLine replays one more journal line. Output, progress, messages, custom
// events and retries change no state, so they are recognised from the start
// of the line and passed over without being decoded; everything else is
// decoded and applied.
func (s *State) addLine(line []byte) error {
	if env, ok := peekEvent(line); ok && env.chatter {
		if err := s.follows(env.v, env.seq); err != nil {
			return err
		}

		s.LastSeq = env.seq

		return nil
	}

	e, err := decodeEvent(line)
	if err != nil {
		return err
	}

	return s.add(e)
}

func (s *State) apply(e Event) {
	switch e.Kind {
	case EventRunStarted, EventRunFinished, EventStopRequested:
		s.applyAttempt(e)
	case EventGroupStarted, EventStepStarted:
		el := s.elements[e.Path]
		el.Status, el.Reason, el.Error, el.Run, el.Waiting = StatusStarted, "", nil, e.Run, false
		s.elements[e.Path] = el
	case EventGateWaiting, EventGateReleased:
		el := s.elements[e.Path]
		el.Waiting = e.Kind == EventGateWaiting
		// A gate's work is done once it is released, even if the run
		// ends before the gate finishes, unless its timeout declined it.
		if e.Kind == EventGateReleased && e.Policy != TimeoutDecline {
			el.Done = true
		}

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

// applyAttempt applies an event about the attempt as a whole.
func (s *State) applyAttempt(e Event) {
	if e.Kind == EventRunStarted {
		if e.Forced {
			s.startOver()
		}

		s.Plan = e.Plan
		s.Fingerprint = e.Fingerprint
		s.Labels = e.Labels
		s.Inputs, s.InputsDigest = e.Inputs, e.InputsDigest
		s.Attempts = append(s.Attempts, AttemptState{Run: e.Run, Attempt: e.Attempt, Started: e.TS})

		return
	}

	// The rest belong to the attempt that is open.
	n := len(s.Attempts)
	if n == 0 || s.Attempts[n-1].Run != e.Run {
		return
	}

	switch e.Kind { //nolint:exhaustive // Only the attempt's own events reach here.
	case EventRunFinished:
		s.Attempts[n-1].Finished = e.TS
		s.Attempts[n-1].Outcome = e.Outcome
	case EventStopRequested:
		// A cancel outranks a graceful stop, whichever came first.
		if cur := s.Attempts[n-1].Stop; cur == nil || cur.Mode != StopCancel {
			s.Attempts[n-1].Stop = &StopRequest{Mode: e.Mode, By: e.By, Who: e.Who, UID: e.UID}
		}
	}
}

// startOver applies a forced attempt: it does everything again, so nothing
// done before it counts as done after it, whether or not the forced attempt
// got that far. Element completion, signals and item results are forgotten.
// Recorded values are kept, and so is the fact that a step's Run was once
// called, which rollback needs.
func (s *State) startOver() {
	for path, el := range s.elements {
		s.elements[path] = ElementState{Status: StatusNotReached, RunCalled: el.RunCalled}
	}

	clear(s.items)
}

func (s *State) finish(e Event) {
	el := s.elements[e.Path]
	el.Reason, el.Error, el.Run, el.Waiting = e.Reason, e.Error, e.Run, false

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
	case OutcomeStopped:
		el.Status, el.Done = StatusStopped, false
	case OutcomeFailed, OutcomeCancelled:
		el.Status, el.Done = StatusFailed, false
	}

	s.elements[e.Path] = el
}
