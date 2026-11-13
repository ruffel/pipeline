//go:build !unix

package runbook

import "os"

// osSignalName names the signals only some systems have.
func osSignalName(os.Signal) string { return "" }
