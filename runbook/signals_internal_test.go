package runbook

import (
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSignalName(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "SIGINT", signalName(os.Interrupt))
	assert.Equal(t, "SIGTERM", signalName(syscall.SIGTERM))
	assert.Equal(t, "SIGHUP", signalName(syscall.SIGHUP))
	assert.Equal(t, "SIGQUIT", signalName(syscall.SIGQUIT))
	assert.Equal(t, syscall.SIGKILL.String(), signalName(syscall.SIGKILL), "others are named as Go names them")
}

// Each signal is handled one way: stop outranks detach, and SIGPIPE is only
// ignored if it isn't listed already.
func TestSignalSets(t *testing.T) {
	t.Parallel()

	stop, detach, pipe := signalSets(SignalOptions{})
	assert.Equal(t, []os.Signal{os.Interrupt, syscall.SIGTERM}, stop)
	assert.Equal(t, []os.Signal{syscall.SIGHUP}, detach)
	assert.Equal(t, []os.Signal{syscall.SIGPIPE}, pipe)

	opts := SignalOptions{
		Stop:   []os.Signal{syscall.SIGHUP, syscall.SIGPIPE},
		Detach: []os.Signal{syscall.SIGHUP, syscall.SIGTERM},
	}
	stop, detach, pipe = signalSets(opts)
	assert.Equal(t, []os.Signal{syscall.SIGHUP, syscall.SIGPIPE}, stop)
	assert.Equal(t, []os.Signal{syscall.SIGTERM}, detach)
	assert.Empty(t, pipe)
	assert.Equal(t, []os.Signal{syscall.SIGHUP, syscall.SIGTERM}, opts.Detach, "the caller's slice is left alone")

	stop, detach, pipe = signalSets(SignalOptions{Stop: []os.Signal{}, Detach: []os.Signal{syscall.SIGPIPE}})
	assert.Empty(t, stop)
	assert.Equal(t, []os.Signal{syscall.SIGPIPE}, detach)
	assert.Empty(t, pipe)
}
