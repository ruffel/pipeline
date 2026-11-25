package pipeline

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
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
	// Removed lists the elements that a change of plan took out, under
	// [WithPlanChange], and that no later plan put back: each as it was last
	// planned, without its children. [State.Element] still says where each
	// one stood, and what it recorded stays readable.
	Removed []NodeSpec
	// Live reports whether a process held the journal open for writing when
	// [Inspect] read it. It is false for states from other sources.
	Live bool

	elements map[string]ElementState
	// kinds is the kind of every element of the latest attempt's plan, and
	// marked are the steps that plan marks irreversible.
	kinds  map[string]Kind
	marked map[string]bool
	values map[string]map[string]json.RawMessage
	items  map[string]map[string]ItemState
	// gates are the paths of the gates that have waited.
	gates map[string]bool
	// ran is the first attempt that called the Run of each step, by path.
	// Unlike RunCalled, a group's reset does not forget it: a recovery step
	// can't take back what an irreversible step did. Only an Undo that
	// passes does.
	ran map[string]int
	// noReturn are the steps whose Run an attempt called while that attempt's
	// own plan marked them irreversible, by path. It is what the step was
	// when it ran, so nothing forgets it: not a later plan that drops the
	// marker or the path, a group's reset, a forced attempt or an undo.
	noReturn map[string]noReturn
	// begun are the steps and gates that an attempt has started, by path,
	// whatever became of them, and that nothing has taken back since. A
	// step recorded as skipped because its group or the run ended before
	// reaching it has not begun.
	begun map[string]bool
	// orders are the orders that OrderFrom groups have settled, by path:
	// the names of each one's children, as its start recorded them. A group
	// that goes back to never started forgets its order.
	orders map[string][]string
	// ranIn is the order each such group's children last ran in, by path.
	// Unlike orders, a forced attempt does not forget it: like RunCalled, it
	// is what a rollback goes by, and the steps that ran are still to undo.
	ranIn map[string][]string
}

// noReturn is a step's Run called while the step was irreversible.
type noReturn struct {
	// attempt is the first attempt that called it.
	attempt int
	// unplanned means the journal has no plan for that attempt, or one that
	// does not name the step: whether it was irreversible can't be told, so
	// it is taken to have been.
	unplanned bool
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
	// Rollback marks an attempt made by [Rollback], which undid steps
	// instead of running them.
	Rollback bool
	// InputsChanged are the fields of the inputs that differ from the
	// attempt before, as JSON pointers, when the attempt accepted a change:
	// under [WithInputConsumers], [WithInputChange] or [WithNewInputs].
	InputsChanged []string
}

// ElementState is where one group or step stands. The plan's root is a group
// like any other: it starts and finishes with each attempt that runs it.
//
// A rollback changes where elements stand. A step it undoes goes back to
// [StatusNotReached]. A group around such a step is then [StatusStarted],
// part-way through, while work beneath it is still done: a step that could
// not be undone, or a gate still released. Once nothing beneath it is left
// but what Observe steps read, the group goes back to StatusNotReached, and
// so does everything beneath it.
type ElementState struct {
	Status Status
	Reason Reason
	Error  *ErrorInfo
	// UndoError is the error from the step's Undo, when a rollback's latest
	// try at it failed. The step keeps the Status its Run left it with.
	UndoError *ErrorInfo
	// Run is the attempt that last started or finished the element.
	Run string
	// Done means the element's work is complete: it passed, was skipped
	// because it was already done, or is a gate that was released and not
	// declined. A later failure clears it.
	Done bool
	// RunCalled means some attempt called the step's Run, so it may have
	// changed the system.
	RunCalled bool
	// Skip is the flow control the step's Run returned when it last
	// finished, if any.
	Skip Skip
	// Waiting means a gate is waiting for approval: it has started waiting
	// and has not been released or finished. If its runner has gone, it is
	// waiting for nothing; see [State.Live].
	Waiting bool
}

