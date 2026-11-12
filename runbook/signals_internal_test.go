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
