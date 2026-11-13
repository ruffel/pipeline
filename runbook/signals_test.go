//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package runbook_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"github.com/ruffel/pipeline/runbook"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Signals reach the whole process, so the tests that send them run alone,
// each on a signal of its own.

func raise(t *testing.T, sig syscall.Signal) {
	t.Helper()

	require.NoError(t, syscall.Kill(os.Getpid(), sig))
}

// The first stop signal stops the run gracefully; the second cancels it.
func TestHandleSignals_StopThenCancel(t *testing.T) { //nolint:paralleltest // Signals are process-wide.
	stopper := runbook.NewStopper()
	seen := make(chan os.Signal, 2)

	release := runbook.HandleSignals(stopper, runbook.SignalOptions{
		Stop: []os.Signal{syscall.SIGUSR1}, Detach: []os.Signal{},
		OnSignal: func(sig os.Signal) { seen <- sig },
	})
	defer release()

	lr := startRun(t, mustPlan(t,
		runbook.Effect("wait", waitForCancel()),
		runbook.Effect("after", ok()),
	), runbook.WithStopper(stopper))

	waitFor(t, lr.rec, runbook.EventStepRunning)
	raise(t, syscall.SIGUSR1)
	waitFor(t, lr.rec, runbook.EventStopRequested)
	assert.Equal(t, syscall.SIGUSR1, <-seen)

	stop := kinds(lr.rec.all(), runbook.EventStopRequested)[0]
	assert.Equal(t, runbook.StopGraceful, stop.Mode)
	assert.Equal(t, "SIGUSR1", stop.Who)

	raise(t, syscall.SIGUSR1)

	out := lr.wait(t)
	require.ErrorIs(t, out.err, context.Canceled)
	assert.Equal(t, runbook.OutcomeCancelled, out.res.Outcome)
	assert.Equal(t, syscall.SIGUSR1, <-seen)

	state, err := runbook.Inspect(lr.path)
	require.NoError(t, err)
	assert.Equal(t, &runbook.StopRequest{
		Mode: runbook.StopCancel, By: "stopper", Who: "SIGUSR1",
	}, state.Attempts[0].Stop)
}

// A signal is acted on before OnSignal is told of it, so an OnSignal that
// blocks, such as on a write to a full pipe, can't keep a stop from
// happening.
func TestHandleSignals_StopsBeforeTellingOnSignal(t *testing.T) { //nolint:paralleltest // Signals are process-wide.
	stopper := runbook.NewStopper()
	unblock := make(chan struct{})

	release := runbook.HandleSignals(stopper, runbook.SignalOptions{
		Stop: []os.Signal{syscall.SIGWINCH}, Detach: []os.Signal{},
		OnSignal: func(os.Signal) { <-unblock },
	})
	defer release()
	defer close(unblock)

	raise(t, syscall.SIGWINCH)

	select {
	case <-stopper.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the stop waited for OnSignal")
	}
}

// A detach signal is reported, and stops nothing; once released, signals
// are no longer handled.
func TestHandleSignals_DetachAndRelease(t *testing.T) { //nolint:paralleltest // Signals are process-wide.
	stopper := runbook.NewStopper()
	seen := make(chan os.Signal, 2)

	release := runbook.HandleSignals(stopper, runbook.SignalOptions{
		Stop: []os.Signal{}, Detach: []os.Signal{syscall.SIGUSR2},
		OnSignal: func(sig os.Signal) { seen <- sig },
	})

	raise(t, syscall.SIGUSR2)
	assert.Equal(t, syscall.SIGUSR2, <-seen)

	select {
	case <-stopper.Done():
		t.Fatal("a detach signal stopped the run")
	default:
	}

	release()
	release() // Again does nothing.

	// Something else keeps the signal from ending the process.
	guard := make(chan os.Signal, 1)
	signal.Notify(guard, syscall.SIGUSR2)

	defer signal.Stop(guard)

	raise(t, syscall.SIGUSR2)
	<-guard

	select {
	case sig := <-seen:
		t.Fatalf("%v was handled after release", sig)
	case <-time.After(50 * time.Millisecond):
	}
}

// helper runs this test binary as a helper process doing mode.
func helper(t *testing.T, mode string) *exec.Cmd {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestHandleSignals_HelperProcess$") //nolint:gosec // This test's own binary.

	cmd.Env = append(os.Environ(), "RUNBOOK_SIGNALS_HELPER="+mode)

	return cmd
}

// After the cancel, the next stop signal has its default effect, which for
// SIGTERM ends the process. (Go ignores SIGUSR1 by default, so the helper
// process, alone, uses SIGTERM.)
//
// A stop signal also listed to detach is a stop signal only, so it too
// ends the process the third time.
func TestHandleSignals_AThirdSignalEndsTheProcess(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"third", "third-also-detach"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			var stderr bytes.Buffer

			cmd := helper(t, mode)
			cmd.Stderr = &stderr

			err := cmd.Run()

			var exit *exec.ExitError
			require.ErrorAs(t, err, &exit, stderr.String())

			status, ok := exit.Sys().(syscall.WaitStatus)
			require.True(t, ok)
			require.True(t, status.Signaled(), "exited %d: %s", status.ExitStatus(), stderr.String())
			assert.Equal(t, syscall.SIGTERM, status.Signal())
		})
	}
}

// A write to a standard output nobody reads fails, instead of ending the
// process.
func TestHandleSignals_AClosedPipeIsAnError(t *testing.T) {
	t.Parallel()

	var stderr bytes.Buffer

	cmd := helper(t, "pipe")
	cmd.Stderr = &stderr

	out, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	require.NoError(t, out.Close())

	require.NoError(t, cmd.Wait(), stderr.String())
}

// TestHandleSignals_HelperProcess is the helper process the tests above run.
func TestHandleSignals_HelperProcess(t *testing.T) {
	t.Parallel()

	mode := os.Getenv("RUNBOOK_SIGNALS_HELPER")

	switch mode {
	case "":
		t.Skip("run by other tests")
	case "third", "third-also-detach":
		stopper := runbook.NewStopper()
		seen := make(chan os.Signal, 3)

		opts := runbook.SignalOptions{
			Stop: []os.Signal{syscall.SIGTERM}, OnSignal: func(sig os.Signal) { seen <- sig },
		}
		if mode == "third-also-detach" {
			opts.Detach = []os.Signal{syscall.SIGTERM, syscall.SIGHUP}
		}

		runbook.HandleSignals(stopper, opts)

		raise(t, syscall.SIGTERM)
		<-seen
		raise(t, syscall.SIGTERM)
		<-seen

		// The handler lets go of the signal just after reporting it.
		for range 500 {
			raise(t, syscall.SIGTERM)
			time.Sleep(10 * time.Millisecond)
		}

		t.Fatal("the third signal didn't end the process")
	case "pipe":
		runbook.HandleSignals(runbook.NewStopper(), runbook.SignalOptions{})

		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stdout.Write([]byte("x\n")); err != nil {
				if !errors.Is(err, syscall.EPIPE) {
					t.Fatalf("write: %v", err)
				}

				os.Exit(0)
			}
		}

		t.Fatal("writes never failed")
	default:
		t.Fatalf("unknown mode %q", mode)
	}
}