// ItemState is the latest result recorded for one target of a step.
type ItemState struct {
	Status  ItemStatus
	Message string
	// Data is what the step gave with [StepContext.ItemWith], if anything.
	Data json.RawMessage
	Run  string
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
		kinds:    map[string]Kind{},
		marked:   map[string]bool{},
		values:   map[string]map[string]json.RawMessage{},
		items:    map[string]map[string]ItemState{},
		gates:    map[string]bool{},
		ran:      map[string]int{},
		noReturn: map[string]noReturn{},
		begun:    map[string]bool{},
		orders:   map[string][]string{},
		ranIn:    map[string][]string{},
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

// Values returns every value recorded under path, by key: the latest for
// each, from any attempt. It is for a viewer that shows all a step has
// recorded without knowing its keys.
func (s *State) Values(path string) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(s.values[path]))
	maps.Copy(out, s.values[path])

	return out
}

// Items returns the latest result for each target of the step at path.
func (s *State) Items(path string) map[string]ItemState {
	out := make(map[string]ItemState, len(s.items[path]))
	maps.Copy(out, s.items[path])

	return out
}

// Order returns the order in which the children of the group at path run,
// by name, if it is a [GroupNode.OrderFrom] group that has settled one: it
// has started, and has not gone back to never started since. Otherwise it
// returns nil, and the group's children run in the plan's order. A group
// with no children has no order to settle.
func (s *State) Order(path string) []string {
	return slices.Clone(s.orders[path])
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
		return true, fmt.Errorf("pipeline: decode inputs: %w", err)
	}

	return true, nil
}

// Draining returns the [GroupNode.DrainOnStop] groups that a graceful stop
// is waiting for, in plan order: those that had started when the latest
// attempt was asked to stop, and have not finished. The stop takes effect
// when they end.
//
// It is empty once the attempt has finished, when no stop is pending, and
// when the run was asked to cancel, which waits for nothing. If the
// attempt's runner has gone, it died part-way through them, and nothing is
// finishing them; see [State.Live].
func (s *State) Draining() []string {
	n := len(s.Attempts)
	if n == 0 || s.Plan == nil {
		return nil
	}

	last := s.Attempts[n-1]
	if last.Outcome != "" || last.Stop == nil || last.Stop.Mode != StopGraceful {
		return nil
	}

	var groups []string

	for _, spec := range flatten(*s.Plan) {
		if el := s.elements[spec.Path]; spec.DrainOnStop && el.Status == StatusStarted && el.Run == last.Run {
			groups = append(groups, spec.Path)
		}
	}

	return groups
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

// PointOfNoReturn returns the path of the first step that keeps [Rollback]
// from going back, or "" if there is none. Once there is one, Rollback
// refuses the journal with [ErrIrreversible], so a status command can say
// that rolling back is no longer possible without trying it.
//
// A step is one if an attempt called its Run while that attempt's plan
// marked it [StepNode.Irreversible]. The journal remembers that whatever
// comes after: a later plan that drops the marker, or the step, does not
// move the point back, and neither does a group's reset by its recovery
// step. What an irreversible step did is still done. A step that ran before
// the latest plan marked it counts too.
//
// The first is the first in the order of the latest attempt's plan, and
// then of the steps that a change of plan removed.
func (s *State) PointOfNoReturn() string {
	path, _, _ := s.pointOfNoReturn(s.Plan)

	return path
}

// pointOfNoReturn returns the first step that keeps a rollback of plan from
// starting, and the attempt that made it one: the first in plan order, then
// among the steps a change of plan removed.
func (s *State) pointOfNoReturn(plan *NodeSpec) (string, noReturn, bool) {
	var specs []NodeSpec

	if plan != nil {
		specs = flatten(*plan)
	}

	// A step that a change of plan removed is no easier to undo.
	for _, n := range append(specs, s.Removed...) {
		at, held := s.noReturn[n.Path]
		attempt, ran := s.ran[n.Path]

		switch {
		case held && !at.unplanned:
			// What the journal says outright comes first, so the attempt
			// named is the same whatever plan is asking.
			return n.Path, at, true
		case ran && n.Irreversible:
			// A step the plan marks now, that ran before it did: nothing
			// says its Run was any easier to undo then.
			return n.Path, noReturn{attempt: attempt}, true
		case held:
			return n.Path, at, true
		}
	}

	// Only a journal without its plans has a step that neither names.
	if rest := slices.Sorted(maps.Keys(s.noReturn)); len(rest) > 0 {
		return rest[0], s.noReturn[rest[0]], true
	}

	return "", noReturn{}, false
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
		return fmt.Errorf("pipeline: event %d: schema version %d is newer than this reader's %d", seq, v, SchemaVersion)
	}

	if seq <= s.LastSeq {
		return fmt.Errorf("pipeline: event %d follows event %d: sequence numbers must increase", seq, s.LastSeq)
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
		s.start(e)
	case EventGateWaiting, EventGateReleased:
		s.gate(e)
	case EventStepRunning:
		s.running(e.Path)
	case EventGroupFinished, EventStepFinished:
		s.finish(e)
	case EventGroupReset:
		s.reset(e.Path)
	case EventUndoFinished:
		s.undone(e)
	case EventUndoStarted:
		// The step is undone only once its undo passes. A new try at it
		// replaces what the last one failed with.
		if el, ok := s.elements[e.Path]; ok {
			el.UndoError = nil
			s.elements[e.Path] = el
		}
	case EventValue:
		if s.values[e.Path] == nil {
			s.values[e.Path] = map[string]json.RawMessage{}
		}

		s.values[e.Path][e.Key] = e.Value
	case EventItem:
		if s.items[e.Path] == nil {
			s.items[e.Path] = map[string]ItemState{}
		}

		s.items[e.Path][e.Item] = ItemState{Status: e.ItemStatus, Message: e.Message, Data: e.Data, Run: e.Run}
	case EventOutput, EventProgress, EventMessage, EventCustom, EventStepRetry, EventPlanChanged:
		// Best-effort chatter: viewers render it, but it changes no state.
		// So do they a change of plan, which run.started has applied.
	}
}

