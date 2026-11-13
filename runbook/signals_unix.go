//go:build unix

package runbook

import (
	"os"
	"syscall"
)

// osSignalName names the signals only some systems have.
func osSignalName(sig os.Signal) string {
	switch sig {
	case syscall.SIGUSR1:
		return "SIGUSR1"
	case syscall.SIGUSR2:
		return "SIGUSR2"
	default:
		return ""
	}
}
