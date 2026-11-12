package runbook

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

var (
	// ErrNotRunning is returned by [RequestStop] and [Approve] when no run is
	// in progress on the journal: none has started, the latest has finished,
	// or the process running it is gone.
	ErrNotRunning = errors.New("runbook: no run is in progress on the journal")

	// ErrGateNotWaiting is returned by [Approve] when the gate is not waiting
	// for approval, or stopped waiting without being approved.
	ErrGateNotWaiting = errors.New("runbook: the gate is not waiting for approval")
)

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
	// requestPoll is how often RequestStop and Approve read the journal for
	// their answer.
	requestPoll = 50 * time.Millisecond
)

// controlDir is where other processes leave requests for a run of the
// journal at path.
func controlDir(path string) string {
	return path + ".control"
}

// What a request asks.
const (
	requestStop    = "stop"
	requestCancel  = "cancel"
	requestApprove = "approve"
)

// controlFile names the kind of request for a stop mode.
func controlFile(mode StopMode) (string, error) {
	switch mode {
	case StopGraceful:
		return requestStop, nil
	case StopCancel:
		return requestCancel, nil
	default:
		return "", fmt.Errorf("runbook: unknown stop mode %q", mode)
	}
}

// A request file is named for what it asks and the run it asks it of, such
// as "stop.<run>", so a caller that read an earlier attempt can't replace a
// request made of the attempt now running. An approval's name adds a hash
// of the gate's path, as "approve.<run>.<hash>": a path can be longer than
// a file name may be. The file holds who asked, and the gate's path.
type controlRequest struct {
	Who  string `json:"who,omitempty"`
	Gate string `json:"gate,omitempty"`

	// From the file: its name, and the user that owned it.
	kind string
	run  string
	uid  *int
}

// requestName names the file for a request of run. key is a gate's hash,
// for an approval.
func requestName(kind, run, key string) (string, error) {
	if run == "" || strings.ContainsAny(run, "./\\\x00") {
		return "", fmt.Errorf("runbook: run %q can't name a request", run)
	}

	if key != "" {
		return kind + "." + run + "." + key, nil
	}

	return kind + "." + run, nil
}

// gateKey is the hash of a gate's path that names its approvals.
func gateKey(gate string) string {
	sum := sha256.Sum256([]byte(gate))

	return hex.EncodeToString(sum[:16])
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
// request left for an earlier run must not stop or approve anything in this
// one. A cancel is acted on first, and approvals last.
func (r *runner) readControl() {
	for _, req := range takeRequests(r.control, r.id) {
		switch req.kind {
		case requestCancel:
			r.cancelRequested(StopRequest{Mode: StopCancel, By: stopByControl, Who: req.Who, UID: req.uid})
		case requestStop:
			r.stop.fire(StopRequest{Mode: StopGraceful, By: stopByControl, Who: req.Who, UID: req.uid})
		case requestApprove:
			r.approveGate(req)
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
		return cmp.Compare(rank(a.kind), rank(b.kind))
	})

	return reqs
}

// rank orders requests: a cancel, then a graceful stop, then approvals.
func rank(kind string) int {
	switch kind {
	case requestCancel:
		return 0
	case requestStop:
		return 1
	default:
		return 2
	}
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

	var key string

	switch kind {
	case requestStop, requestCancel:
	case requestApprove:
		run, key, _ = strings.Cut(run, ".")
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

	// An approval names its gate twice; the two must agree.
	if kind == requestApprove && (req.Gate == "" || gateKey(req.Gate) != key) {
		return controlRequest{}, false
	}

	req.kind, req.run, req.uid = kind, run, fileUID(info)

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
	kind, err := controlFile(mode)
	if err != nil {
		return err
	}

	path, data, err := newRequest(path, controlRequest{Who: who})
	if err != nil {
		return fmt.Errorf("runbook: request stop: %w", err)
	}

	q := &stopRequester{
		reader: journalReader{path: path},
		dir:    controlDir(path),
		kind:   kind,
		mode:   mode,
		data:   data,
		locked: journalLocked,
	}

	return awaitAnswer(ctx, q.step)
}

// newRequest encodes a request, refusing one too big for the run to read,
// and returns the journal's path made absolute: OpenJournal fixes it the
// same way, and the control directory is named after it.
func newRequest(path string, req controlRequest) (string, []byte, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return "", nil, err
	}

	// The run would throw away a request too big to read, and never answer.
	if len(data) > maxControlRequest {
		return "", nil, fmt.Errorf("who is too long: the request is %d bytes, at most %d", len(data), maxControlRequest)
	}

	path, err = filepath.Abs(path)
	if err != nil {
		return "", nil, err
	}

	return path, data, nil
}

// awaitAnswer calls step until it reports the request settled, waiting
// between calls, or until ctx is done.
func awaitAnswer(ctx context.Context, step func() (bool, error)) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		done, err := step()
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
	kind   string
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

// write puts the request in place for the latest attempt.
func (q *stopRequester) write() error {
	name, err := requestName(q.kind, q.latest, "")
	if err == nil {
		err = writeRequest(q.dir, name, q.data)
	}

	if err != nil {
		return fmt.Errorf("runbook: request stop: %w", err)
	}

	return nil
}

// writeRequest puts a request in place: written under a temporary name and
// renamed, so the run never reads half of one.
func writeRequest(dir, name string, data []byte) error {
	f, err := os.CreateTemp(dir, "."+name+"-*")
	if err != nil {
		return err
	}

	tmp := f.Name()

	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}

	if err == nil {
		err = os.Rename(tmp, filepath.Join(dir, name))
	}

	if err != nil {
		_ = os.Remove(tmp)

		return err
	}

	return nil
}

