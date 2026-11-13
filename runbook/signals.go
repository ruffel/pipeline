package runbook

import (
	"os"
	"os/signal"
	"slices"
	"sync"
	"syscall"
)

// SignalOptions configures [HandleSignals].
type SignalOptions struct {
	// Stop are the signals that stop the run. Nil means [os.Interrupt] and
	// SIGTERM; an empty, non-nil slice means none.
	Stop []os.Signal
	// Detach are the signals that say the terminal has gone, such as
	// SIGHUP. Nil means SIGHUP; an empty, non-nil slice means none. A
	// signal in Stop as well is a stop signal only.
	Detach []os.Signal
	// OnSignal, if set, is told of each stop and detach signal once it has
	// been acted on, such as to print that the run is stopping. It is called
	// from the handler's goroutine, one signal at a time, so while it blocks
	// the next signal waits: it should not block, as a write to a full pipe
	// or a stopped terminal can.
	OnSignal func(os.Signal)
}

// HandleSignals lets signals stop a run through s, until the function it
// returns, release, is called. It is never installed by [Run]; a program
// calls it when it wants this.
//
// The first stop signal stops the run gracefully, with [Stopper.Stop], and
// the second cancels it, with [Stopper.Cancel]; the signal's name, such as
// "SIGINT", is recorded as who asked. After the second, stop signals are no
// longer handled, so a third has its default effect, which for SIGINT and
// SIGTERM ends the process. The journal then shows the running steps as
// interrupted, and a later run resumes them.
//
// A detach signal is only reported to OnSignal: the run goes on, though the
// terminal is gone, so writes to it fail and reads of it end, as a
// [StdinApprover] sees.
//
// SIGPIPE is handled too, so a write to a standard output or error whose
// reader has gone fails with EPIPE instead of ending the process.
//
// release stops handling all of these, and restores their default effect
// unless something else in the program handles them. It waits for a call to
// OnSignal in progress, and is safe to call more than once.
func HandleSignals(s *Stopper, opts SignalOptions) func() {
	stopSigs, detachSigs, pipeSigs := signalSets(opts)
	stops, detaches, pipes := notify(stopSigs), notify(detachSigs), notify(pipeSigs)
	quit := make(chan struct{})

	var wg sync.WaitGroup

	wg.Go(func() { handleSignals(s, opts.OnSignal, stops, detaches, pipes, quit) })

	return sync.OnceFunc(func() {
		for _, ch := range []chan os.Signal{stops, detaches, pipes} {
			if ch != nil {
				signal.Stop(ch)
			}
		}

		close(quit)
		wg.Wait()
	})
}

// signalSets returns the signals to handle as stop signals, as detach
// signals, and only to ignore. Each signal is in one set: a stop signal
// listed to detach too is a stop signal only, so that once stop signals are
// let go, a third has its default effect. SIGPIPE is ignored unless it is
// listed already.
func signalSets(opts SignalOptions) ([]os.Signal, []os.Signal, []os.Signal) {
	stopSigs, detachSigs := opts.Stop, opts.Detach

	if stopSigs == nil {
		stopSigs = []os.Signal{os.Interrupt, syscall.SIGTERM}
	}

	if detachSigs == nil {
		detachSigs = []os.Signal{syscall.SIGHUP}
	}

	detachSigs = slices.DeleteFunc(slices.Clone(detachSigs), func(sig os.Signal) bool {
		return slices.Contains(stopSigs, sig)
	})

	var pipeSigs []os.Signal
	if !slices.Contains(stopSigs, os.Signal(syscall.SIGPIPE)) && !slices.Contains(detachSigs, os.Signal(syscall.SIGPIPE)) {
		pipeSigs = []os.Signal{syscall.SIGPIPE}
	}

	return stopSigs, detachSigs, pipeSigs
}

// notify returns a channel the signals arrive on, or nil if there are none.
// (Notify with no signals relays every signal.)
func notify(sigs []os.Signal) chan os.Signal {
	if len(sigs) == 0 {
		return nil
	}

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, sigs...)

	return ch
}

func handleSignals(s *Stopper, onSignal func(os.Signal), stops, detaches, pipes chan os.Signal, quit <-chan struct{}) {
	report := func(sig os.Signal) {
		if onSignal != nil {
			onSignal(sig)
		}
	}

	stopped := false

	for {
		select {
		case sig := <-stops:
			// Act first: an OnSignal that blocks must not keep the run
			// from stopping.
			if !stopped {
				s.Stop(signalName(sig))

				stopped = true
			} else {
				s.Cancel(signalName(sig))

				// The next one has its default effect.
				signal.Stop(stops)

				stops = nil
			}

			report(sig)
		case sig := <-detaches:
			report(sig)
		case <-pipes:
			// The write that raised it fails with EPIPE.
		case <-quit:
			return
		}
	}
}

// signalName is how a signal is recorded as who asked for a stop.
func signalName(sig os.Signal) string {
	switch sig {
	case os.Interrupt:
		return "SIGINT"
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGHUP:
		return "SIGHUP"
	case syscall.SIGQUIT:
		return "SIGQUIT"
	}

	if name := osSignalName(sig); name != "" {
		return name
	}

	return sig.String()
}
