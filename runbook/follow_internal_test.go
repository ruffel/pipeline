//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package runbook

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func appendLine(t *testing.T, path, line string) {
	t.Helper()

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, journalFileMode) //nolint:gosec // A file in the test's own temp dir.
	require.NoError(t, err)

	_, err = f.WriteString(line + "\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

// A new runner can take the journal just after Follow sees it unlocked, and
// write run.started before Follow's last read. That runner is alive: Follow
// must look again, not report it gone.
func TestFollow_ARunnerThatStartsDuringTheLastCheckIsNotReportedGone(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "test.journal")
	appendLine(t, path, `{"v":1,"seq":1,"kind":"run.started","run":"r1"}`)
	appendLine(t, path, `{"v":1,"seq":2,"kind":"run.finished","run":"r1","outcome":"failed"}`)

	var (
		mu     sync.Mutex
		seen   []Event
		checks int
		live   atomic.Bool
	)

	f := &follower{
		reader: journalReader{path: path},
		opts:   FollowOptions{Poll: time.Millisecond},
		observers: []Observer{ObserverFunc(func(_ context.Context, e Event) {
			mu.Lock()
			defer mu.Unlock()

			seen = append(seen, e)
		})},
		locked: func(string) bool {
			checks++
			if checks == 1 {
				// The journal is free at this instant; the new runner takes
				// it and starts its attempt right afterwards.
				appendLine(t, path, `{"v":1,"seq":3,"kind":"run.started","run":"r2"}`)
				live.Store(true)

				return false
			}

			return live.Load()
		},
	}

	done := make(chan error, 1)

	go func() { done <- f.run(t.Context()) }()

	count := func() int {
		mu.Lock()
		defer mu.Unlock()

		return len(seen)
	}

	require.Eventually(t, func() bool { return count() == 3 }, 10*time.Second, time.Millisecond)

	select {
	case err := <-done:
		t.Fatalf("Follow returned %v while the new runner holds the journal", err)
	case <-time.After(20 * time.Millisecond):
	}

	// The new runner finishes and lets go.
	appendLine(t, path, `{"v":1,"seq":4,"kind":"run.finished","run":"r2","outcome":"passed"}`)
	live.Store(false)

	require.NoError(t, <-done)
	assert.Equal(t, 4, count())
}
