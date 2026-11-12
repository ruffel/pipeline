package runbook

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFileApprover_WithdrawsIfItCannotTakeTheFile(t *testing.T) {
	t.Parallel()

	t.Run("unreadable", func(t *testing.T) {
		t.Parallel()

		// A directory opens, on some systems, but doesn't read.
		_, err := FileApprover(t.TempDir(), time.Millisecond).Wait(t.Context(), GateInfo{})
		require.Error(t, err)
	})

	t.Run("not openable", func(t *testing.T) {
		t.Parallel()

		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("needs a file this user can't open")
		}

		dir := t.TempDir()
		flag := filepath.Join(dir, "continue")
		require.NoError(t, os.WriteFile(flag, []byte("alice"), 0o000))

		_, err := FileApprover(flag, time.Millisecond).Wait(t.Context(), GateInfo{})
		require.ErrorContains(t, err, "permission denied")
		assert.FileExists(t, flag, "a file it can't read is put back")

		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		assert.Len(t, entries, 1, "nothing else is left behind")
	})

	t.Run("not removable", func(t *testing.T) {
		t.Parallel()

		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("needs a directory this user can't remove files from")
		}

		dir := t.TempDir()
		flag := filepath.Join(dir, "continue")
		require.NoError(t, os.WriteFile(flag, []byte("alice"), 0o600))
		require.NoError(t, os.Chmod(dir, 0o500))       //nolint:gosec // Read-only on purpose.
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // So the test can clean up.

		_, err := FileApprover(flag, time.Millisecond).Wait(t.Context(), GateInfo{})
		require.ErrorContains(t, err, "permission denied")
		assert.FileExists(t, flag, "a file it can't remove doesn't approve")
	})
}

// A gate released some other way leaves the file for the next gate.
func TestFileApprover_LeavesTheFileOnceTheGateIsReleased(t *testing.T) {
	t.Parallel()

	flag := filepath.Join(t.TempDir(), "continue")
	require.NoError(t, os.WriteFile(flag, nil, 0o600))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := FileApprover(flag, time.Millisecond).Wait(ctx, GateInfo{})
	require.ErrorIs(t, err, context.Canceled)
	assert.FileExists(t, flag)
}

// writes is a Writer that hands each write to the test.
type writes chan string

func (w writes) Write(p []byte) (int, error) {
	w <- string(p)

	return len(p), nil
}

// newLineTest returns a lineApprover reading what the test writes, and
// where it writes its questions.
func newLineTest(t *testing.T) (*lineApprover, *io.PipeWriter, writes) {
	t.Helper()

	r, w := io.Pipe()

	t.Cleanup(func() { _ = w.Close() })

	out := make(writes, 16)

	return &lineApprover{name: "stdin", in: newLineReader(r), out: out, who: func() string { return "alice" }}, w, out
}

type waited struct {
	who string
	err error
}

func waitIn(ctx context.Context, a *lineApprover, gate GateInfo) <-chan waited {
	ch := make(chan waited, 1)

	go func() {
		who, err := a.Wait(ctx, gate)
		ch <- waited{who, err}
	}()

	return ch
}

func writeLine(t *testing.T, w io.Writer, line string) {
	t.Helper()

	_, err := io.WriteString(w, line+"\n")
	require.NoError(t, err)
}

// Only yes approves; anything else asks again, without the prompt.
func TestStdinApprover_AsksUntilYes(t *testing.T) {
	t.Parallel()

	a, in, out := newLineTest(t)
	done := waitIn(t.Context(), a, GateInfo{Path: "p/go", Prompt: "Upgrade DC2?"})

	assert.Equal(t, "Upgrade DC2?\nType yes to approve p/go: ", <-out)

	for _, line := range []string{"no", "", "yess"} {
		writeLine(t, in, line)
		assert.Equal(t, "Type yes to approve p/go: ", <-out)
	}

	writeLine(t, in, "  YES ")

	got := <-done
	require.NoError(t, got.err)
	assert.Equal(t, "alice", got.who)
}

// The end of input withdraws every approver reading it, now and later.
func TestStdinApprover_WithdrawsAtTheEndOfInput(t *testing.T) {
	t.Parallel()

	a, in, out := newLineTest(t)
	done := waitIn(t.Context(), a, GateInfo{Path: "p/go"})

	<-out
	require.NoError(t, in.Close())

	got := <-done
	require.ErrorIs(t, got.err, io.EOF)

	_, err := a.Wait(t.Context(), GateInfo{Path: "p/later"})
	require.ErrorIs(t, err, io.EOF)
}

