package runbook

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// ErrNotRunning is returned by [RequestStop] when no run is in progress on
// the journal: none has started, the latest has finished, or the process
// running it is gone.
var ErrNotRunning = errors.New("runbook: no run is in progress on the journal")

// DefaultControlPoll is how often a run looks for requests from other
// processes.
const DefaultControlPoll = 250 * time.Millisecond

// WithControlPoll sets how often the run looks for requests from other
// processes, such as [RequestStop]. Zero means [DefaultControlPoll].
func WithControlPoll(d time.Duration) Option {
	return func(c *config) {
		c.controlPoll = d
	}
}

const (
	// controlDirMode lets only the journal's owner make requests: whoever
	// can write to the directory can stop the run.
	controlDirMode = 0o700
	// maxControlRequest bounds what a run reads of a request.
	maxControlRequest = 4 << 10
	// requestPoll is how often RequestStop reads the journal for its answer.
	requestPoll = 50 * time.Millisecond
)

// controlDir is where other processes leave requests for a run of the
// journal at path.
func controlDir(path string) string {
	return path + ".control"
}

// controlFile names the request file for a stop mode.
func controlFile(mode StopMode) (string, error) {
	switch mode {
	case StopGraceful:
		return "stop", nil
	case StopCancel:
		return "cancel", nil
	default:
		return "", fmt.Errorf("runbook: unknown stop mode %q", mode)
	}
}

// A request file is named for what it asks and the run it asks it of, such
// as "stop.<run>", so a caller that read an earlier attempt can't replace a
// request made of the attempt now running. It holds who asked.
type controlRequest struct {
	Who string `json:"who,omitempty"`

	// From the file: its name, and the user that owned it.
	mode StopMode
	run  string
	uid  *int
}

// requestName names the file for a request of run.
func requestName(mode StopMode, run string) (string, error) {
	name, err := controlFile(mode)
	if err != nil {
		return "", err
	}

	if run == "" || strings.ContainsAny(run, "/\\\x00") {
		return "", fmt.Errorf("runbook: run %q can't name a request", run)
	}

	return name + "." + run, nil
}

// prepareControl makes the control directory, private to its owner. A
// directory left by an earlier run is made private again.
func prepareControl(dir string) error {
	if err := os.Mkdir(dir, controlDirMode); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("runbook: control directory: %w", err)
	}

	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("runbook: control directory: %w", err)
	}

	if err := checkControlDir(dir, info, os.Geteuid()); err != nil {
		return err
	}

	if info.Mode().Perm() != controlDirMode {
		if err := os.Chmod(dir, controlDirMode); err != nil {
			return fmt.Errorf("runbook: control directory: %w", err)
		}
	}

	return nil
}

// checkControlDir refuses a control directory that isn't one, or that
// belongs to a user other than euid: making it private would not take it
// from them, and they could read or replace requests.
func checkControlDir(dir string, info os.FileInfo, euid int) error {
	if !info.IsDir() {
		return fmt.Errorf("runbook: control directory: %s is not a directory", dir)
	}

	if uid := fileUID(info); uid != nil && *uid != euid {
		return fmt.Errorf("runbook: control directory: %s belongs to user %d, not %d", dir, *uid, euid)
	}

	return nil
}

// watchControl reads the control directory until done is closed.
func (r *runner) watchControl(done <-chan struct{}) {
	poll := time.NewTicker(r.controlPoll)
	defer poll.Stop()

	for {
		r.readControl()

		select {
		case <-done:
			return
		case <-poll.C:
		}
	}
}

// readControl acts on the requests for this run and deletes the rest: a
// request left for an earlier run must not stop this one. A cancel is acted
// on first.
func (r *runner) readControl() {
	for _, req := range takeRequests(r.control, r.id) {
		stop := StopRequest{Mode: req.mode, By: stopByControl, Who: req.Who, UID: req.uid}

		if req.mode == StopCancel {
			r.cancelRequested(stop)
		} else {
			r.stop.fire(stop)
		}
	}
}

// takeRequests takes every request from dir and returns those for run,
// cancels first. Everything else there is deleted, except files whose names
// start with a dot: requests still being written.
func takeRequests(dir, run string) []controlRequest {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var reqs []controlRequest

	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}

		req, ok := takeRequest(dir, e.Name())
		if ok && req.run == run {
			reqs = append(reqs, req)
		}
	}

	slices.SortStableFunc(reqs, func(a, b controlRequest) int {
		return cmp.Compare(rank(a.mode), rank(b.mode))
	})

	return reqs
}

// rank orders requests: a cancel before a graceful stop.
func rank(mode StopMode) int {
	if mode == StopCancel {
		return 0
	}

	return 1
}

// takeRequest takes the file named name from dir, deletes it, and returns
// the request it holds, if it is one.
func takeRequest(dir, name string) (controlRequest, bool) {
	// Moved aside before it is read, so a request written meanwhile is left
	// for the next look rather than deleted unread.
	taken := filepath.Join(dir, ".taken-"+name)
	if err := os.Rename(filepath.Join(dir, name), taken); err != nil {
		return controlRequest{}, false
	}

	defer func() { _ = os.Remove(taken) }()

	kind, run, _ := strings.Cut(name, ".")

	var mode StopMode

	switch kind {
	case "stop":
		mode = StopGraceful
	case "cancel":
		mode = StopCancel
	default:
		return controlRequest{}, false
	}

	info, err := os.Lstat(taken)
	if err != nil || !info.Mode().IsRegular() {
		return controlRequest{}, false
	}

	f, err := os.Open(taken) //nolint:gosec // In the run's own control directory.
	if err != nil {
		return controlRequest{}, false
	}

	defer func() { _ = f.Close() }()

	info, err = f.Stat()
	if err != nil {
		return controlRequest{}, false
	}

	data, err := io.ReadAll(io.LimitReader(f, maxControlRequest+1))
	if err != nil || len(data) > maxControlRequest {
		return controlRequest{}, false
	}

	var req controlRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return controlRequest{}, false
	}

	req.mode, req.run, req.uid = mode, run, fileUID(info)

	return req, true
}

