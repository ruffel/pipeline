package runbook

import (
	"context"
	"fmt"
	"sync"
)

// StopMode says how a run was asked to stop.
type StopMode string

// The stop modes.
const (
	// StopGraceful lets running steps finish and starts nothing new.
	StopGraceful StopMode = "graceful"
	// StopCancel cancels running steps, as cancelling the run's context
	// does.
	StopCancel StopMode = "cancel"
)

// Stopper asks a run to stop gracefully: no new step starts, steps already
// running finish and are recorded, and the run finishes with outcome
// [OutcomeStopped]. Give it to [Run] with [WithStopper].
//
// A step waiting between tries is not tried again: it is recorded as failed
// with its last error. Parallel steps still queued are not started. A stop
// that comes after the work has ended changes nothing.
//
// A stopped run is unfinished, and a later Run resumes it. A Stopper stays
// stopped: use a new one for each run. It is safe for concurrent use.
type Stopper struct {
	once sync.Once
	done chan struct{}

	mu  sync.Mutex
	who string
}

// NewStopper returns a Stopper that has not been stopped.
func NewStopper() *Stopper {
	return &Stopper{done: make(chan struct{})}
}

// Stop asks the run to stop. who says who or what asked, such as an
// operator's name or "SIGTERM"; it is recorded in the journal. Only the
// first call counts.
func (s *Stopper) Stop(who string) {
	s.once.Do(func() {
		s.mu.Lock()
		s.who = who
		s.mu.Unlock()

		close(s.done)
	})
}

// Done returns a channel closed when Stop is first called.
func (s *Stopper) Done() <-chan struct{} {
	return s.done
}

func (s *Stopper) requester() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.who
}

// WithStopper lets s stop the run gracefully. See [Stopper].
func WithStopper(s *Stopper) Option {
	return func(c *config) {
		c.stopper = s
	}
}

// StopRequest is a stop asked of a run.
type StopRequest struct {
	Mode StopMode
	// By is how the request was made: "stopper" for a [Stopper], "control"
	// for [RequestStop].
	By string
	// Who is who or what asked, as they said.
	Who string
	// UID is the user that owned a [RequestStop] request's file. It is nil
	// for a Stopper.
	UID *int
}

// How a stop request was made.
const (
	stopBy        = "stopper"
	stopByControl = "control"
)

// stopSignal is a run's graceful stop: the first request wins, and is
// readable once done is closed.
type stopSignal struct {
	once sync.Once
	done chan struct{}
	req  StopRequest
}

func newStopSignal() *stopSignal {
	return &stopSignal{done: make(chan struct{})}
}

// fire asks for the stop. Only the first request counts.
func (s *stopSignal) fire(req StopRequest) {
	s.once.Do(func() {
		s.req = req
		close(s.done)
	})
}

// stopping reports whether the run has been asked to stop, and records the
// request the first time it is seen.
func (r *runner) stopping() bool {
	if r.stopper != nil {
		select {
		case <-r.stopper.Done():
			r.stop.fire(r.stopperRequest())
		default:
		}
	}

	select {
	case <-r.stop.done:
		r.noteStop()

		return true
	default:
		return false
	}
}

// stopDone is closed when the run is asked to stop gracefully. A Stopper's
// request reaches it through watchStop.
func (r *runner) stopDone() <-chan struct{} {
	return r.stop.done
}

func (r *runner) stopperRequest() StopRequest {
	return StopRequest{Mode: StopGraceful, By: stopBy, Who: r.stopper.requester()}
}

// noteStop records the graceful stop request, once. A journal failure here
// is seen by the run's next write.
func (r *runner) noteStop() {
	r.stopNoted.Do(func() {
		_ = r.emit(stopEvent(r.plan.root.path, r.stop.req))
	})
}

// cancelRequested records a request to cancel the run, once, and cancels it.
func (r *runner) cancelRequested(req StopRequest) {
	r.cancelNoted.Do(func() {
		_ = r.emit(stopEvent(r.plan.root.path, req))

		cause := "runbook: cancel requested"
		if req.Who != "" {
			cause += " by " + req.Who
		}

		r.cancel(fmt.Errorf("%s: %w", cause, context.Canceled))
	})
}

func stopEvent(path string, req StopRequest) Event {
	return Event{Kind: EventStopRequested, Path: path, Mode: req.Mode, By: req.By, Who: req.Who, UID: req.UID}
}

// watchStop records a stop request as soon as it is made, so viewers learn
// of it while steps are still running, until done is closed.
func (r *runner) watchStop(done <-chan struct{}) {
	var stopper <-chan struct{}
	if r.stopper != nil {
		stopper = r.stopper.Done()
	}

	select {
	case <-stopper:
		r.stop.fire(r.stopperRequest())
		r.noteStop()
	case <-r.stop.done:
		r.noteStop()
	case <-done:
	}
}

// halt records that a stop kept something from happening: a step from
// starting, or a step from being tried again.
func (r *runner) halt() {
	r.halted.Store(true)
}

// onlyStopped reports whether err is nothing but [ErrStopped]: the stop kept
// something from starting, and nothing failed.
func onlyStopped(err error) bool {
	if err == ErrStopped { //nolint:errorlint // The sentinel itself: anything wrapping it carries more.
		return true
	}

	joined, ok := err.(interface{ Unwrap() []error })
	if !ok || len(joined.Unwrap()) == 0 {
		return false
	}

	for _, e := range joined.Unwrap() {
		if !onlyStopped(e) {
			return false
		}
	}

	return true
}
