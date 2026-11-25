package pipeline

import (
	"context"
	"maps"
	"time"
)

// Observer receives every event after the journal has stored it. Calls are
// serialised, in journal order, from the goroutine that wrote the event, so
// an observer that blocks stalls the run. Put one that can block for long,
// such as one writing to a socket or a pipe, behind a [NewAsyncObserver]. A
// view of the run on its own terminal is usually called directly, and has to
// be beside a [StdinApprover], so a gate's line is written before its
// question.
//
// A panic in OnEvent is recovered. That observer receives nothing more, and a
// warning is recorded.
type Observer interface {
	OnEvent(ctx context.Context, e Event)
}

// ObserverFunc adapts a function to [Observer].
type ObserverFunc func(ctx context.Context, e Event)

// OnEvent implements [Observer] by calling f.
func (f ObserverFunc) OnEvent(ctx context.Context, e Event) {
	f(ctx, e)
}

// Clock tells the time and waits. The default is the system clock; tests can
// supply their own.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// Option configures [Run].
type Option func(*config)

// ResumeMode says how a resumed run treats an Effect step that an earlier
// attempt finished.
type ResumeMode int

// The resume modes.
const (
	// ResumeVerify runs the step's Check again, and its Run if the work is
	// no longer done. A step with no Check is skipped. It is the default.
	ResumeVerify ResumeMode = iota
	// ResumeTrust skips the step without calling Check.
	ResumeTrust
)

type config struct {
	observers []Observer
	labels    L
	clock     Clock
	resume    ResumeMode
	force     bool
	redact    func(string) string
	inputs    any
	newInputs bool
	// inputChange are the hooks that decide whether changed inputs are
	// accepted, and inputConsumers what WithInputConsumers was given.
	inputChange    []func(InputChange) error
	inputConsumers map[string][]string
	strictUndo     bool
	planChange     PlanChange
	stopper        *Stopper
	controlPoll    time.Duration

	// outputBudget is MaxStepOutput, except in tests.
	outputBudget int
}

// WithObservers adds observers to the run. Nil observers are ignored.
func WithObservers(observers ...Observer) Option {
	return func(c *config) {
		for _, o := range observers {
			if o != nil {
				c.observers = append(c.observers, o)
			}
		}
	}
}

// WithRunLabels sets labels on the run, such as {"operation": "u-42"}. They
// are recorded in the run.started event.
func WithRunLabels(l L) Option {
	return func(c *config) {
		c.labels = maps.Clone(l)
	}
}

// WithResume sets how a resumed run treats Effect steps that an earlier
// attempt finished. The default is [ResumeVerify].
func WithResume(mode ResumeMode) Option {
	return func(c *config) {
		c.resume = mode
	}
}

// Force runs the whole plan again, whatever the journal says: nothing is
// skipped because an earlier attempt did it, and a journal whose latest
// attempt passed is accepted. Check still decides whether each Run is
// needed, so an Effect step with no Check has its Run called again. Values
// recorded by earlier attempts stay readable. The attempt
// is recorded as forced, and later attempts resume from it alone: what was
// done before a forced attempt no longer counts, even if the forced attempt
// failed before redoing it.
func Force() Option {
	return func(c *config) {
		c.force = true
	}
}

// WithInputs records v as the run's inputs: the answers, flags, defaults and
// discovery results the plan was built from. It is encoded as JSON and
// stored with the attempt, so a later resume can read it back with
// [State.DecodeInputs] and build the same plan without asking again.
//
// Fields of type [Secret] are never stored, and the values of those secrets
// are masked wherever they appear in what steps write.
//
// A resume must be given the same inputs: if they differ from the latest
// attempt's, [Run] returns [ErrInputsChanged] unless [WithNewInputs] is
// given. Secrets are not part of the comparison, and nor is the difference
// between a field that is null, empty, or left out.
func WithInputs(v any) Option {
	return func(c *config) {
		c.inputs = v
	}
}

// WithInputConsumers says which steps use which inputs, so that a resume can
// accept a change to an input that nothing has used yet: the usual fix
// after a failed run is to correct an address or a flag and run again.
//
// It maps a field of the inputs to the paths of the steps and groups that
// use it. A field is named by its JSON pointer into the encoded inputs, such
// as "/hosts/2/address". A pointer covers everything beneath it, and ""
// covers all the inputs.
//
// On a resume whose inputs differ from the latest attempt's, each changed
// field is checked:
//
//   - If every consumer that covers it has never started, the change is
//     accepted. A group has started once anything beneath it has. A step
//     that a rollback undid, or its group's recovery reset, has not.
//   - If a consumer has started, whatever its outcome, the attempt is
//     refused with an [*InputChangeError], which matches [ErrInputsChanged]
//     and lists each such field, its old and new value, and the consumer.
//   - If no consumer covers it, it counts as used by every step, and the
//     change is refused in the same way.
//
// An accepted change is recorded with the new attempt, as the pointers that
// changed: see [AttemptState.InputsChanged]. A consumer that the plan does
// not have is an error on every attempt, changed inputs or not.
// [WithNewInputs] accepts any change without asking.
//
// A change to a whole container, such as a list that was removed, is also a
// change to everything beneath it, so the consumers declared beneath it must
// not have started either.
func WithInputConsumers(consumers map[string][]string) Option {
	return func(c *config) {
		c.inputConsumers = maps.Clone(consumers)
	}
}

