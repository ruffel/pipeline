//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package runbook

import (
	"errors"
	"fmt"
)

// OpenJournal is not supported on this platform: the file journal needs
// flock. Use [MemoryJournal].
func OpenJournal(_ string, _ ...JournalOption) (*Journal, error) {
	return nil, fmt.Errorf("runbook: file journals: %w", errors.ErrUnsupported)
}

// journalLocked always reports false: nothing can hold a file journal here.
func journalLocked(_ string) bool {
	return false
}
