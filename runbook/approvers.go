package runbook

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// defaultFilePoll is how often a [FileApprover] looks for its file when it
// is given no interval.
const defaultFilePoll = time.Second

// FileApprover returns an [Approver] that approves when a file exists at
// path, looking for it every poll (every second if poll is not positive).
// It is for tools that already approve by creating a flag file.
//
// The file approves whenever it is there, including if it was created
// before the gate started waiting. The approver removes the file when it
// approves, so one file approves one gate. The file's first line, if any,
// is recorded as who approved.
//
// If the file can't be read or removed, the approver withdraws, which the
// run logs as a warning; the gate goes on waiting for its other approvers.
func FileApprover(path string, poll time.Duration) Approver {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}

	if poll <= 0 {
		poll = defaultFilePoll
	}

	return &fileApprover{path: path, poll: poll}
}

type fileApprover struct {
	path string
	poll time.Duration
}

func (a *fileApprover) Name() string { return "file" }

func (a *fileApprover) Wait(ctx context.Context, _ GateInfo) (string, error) {
	tick := time.NewTicker(a.poll)
	defer tick.Stop()

	for {
		// A gate released some other way leaves the file for the next one.
		if err := ctx.Err(); err != nil {
			return "", err
		}

		who, ok, err := a.take()
		if err != nil || ok {
			return who, err
		}

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-tick.C:
		}
	}
}

// take reads and removes the file, if it is there, and returns its first
// line.
func (a *fileApprover) take() (string, bool, error) {
	f, err := os.Open(a.path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}

	if err != nil {
		return "", false, fmt.Errorf("runbook: file approver: %w", err)
	}

	data, err := io.ReadAll(io.LimitReader(f, maxControlRequest))
	_ = f.Close()

	if err != nil {
		return "", false, fmt.Errorf("runbook: file approver: read %s: %w", a.path, err)
	}

	if err := os.Remove(a.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", false, fmt.Errorf("runbook: file approver: %w", err)
	}

	first, _, _ := strings.Cut(string(data), "\n")

	return strings.TrimSpace(first), true, nil
}

// lineBacklog is how many lines a waiting [StdinApprover] holds before it
// has taken them.
const lineBacklog = 16

// stdin is the reader every [StdinApprover] shares: only one reader can own
// standard input, as a read of it can't be cancelled.
var stdin = newLineReader(os.Stdin) //nolint:gochecknoglobals // There is one standard input per process.

// StdinApprover returns an [Approver] that asks on standard error, and
// approves when a line read from standard input is "yes" or "y", in any
// case. Who approved is recorded as the user the process runs as. Any other
// line asks again: to say no, stop the run.
//
// It withdraws when standard input ends or fails to read, which is what a
// lost terminal produces, and when it can't write its question. Run
// detached, with no terminal, use [Approve] instead.
//
// Once a gate has asked, standard input is read for the rest of the
// process. A line read while no gate is asking is ignored. If several gates
// are asking at once, each line goes to the one that has waited longest.
func StdinApprover() Approver {
	return &lineApprover{name: "stdin", in: stdin, out: os.Stderr, who: currentUser}
}

// lineApprover approves when a line read from in says yes.
type lineApprover struct {
	name string
	in   *lineReader
	out  io.Writer
	who  func() string
}

func (a *lineApprover) Name() string { return a.name }

func (a *lineApprover) Wait(ctx context.Context, gate GateInfo) (string, error) {
	lines, leave := a.in.join()
	defer leave()

	question := "Type yes to approve " + gate.Path + ": "
	if gate.Prompt != "" {
		question = gate.Prompt + "\n" + question
	}

	for {
		if _, err := io.WriteString(a.out, question); err != nil {
			return "", fmt.Errorf("runbook: %s: %w", a.name, err)
		}

		select {
		case line := <-lines:
			if yes(line) {
				return a.who(), nil
			}
		case <-a.in.done:
			return "", fmt.Errorf("runbook: %s: %w", a.name, a.in.err)
		case <-ctx.Done():
			// End the unanswered question's line.
			_, _ = io.WriteString(a.out, "\n")

			return "", ctx.Err()
		}

		question = "Type yes to approve " + gate.Path + ": "
	}
}

func yes(line string) bool {
	line = strings.TrimSpace(line)

	return strings.EqualFold(line, "yes") || strings.EqualFold(line, "y")
}

func currentUser() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}

	return u.Username
}

// lineReader reads lines from r and hands each to the longest waiting of
// those that joined, once the first of them joins. A line read while none
// are waiting is dropped.
type lineReader struct {
	r    io.Reader
	once sync.Once

	mu      sync.Mutex
	waiters []chan string

	// done is closed when r is through, with err saying why.
	done chan struct{}
	err  error
}

func newLineReader(r io.Reader) *lineReader {
	return &lineReader{r: r, done: make(chan struct{})}
}

// join waits for lines, and returns where they arrive and a function to
// stop waiting.
func (l *lineReader) join() (<-chan string, func()) {
	l.once.Do(func() { go l.read() })

	// A few lines can wait to be taken, so none typed ahead is lost while
	// the waiter asks again.
	ch := make(chan string, lineBacklog)

	l.mu.Lock()
	l.waiters = append(l.waiters, ch)
	l.mu.Unlock()

	return ch, func() {
		l.mu.Lock()
		defer l.mu.Unlock()

		l.waiters = slices.DeleteFunc(l.waiters, func(w chan string) bool { return w == ch })
	}
}

func (l *lineReader) read() {
	sc := bufio.NewScanner(l.r)
	for sc.Scan() {
		l.deliver(sc.Text())
	}

	l.err = sc.Err()
	if l.err == nil {
		l.err = io.EOF
	}

	close(l.done)
}

// deliver hands line to the longest waiting waiter, or drops it.
func (l *lineReader) deliver(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if len(l.waiters) == 0 {
		return
	}

	select {
	case l.waiters[0] <- line:
	default:
		// The waiter is that many lines behind; this one is dropped.
	}
}