// start applies a group, step or gate starting.
func (s *State) start(e Event) {
	el := s.elements[e.Path]
	el.Status, el.Reason, el.Error, el.Run, el.Waiting = StatusStarted, "", nil, e.Run, false
	s.elements[e.Path] = el

	if e.Kind == EventStepStarted {
		s.begun[e.Path] = true
	}

	// Every start of a group that runs in a settled order says the order.
	// One that says none runs in the plan's, whatever the group ran in
	// before a reset or a forced attempt.
	if e.Kind == EventGroupStarted {
		if len(e.Order) > 0 {
			s.orders[e.Path], s.ranIn[e.Path] = e.Order, e.Order
		} else {
			delete(s.orders, e.Path)
			delete(s.ranIn, e.Path)
		}
	}
}

// running applies a step's Run being called. What the step was when it ran
// is settled here, from the plan of the attempt that ran it, and not from
// whatever plan a later attempt brings.
func (s *State) running(path string) {
	el := s.elements[path]
	el.RunCalled = true
	s.elements[path] = el

	attempt := len(s.Attempts)

	if _, ran := s.ran[path]; !ran {
		s.ran[path] = attempt
	}

	_, planned := s.kinds[path]

	if _, held := s.noReturn[path]; !held && (s.marked[path] || !planned) {
		s.noReturn[path] = noReturn{attempt: attempt, unplanned: !planned}
	}
}

// applyAttempt applies an event about the attempt as a whole.
func (s *State) applyAttempt(e Event) {
	if e.Kind == EventRunStarted {
		if e.Forced {
			s.startOver()
		}

		s.replan(e.Plan, e.Fingerprint)
		s.Labels = e.Labels
		s.Inputs, s.InputsDigest = e.Inputs, e.InputsDigest
		s.Attempts = append(s.Attempts, AttemptState{
			Run: e.Run, Attempt: e.Attempt, Started: e.TS, Rollback: e.Rollback, InputsChanged: e.InputsChanged,
		})

		clear(s.kinds)
		clear(s.marked)

		if e.Plan != nil {
			s.index(*e.Plan)
		}

		// The root has no group events of its own: a run starts and
		// finishes it. A rollback leaves it to what it undoes.
		if !e.Rollback {
			el := s.elements[e.Path]
			el.Status, el.Reason, el.Error, el.Run, el.Done = StatusStarted, "", nil, e.Run, false
			s.elements[e.Path] = el
		}

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

		if !s.Attempts[n-1].Rollback {
			s.finish(e)
		}

		if s.Attempts[n-1].Rollback && e.Outcome == OutcomePassed {
			s.unreleaseGates()
		}
	case EventStopRequested:
		// A cancel outranks a graceful stop, whichever came first.
		if cur := s.Attempts[n-1].Stop; cur == nil || cur.Mode != StopCancel {
			s.Attempts[n-1].Stop = &StopRequest{Mode: e.Mode, By: e.By, Who: e.Who, UID: e.UID}
		}
	}
}