// Approve approves the gate at the path gate (such as
// "upgrade/region1/continue-DC2") in the run in progress on the journal
// file at path, from this or any other process. who says who approved; it
// is recorded in the journal, with the user that wrote the approval.
//
// The approval goes through the run's control directory, as [RequestStop]
// does. Approve returns nil once the journal shows the gate released by an
// approval, whichever approver gave it. If the run finishes and another
// attempt starts first, the approval is made again once that attempt's gate
// is waiting.
//
// Approve returns [ErrGateNotWaiting] when the gate is not waiting, or stops
// waiting without an approval (its timeout released it, or a stop or cancel
// ended its wait), and [ErrNotRunning] when no run is in progress, or the
// run finished first. It returns the context's error when
// ctx is done first; the approval may still be acted on.
func Approve(ctx context.Context, path, gate, who string) error {
	path, data, err := newRequest(path, controlRequest{Who: who, Gate: gate})
	if err != nil {
		return fmt.Errorf("runbook: approve: %w", err)
	}

	q := &approveRequester{
		reader: journalReader{path: path},
		dir:    controlDir(path),
		gate:   gate,
		data:   data,
		locked: journalLocked,
	}

	return awaitAnswer(ctx, q.step)
}

// approveRequester is one Approve call's progress.
type approveRequester struct {
	reader journalReader
	dir    string
	gate   string
	data   []byte
	locked func(path string) bool

	// latest is the latest attempt's run, and open whether it has not
	// finished. In that attempt, the gate is waiting, or was released, and if
	// so whether by an approval, or finished.
	latest   string
	open     bool
	waiting  bool
	released bool
	approved bool
	finished bool
	// issued is the run the approval was last written for.
	issued string
}

// step reads what the journal has gained, and writes the approval for the
// latest attempt if its gate is waiting and it has not been written yet. It
// reports whether the approval is settled, and if so with what error.
func (q *approveRequester) step() (bool, error) {
	live := q.locked(q.reader.path)

	if err := q.read(); err != nil {
		return true, err
	}

	switch {
	case (q.released || q.finished) && q.issued == q.latest:
		// The gate the approval was issued to is through waiting. Unless
		// it was approved, by this call or another approver, it never will
		// be in this attempt.
		if q.approved {
			return true, nil
		}

		return true, ErrGateNotWaiting
	case !q.open:
		return true, ErrNotRunning
	case !live && !q.locked(q.reader.path):
		return true, ErrNotRunning
	case q.issued == "" && !q.waiting:
		return true, ErrGateNotWaiting
	case q.waiting && q.issued != q.latest:
		name, err := requestName(requestApprove, q.latest, gateKey(q.gate))
		if err == nil {
			err = writeRequest(q.dir, name, q.data)
		}

		if err != nil {
			return true, fmt.Errorf("runbook: approve: %w", err)
		}

		q.issued = q.latest
	}

	return false, nil
}

// read follows the journal's attempts and the gate. Everything else is
// passed over, chatter without being decoded.
func (q *approveRequester) read() error {
	err := q.reader.next(func(line []byte) error {
		if env, ok := peekEvent(line); ok && env.chatter {
			return nil
		}

		e, err := decodeEvent(line)
		if err != nil {
			return err
		}

		if e.Kind == EventRunStarted {
			q.latest, q.open, q.waiting, q.released, q.approved, q.finished = e.Run, true, false, false, false, false

			return nil
		}

		if e.Run != q.latest {
			return nil
		}

		switch e.Kind { //nolint:exhaustive // Only these say where the gate stands.
		case EventRunFinished:
			q.open = false
		case EventGateWaiting:
			if e.Path == q.gate {
				q.waiting, q.released, q.approved, q.finished = true, false, false, false
			}
		case EventGateReleased:
			if e.Path == q.gate {
				q.waiting, q.released, q.approved = false, true, e.Policy == ""
			}
		case EventStepFinished:
			if e.Path == q.gate {
				q.waiting, q.finished = false, true
			}
		}

		return nil
	})

	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	return err
}