// WithInputChange sets a function that decides whether a resume may go
// ahead with inputs that differ from the latest attempt's, for a rule that
// [WithInputConsumers] can't express. It is called once, before the attempt
// starts, with what changed and the journal's state. If it returns nil, the
// change is accepted and recorded with the attempt. If it returns an error,
// the attempt is refused with an error that matches both it and
// [ErrInputsChanged].
//
// Given more than once, or with WithInputConsumers, every one must accept.
func WithInputChange(decide func(InputChange) error) Option {
	return func(c *config) {
		if decide != nil {
			c.inputChange = append(c.inputChange, decide)
		}
	}
}

// WithNewInputs lets a resume go ahead with inputs that differ from the
// latest attempt's, whatever has used them. The new inputs are recorded
// with the new attempt.
func WithNewInputs() Option {
	return func(c *config) {
		c.newInputs = true
	}
}

// PlanChange says how an attempt treats a journal whose latest attempt used
// a plan of a different shape.
type PlanChange int

// MatchByPath carries each element's record over to the element of the new
// plan with the same path. See [WithPlanChange].
const MatchByPath PlanChange = 1

// WithPlanChange lets [Run] and [Rollback] go ahead with a plan whose shape
// differs from the one the journal's latest attempt used, instead of
// refusing it with [ErrPlanChanged]. It is for a fixed build of a tool
// resuming the operation the broken build left unfinished.
//
// With [MatchByPath], elements are matched by path, which is their identity:
//
//   - An element in both plans keeps its record. If its kind differs, the
//     attempt is refused with an error that matches ErrPlanChanged and names
//     it: what a passed outcome means depends on the kind.
//   - An element only in the new plan has never started. If it is inside a
//     group that had settled, the group is no longer settled, so a resume
//     enters it and runs the new element. The group's other steps are
//     treated as on any resume.
//   - An element only in the old plan keeps its record, and the values it
//     recorded stay readable. It is listed in [State.Removed]. A later plan
//     that puts it back carries on from that record, so it must have the
//     kind it had.
//
// A step that moved to another path is one removed and one added.
//
// A group that has settled an order, as a [GroupNode.OrderFrom] group does,
// must still have the children the order names, whether or not the plan
// still has the option. An attempt whose plan adds one, or no longer has
// one, is refused with an error that matches ErrPlanChanged and names the
// group.
//
// A rollback can't undo a removed step, because its Undo went with it. If
// such a step ran and had an Undo, Rollback refuses with an error that
// matches ErrPlanChanged and names it.
//
// The attempt records the new plan, and a [EventPlanChanged] event listing
// what was added and removed. Later attempts compare with the new plan, so
// they need no option unless the plan changes again. A journal whose latest
// attempt passed is still complete: adding a step does not reopen it.
func WithPlanChange(how PlanChange) Option {
	return func(c *config) {
		c.planChange = how
	}
}

// WithStrictUndo makes [Rollback] refuse a journal, before it undoes
// anything, if a step it would reach has no Undo. Without it, such a step is
// recorded as skipped with [ReasonNoUndo] and the rollback can still pass,
// leaving that step's work in place. The error matches [ErrNoUndo] and names
// every such step. [Run] ignores the option.
func WithStrictUndo() Option {
	return func(c *config) {
		c.strictUndo = true
	}
}

// WithRedactor sets a function that removes secrets from text. It is applied
// to every message, output line, error message and panic stack, and to every
// string inside a custom event's data, before the event is written to the
// journal or given to an observer. So secrets a step prints never reach
// either.
//
// Every error [Run] returns carries a redacted message too, and still
// unwraps to the original error.
//
// Output is redacted a line at a time, before a long line is split into
// pieces. A line of [MaxRedactLine] bytes or more is replaced by a notice.
//
// Names, paths, labels, item IDs and recorded values are not redacted: they
// must simply never be secret.
//
// The function is called from several goroutines, and must be safe for
// that. If it panics, the text it was given is replaced whole.
func WithRedactor(fn func(string) string) Option {
	return func(c *config) {
		c.redact = fn
	}
}

// WithClock replaces the system clock, for tests.
func WithClock(clock Clock) Option {
	return func(c *config) {
		if clock != nil {
			c.clock = clock
		}
	}
}

type systemClock struct{}

func (systemClock) Now() time.Time {
	return time.Now()
}

func (systemClock) After(d time.Duration) <-chan time.Time {
	return time.After(d)
}