// replan applies the plan an attempt started with. If its shape differs from
// the one before it, the attempt was made under WithPlanChange. The change
// is worked out here, from the two plans, and not from the plan.changed
// event that follows: a crash between the two events must not lose it.
func (s *State) replan(plan *NodeSpec, fingerprint string) {
	if s.Plan != nil && plan != nil && s.Fingerprint != fingerprint {
		s.changePlan(diffPlans(*s.Plan, *plan))
	}

	s.Plan, s.Fingerprint = plan, fingerprint
}

// planDiff is what changed between the plans of two attempts.
type planDiff struct {
	// added and removed are the elements only in the new plan and only in
	// the old one, in plan order, without their children.
	added, removed []NodeSpec
	// rekinded are the paths in both plans whose kind differs.
	rekinded []string
}

// diffPlans compares two plans by path.
func diffPlans(before, after NodeSpec) planDiff {
	was, now := flatten(before), flatten(after)

	index := func(specs []NodeSpec) map[string]Kind {
		kinds := make(map[string]Kind, len(specs))
		for _, n := range specs {
			kinds[n.Path] = n.Kind
		}

		return kinds
	}

	wasKind, nowKind := index(was), index(now)

	var d planDiff

	for _, n := range was {
		if _, ok := nowKind[n.Path]; !ok {
			d.removed = append(d.removed, n)
		}
	}

	for _, n := range now {
		switch kind, ok := wasKind[n.Path]; {
		case !ok:
			d.added = append(d.added, n)
		case kind != n.Kind:
			d.rekinded = append(d.rekinded, n.Path)
		}
	}

	return d
}

// flatten lists n and everything beneath it in plan order, a failure handler
// after its group's children, each without its own children.
func flatten(n NodeSpec) []NodeSpec {
	flat := n
	flat.Children, flat.OnFailure = nil, nil

	out := []NodeSpec{flat}

	for _, c := range n.Children {
		out = append(out, flatten(c)...)
	}

	if n.OnFailure != nil {
		out = append(out, flatten(*n.OnFailure)...)
	}

	return out
}

// paths returns the paths of specs.
func paths(specs []NodeSpec) []string {
	out := make([]string, 0, len(specs))
	for _, n := range specs {
		out = append(out, n.Path)
	}

	return out
}

// changePlan applies a change of plan. What was removed keeps its record and
// is listed in Removed, until a later plan puts it back. A group that had
// settled and now has something new beneath it is settled no longer, so a
// resume enters it.
func (s *State) changePlan(d planDiff) {
	back := map[string]bool{}

	for _, n := range d.added {
		back[n.Path] = true

		s.unsettle(n.Path)
	}

	s.Removed = slices.DeleteFunc(s.Removed, func(n NodeSpec) bool { return back[n.Path] })
	s.Removed = append(s.Removed, d.removed...)
}

// unsettle marks the groups around path as part-way through, if they had
// finished their work.
func (s *State) unsettle(path string) {
	for i := strings.LastIndexByte(path, '/'); i >= 0; i = strings.LastIndexByte(path, '/') {
		path = path[:i]

		if el, ok := s.elements[path]; ok && el.Done {
			el.Status, el.Reason, el.Done = StatusStarted, "", false
			s.elements[path] = el
		}
	}
}

// startOver applies a forced attempt: it does everything again, so nothing
// done before it counts as done after it, whether or not the forced attempt
// got that far. Element completion, signals, item results and the orders
// that groups settled are forgotten. Recorded values are kept, and so are the
// fact that a step's Run was once called and the order a group's children
// ran in, which rollback needs.
func (s *State) startOver() {
	for path, el := range s.elements {
		s.elements[path] = ElementState{Status: StatusNotReached, RunCalled: el.RunCalled}
	}

	clear(s.items)
	clear(s.orders)
}

// reset applies a group's reset: everything beneath it goes back to never
// started, as if no attempt had touched it. Unlike a forced attempt, it also
// forgets that Runs were called and the values they recorded, so a later
// rollback leaves the group to its recovery handler. The group has nothing
// left of what it did, so the order it settled goes too, with those of the
// groups beneath it: each reads its order again when it next starts.
func (s *State) reset(path string) {
	beneath := func(p string) bool { return strings.HasPrefix(p, path+"/") }

	maps.DeleteFunc(s.elements, func(p string, _ ElementState) bool { return beneath(p) })
	maps.DeleteFunc(s.values, func(p string, _ map[string]json.RawMessage) bool { return beneath(p) })
	maps.DeleteFunc(s.items, func(p string, _ map[string]ItemState) bool { return beneath(p) })
	maps.DeleteFunc(s.begun, func(p string, _ bool) bool { return beneath(p) })

	within := func(p string, _ []string) bool { return p == path || beneath(p) }

	maps.DeleteFunc(s.orders, within)
	maps.DeleteFunc(s.ranIn, within)
}

