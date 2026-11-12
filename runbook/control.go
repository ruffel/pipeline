package runbook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

// controlRequest is what a request file holds: the run it is for, and who
// asked. uid is the user that owned the file.
type controlRequest struct {
	Run string `json:"run"`
	Who string `json:"who,omitempty"`

	uid *int
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

	if !info.IsDir() {
		return fmt.Errorf("runbook: control directory: %s is not a directory", dir)
	}

	if info.Mode().Perm() != controlDirMode {
		if err := os.Chmod(dir, controlDirMode); err != nil {
			return fmt.Errorf("runbook: control directory: %w", err)
		}
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
// request left for an earlier run must not stop this one.
func (r *runner) readControl() {
	for _, mode := range []StopMode{StopCancel, StopGraceful} {
		name, _ := controlFile(mode)

		req, ok := takeRequest(r.control, name)
		if !ok || req.Run != r.id {
			continue
		}

		stop := StopRequest{Mode: mode, By: stopByControl, Who: req.Who, UID: req.uid}

		if mode == StopCancel {
			r.cancelRequested(stop)
		} else {
			r.stop.fire(stop)
		}
	}
}

// takeRequest takes the request named name from dir, if there is one, and
// deletes it. Anything there that isn't a request is deleted too.
func takeRequest(dir, name string) (controlRequest, bool) {
	// Moved aside before it is read, so a request written meanwhile is left
	// for the next look rather than deleted unread.
	taken := filepath.Join(dir, ".taken-"+name)
	if err := os.Rename(filepath.Join(dir, name), taken); err != nil {
		return controlRequest{}, false
	}

	defer func() { _ = os.Remove(taken) }()

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

	req.uid = fileUID(info)

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
	name, err := controlFile(mode)
	if err != nil {
		return err
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
		name:   name,
		mode:   mode,
		who:    who,
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
	name   string
	mode   StopMode
	who    string
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
	case q.answered:
		return true, nil
	case !q.open:
		return true, ErrNotRunning
	case !live && !q.locked(q.reader.path):
		// The attempt never finished, and nothing holds the journal: its
		// runner is gone. (One that took the journal since is read next.)
		return true, ErrNotRunning
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
	data, err := json.Marshal(controlRequest{Run: q.latest, Who: q.who})
	if err != nil {
		return fmt.Errorf("runbook: request stop: %w", err)
	}

	f, err := os.CreateTemp(q.dir, "."+q.name+"-*")
	if err != nil {
		return fmt.Errorf("runbook: request stop: %w", err)
	}

	tmp := f.Name()

	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}

	if err == nil {
		err = os.Rename(tmp, filepath.Join(q.dir, q.name))
	}

	if err != nil {
		_ = os.Remove(tmp)

		return fmt.Errorf("runbook: request stop: %w", err)
	}

	return nil
}