// A gate released some other way ends the question's line.
func TestStdinApprover_GivesUpWhenTheGateIsReleased(t *testing.T) {
	t.Parallel()

	a, _, out := newLineTest(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := waitIn(ctx, a, GateInfo{Path: "p/go"})

	<-out
	cancel()

	require.ErrorIs(t, (<-done).err, context.Canceled)
	assert.Equal(t, "\n", <-out)
}

// An approver that can't ask withdraws.
func TestStdinApprover_WithdrawsIfItCannotAsk(t *testing.T) {
	t.Parallel()

	r, w := io.Pipe()

	t.Cleanup(func() { _ = w.Close() })

	a := &lineApprover{name: "stdin", in: newLineReader(r), out: failingWriter{}, who: func() string { return "" }}

	_, err := a.Wait(t.Context(), GateInfo{Path: "p/go"})
	require.ErrorIs(t, err, io.ErrClosedPipe)
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

// A line read while nobody waits is dropped; one read while several wait
// goes to the one that has waited longest.
func TestLineReader_Deliver(t *testing.T) {
	t.Parallel()

	r, w := io.Pipe()

	t.Cleanup(func() { _ = w.Close() })

	l := newLineReader(r)
	l.deliver("yes")

	first, leaveFirst := l.join()
	second, leaveSecond := l.join()

	defer leaveSecond()

	assert.Empty(t, first, "a line from before anyone waited is dropped")

	l.deliver("one")
	assert.Equal(t, "one", <-first)
	assert.Empty(t, second)

	leaveFirst()
	l.deliver("two")
	assert.Equal(t, "two", <-second)
}

func TestYes(t *testing.T) {
	t.Parallel()

	for _, line := range []string{"yes", "y", "Y", "YES", " yes\r"} {
		assert.True(t, yes(line), line)
	}

	for _, line := range []string{"", "no", "n", "yess", "ye s", "ok"} {
		assert.False(t, yes(line), strings.TrimSpace(line))
	}
}

// Approvers watching the same file race for it, and only the one that takes
// it approves.
func TestFileApprover_OneFileApprovesOnce(t *testing.T) {
	t.Parallel()

	flag := filepath.Join(t.TempDir(), "continue")

	for range 200 {
		require.NoError(t, os.WriteFile(flag, []byte("alice"), 0o600))

		var (
			wg       sync.WaitGroup
			approved atomic.Int32
		)

		errs := make(chan error, 4)

		for range 4 {
			wg.Go(func() {
				_, ok, err := (&fileApprover{path: flag}).take()
				if ok {
					approved.Add(1)
				}

				errs <- err
			})
		}

		wg.Wait()
		close(errs)

		for err := range errs {
			require.NoError(t, err)
		}

		require.Equal(t, int32(1), approved.Load())
		require.NoFileExists(t, flag)
	}

	entries, err := os.ReadDir(filepath.Dir(flag))
	require.NoError(t, err)
	assert.Empty(t, entries, "nothing taken is left behind")
}

// An answer read just before the input ends still approves.
func TestStdinApprover_AnAnswerBeforeTheEndApproves(t *testing.T) {
	t.Parallel()

	for range 50 {
		in := newLineReader(nil)
		in.once.Do(func() {}) // The test reads for it.

		out := pausedWriter{asking: make(chan struct{}), resume: make(chan struct{})}
		a := &lineApprover{name: "stdin", in: in, out: out, who: func() string { return "alice" }}
		done := waitIn(t.Context(), a, GateInfo{Path: "p/go"})

		// While the approver asks, the answer arrives and the input ends.
		<-out.asking
		in.deliver("yes")
		in.err = io.EOF
		close(in.done)

		out.resume <- struct{}{}

		got := <-done
		require.NoError(t, got.err)
		require.Equal(t, "alice", got.who)
	}
}

// pausedWriter holds each write until the test lets it go on.
type pausedWriter struct {
	asking chan struct{}
	resume chan struct{}
}

func (w pausedWriter) Write(p []byte) (int, error) {
	w.asking <- struct{}{}

	<-w.resume

	return len(p), nil
}

// The first gate to ask can't lose an answer already waiting to be read.
func TestStdinApprover_TheFirstGateGetsInputAlreadyThere(t *testing.T) {
	t.Parallel()

	for range 2000 {
		out := make(writes, 1)
		a := &lineApprover{
			name: "stdin", in: newLineReader(strings.NewReader("yes\n")), out: out,
			who: func() string { return "alice" },
		}

		who, err := a.Wait(t.Context(), GateInfo{Path: "p/go"})
		require.NoError(t, err)
		require.Equal(t, "alice", who)
	}
}