// gate applies a gate waiting or being released.
func (s *State) gate(e Event) {
	s.gates[e.Path] = true

	el := s.elements[e.Path]
	el.Waiting = e.Kind == EventGateWaiting

	// A gate's work is done once it is released, even if the run ends
	// before the gate finishes, unless its timeout declined it.
	if e.Kind == EventGateReleased && e.Policy != TimeoutDecline {
		el.Done = true
	}

	s.elements[e.Path] = el
}

// undone applies a rollback's undo of a step. A step undone goes back to
// never started, with its recorded values and items forgotten, and the
// groups around it are no longer settled. One whose undo was skipped keeps
// its outcome, and so does one whose undo failed, with the error beside it.
func (s *State) undone(e Event) {
	path := e.Path

	if e.Outcome == OutcomeFailed {
		if el, ok := s.elements[path]; ok {
			el.UndoError = e.Error
			s.elements[path] = el
		}
	}

	if e.Outcome != OutcomeUndone {
		return
	}

	delete(s.elements, path)
	delete(s.values, path)
	delete(s.items, path)
	delete(s.ran, path)
	delete(s.begun, path)
	s.unwind(path)
}

// unreleaseGates applies a rollback that passed: every gate waits again.
func (s *State) unreleaseGates() {
	for path := range s.gates {
		delete(s.elements, path)
		delete(s.begun, path)
		s.unwind(path)
	}
}

// unwind applies the loss of the element at path, undone or waiting again,
// to the groups around it, innermost first. None of them is settled any
// more, so a later attempt enters them again. While work beneath a group is
// still done, the group is part-way through: started. Once only what Observe
// steps read is left beneath it, the group and everything beneath it go back
// to never started.
func (s *State) unwind(path string) {
	for i := strings.LastIndexByte(path, '/'); i >= 0; i = strings.LastIndexByte(path, '/') {
		path = path[:i]

		if !s.workBeneath(path) {
			s.reset(path)
			delete(s.elements, path)

			continue
		}

		if el, ok := s.elements[path]; ok {
			el.Done = false

			if el.Status != StatusNotReached {
				el.Status, el.Reason, el.Error = StatusStarted, "", nil
			}

			s.elements[path] = el
		}
	}
}

// workBeneath reports whether a step or gate beneath the group at path still
// stands for something: it was reached, or its Run was called. Observe steps
// don't count: they change nothing. An element the plan doesn't name counts,
// so a journal replayed without its plan forgets nothing it shouldn't.
func (s *State) workBeneath(path string) bool {
	for p, el := range s.elements {
		if !strings.HasPrefix(p, path+"/") {
			continue
		}

		if kind, ok := s.kinds[p]; ok && (kind == KindGroup || kind == KindObserve) {
			continue
		}

		if el.Status != StatusNotReached || el.RunCalled {
			return true
		}
	}

	return false
}

// index notes the kind of n and of everything beneath it, and which of them
// are marked irreversible.
func (s *State) index(n NodeSpec) {
	s.kinds[n.Path] = n.Kind

	if n.Irreversible {
		s.marked[n.Path] = true
	}

	for _, c := range n.Children {
		s.index(c)
	}

	if n.OnFailure != nil {
		s.index(*n.OnFailure)
	}
}

func (s *State) finish(e Event) {
	el := s.elements[e.Path]
	el.Reason, el.Error, el.Run, el.Waiting = e.Reason, e.Error, e.Run, false

	// A skip belongs to the last time Run actually finished: a passed or
	// failed outcome sets or clears it, and a skip leaves it for resume.
	if e.Outcome == OutcomePassed || e.Outcome == OutcomeFailed {
		el.Skip = e.Skip
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
	case OutcomeUndone:
		// Only an undo is undone, and it never finishes a group or step.
	}

	s.elements[e.Path] = el
}