// RequestStop asks the run in progress on the journal file at path to stop,
// from this or any other process. who says who is asking, such as an
// operator's name; it is recorded in the journal, with the user that wrote
// the request. mode is [StopGraceful] or [StopCancel].
//
// The request goes through a file in the directory beside the journal that
// the run made, and that only the run's user can write to. RequestStop
// returns nil once the journal shows the run recorded it, or that the run
// was already asked to stop the same way (a cancel answers a graceful stop
// too). If the run finishes and another attempt starts first, the request
// is made again of the new attempt.
//
// RequestStop returns [ErrNotRunning] when no run is in progress, or when
// the run finished without seeing the request. It returns the context's
// error when ctx is done first; the request may still be acted on.
func RequestStop(ctx context.Context, path string, mode StopMode, who string) error {
	if _, err := controlFile(mode); err != nil {
		return err
	}

	data, err := json.Marshal(controlRequest{Who: who})
	if err != nil {
		return fmt.Errorf("runbook: request stop: %w", err)
	}

	// The run would throw away a request too big to read, and never answer.
	if len(data) > maxControlRequest {
		return fmt.Errorf("runbook: request stop: who is too long: the request is %d bytes, at most %d", len(data), maxControlRequest)
	}

	// OpenJournal fixes the journal's path the same way, and the control
	// directory is named after it.
	path, err = filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("runbook: request stop: %w", err)
	}

	q := &stopRequester{
		reader: journalReader{path: path},
		dir:    controlDir(path),
		mode:   mode,
		data:   data,
		locked: journalLocked,
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		done, err := q.step()
		if done {
			return err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(requestPoll):
		}
	}
}

// stopRequester is one RequestStop call's progress.
type stopRequester struct {
	reader journalReader
	dir    string
	mode   StopMode
	// data is the request, written for each attempt it is made of.
	data []byte
	// locked reports whether a process holds the journal at a path.
	locked func(path string) bool

	// latest is the latest attempt's run; open is whether it has not
	// finished, and answered whether it recorded a stop that answers this
	// request.
	latest   string
	open     bool
	answered bool
	// issued is the run the request was last written for.
	issued string
}

// step reads what the journal has gained, and writes the request for the
// latest attempt if it has not been written yet. It reports whether the
// request is settled, and if so with what error.
func (q *stopRequester) step() (bool, error) {
	// Checked before the read: a runner that has let go of the journal by
	// now has written everything it will, so the read below sees it all.
	live := q.locked(q.reader.path)

	if err := q.read(); err != nil {
		return true, err
	}

	switch {
	case q.answered && q.issued == q.latest:
		// The attempt this call asked recorded it, even if it has finished
		// since.
		return true, nil
	case !q.open:
		return true, ErrNotRunning
	case !live && !q.locked(q.reader.path):
		// The attempt never finished, and nothing holds the journal: its
		// runner is gone. (One that took the journal since is read next.)
		return true, ErrNotRunning
	case q.answered:
		// The attempt in progress was already asked to stop this way.
		return true, nil
	case q.issued != q.latest:
		if err := q.write(); err != nil {
			return true, err
		}

		q.issued = q.latest
	}

	return false, nil
}

// read follows the journal's attempts and stop requests. Everything else is
// passed over, chatter without being decoded.
func (q *stopRequester) read() error {
	err := q.reader.next(func(line []byte) error {
		if env, ok := peekEvent(line); ok && env.chatter {
			return nil
		}

		e, err := decodeEvent(line)
		if err != nil {
			return err
		}

		switch e.Kind { //nolint:exhaustive // Only these say where the run stands.
		case EventRunStarted:
			q.latest, q.open, q.answered = e.Run, true, false
		case EventRunFinished:
			if e.Run == q.latest {
				q.open = false
			}
		case EventStopRequested:
			if e.Run == q.latest && (e.Mode == q.mode || e.Mode == StopCancel) {
				q.answered = true
			}
		}

		return nil
	})

	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	return err
}

// write puts the request in place for the latest attempt: written under a
// temporary name and renamed, so the run never reads half of one.
func (q *stopRequester) write() error {
	name, err := requestName(q.mode, q.latest)
	if err != nil {
		return fmt.Errorf("runbook: request stop: %w", err)
	}

	f, err := os.CreateTemp(q.dir, "."+name+"-*")
	if err != nil {
		return fmt.Errorf("runbook: request stop: %w", err)
	}

	tmp := f.Name()

	_, err = f.Write(q.data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}

	if err == nil {
		err = os.Rename(tmp, filepath.Join(q.dir, name))
	}

	if err != nil {
		_ = os.Remove(tmp)

		return fmt.Errorf("runbook: request stop: %w", err)
	}

	return nil
}
