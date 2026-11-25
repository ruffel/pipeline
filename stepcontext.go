package pipeline

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/ruffel/pipeline/internal/linesplit"
)

// Size limits on what a step writes, so the journal stays quick to replay.
const (
	// MaxRecordBytes caps the encoded values one step records in one attempt.
	// It is the step's allowance, not each key's: every Record counts, on
	// every try of the step, and a key recorded again is counted again.
	MaxRecordBytes = 64 << 10
	// MaxCustomBytes caps the encoded data of one custom event.
	MaxCustomBytes = 64 << 10
	// MaxItemDataBytes caps the encoded data of one item.
	MaxItemDataBytes = 4 << 10
	// MaxWarningBytes caps the warnings one step records in one attempt
	// with RecordWarning: the lengths of their messages, added up.
	MaxWarningBytes = 64 << 10
	// MaxOutputLine is the longest output line recorded as one event. Longer
	// lines are recorded in pieces, all but the first marked Continued.
	MaxOutputLine = 16 << 10
	// MaxRedactLine is the longest output line a redactor is shown whole.
	// With [WithRedactor] set, a line this long or longer is replaced by a
	// notice: split into pieces, a secret could straddle two of them and
	// slip past the redactor.
	MaxRedactLine = 1 << 20
	// MaxStepOutput caps the output one step records in one attempt. Each
	// line or piece counts its length plus one for its line break, so blank
	// lines count too. Lines past the cap are dropped and counted.
	MaxStepOutput = 16 << 20
)

// StepContext gives a running step access to the run: messages, progress,
// output, per-target results, recorded values and custom events. A run makes
// one for each step, shared by its Check and Run and all their tries.
//
// A StepContext stops working when its step returns. Later calls record
// nothing, and those that return an error return [ErrStepReturned]. A zero
// StepContext behaves as if its step had returned.
type StepContext struct {
	r *runner
	n *node

	// mu serialises the step's writes, and makes close wait for any in flight.
	mu       sync.Mutex
	closed   bool
	recorded int
	warned   int
	// tasks is when the step first reported progress on each task, by key.
	tasks map[string]time.Time

	// Output writers, created on first use, and the step's output so far.
	stdout, stderr *linesplit.Writer
	outBytes       int
	dropped        int
	// cut holds the streams whose current line lost a piece to the budget:
	// the rest of that line is dropped too, so no continuation is recorded
	// without the piece before it.
	cut map[string]bool

	// closing stops new writes once the step has returned; writes counts
	// those already admitted, which close waits for before flushing.
	closing bool
	writes  sync.WaitGroup
}

// Path returns the step's path, such as "upgrade/node7/packages".
func (s *StepContext) Path() string {
	if s.n == nil {
		return ""
	}

	return s.n.path
}

// Labels returns the step's labels, including those it inherits.
func (s *StepContext) Labels() L {
	if s.n == nil {
		return nil
	}

	return maps.Clone(s.n.labels)
}

// Attempt returns the attempt number, starting at 1.
func (s *StepContext) Attempt() int {
	if s.r == nil {
		return 0
	}

	return s.r.attempt
}

// Info records an informational message. It is best-effort.
func (s *StepContext) Info(msg string) {
	_ = s.emit(Event{Kind: EventMessage, Level: LevelInfo, Message: msg})
}

// Warn records a warning. It is best-effort: a message for whoever is
// watching, which a crash can lose and the state does not keep. For a
// warning that anything is to act on, use RecordWarning.
func (s *StepContext) Warn(msg string) {
	_ = s.emit(Event{Kind: EventMessage, Level: LevelWarn, Message: msg})
}

// RecordWarning records that something is wrong which does not fail the
// step, such as a task after the main work that could not be done. It is
// durable, where Warn is best-effort: it is stored before RecordWarning
// returns, and [State.Warnings] lists it, so a tool can tell a run that
// passed from one that passed with warnings.
//
// It is a record, and the run takes no notice of it: the step passes or
// fails by what its Run returns, and the run's outcome is the same either
// way. A step's warnings stand until a later attempt calls its Run again,
// which then says afresh what is wrong, and are forgotten with its other
// records: when a rollback undoes the step, a recovery step resets its
// group, or a forced attempt starts over. A warning about one target of
// many is an item with status [ItemWarned].
//
// It returns an error, and records nothing, if the message is empty or
// would take the step past [MaxWarningBytes] in this attempt.
func (s *StepContext) RecordWarning(msg string) error {
	if msg == "" {
		return errors.New("pipeline: warning is empty")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed || s.r == nil {
		return ErrStepReturned
	}

	if s.warned+len(msg) > MaxWarningBytes {
		return fmt.Errorf("pipeline: the step's recorded warnings would exceed %d bytes", MaxWarningBytes)
	}

	if err := s.emitLocked(Event{Kind: EventWarning, Message: msg}); err != nil {
		return err
	}

	s.warned += len(msg)

	return nil
}

// Progress reports progress on the task named key. A total of zero means the
// total is unknown. It is best-effort, and viewers keep only the latest value
// for each key.
//
// Every report says when the step first reported on the task, in this
// attempt, so a viewer that joins late can say how long the task has run
// from the one report it sees. A step that does one thing after another
// gives each its own key.
func (s *StepContext) Progress(key, msg string, current, total int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed || s.r == nil {
		return
	}

	started, reported := s.tasks[key]
	if !reported {
		if s.tasks == nil {
			s.tasks = map[string]time.Time{}
		}

		started = s.r.clock.Now().UTC()
		s.tasks[key] = started
	}

	_ = s.emitLocked(Event{
		Kind: EventProgress, Key: key, Message: msg, Current: current, Total: total, Started: started,
	})
}

// Item records the step's result for one target, such as one host of a call
// that fans out over many. It is durable: a later attempt reads it back with
// PreviousItems.
//
// It is a record, and nothing acts on it. The run does not know which
// targets a step has, so one that is never reported is not missed. A failed
// item does not fail the step, which passes or fails by what its Run
// returns. And what a later attempt does about each target is the step's to
// decide, from PreviousItems. The latest result for a target replaces the
// one before.
//
// A target that passed with something wrong is [ItemWarned], with the
// message saying what. [State.Warnings] lists such items.
func (s *StepContext) Item(id string, status ItemStatus, msg string) error {
	return s.ItemWith(id, status, msg, nil)
}

// ItemWith is Item, with data of the step's own about the result: small and
// structured, for whatever reads the journal to show or act on, such as the
// host a check ran against, or a fingerprint an operator is to verify. It
// saves putting such things in the id or the message, to be parsed out
// again.
//
// The data is encoded as JSON when ItemWith is called, and kept with the
// item in [ItemState], until the target's next result replaces both. Nil is
// no data. ItemWith returns an error, and records nothing, if the data
// can't be encoded or is larger than [MaxItemDataBytes]. Strings in it are
// redacted, as a message is.
func (s *StepContext) ItemWith(id string, status ItemStatus, msg string, data any) error {
	if id == "" {
		return errors.New("pipeline: item id is empty")
	}

	switch status {
	case ItemPassed, ItemFailed, ItemSkipped, ItemWarned:
	default:
		return fmt.Errorf("pipeline: unknown item status %q", status)
	}

	var raw json.RawMessage

	if data != nil {
		var err error

		if raw, err = encodeEmbeddable(data); err != nil {
			return fmt.Errorf("pipeline: item %q: %w", id, err)
		}

		if len(raw) > MaxItemDataBytes {
			return fmt.Errorf("pipeline: item %q: data is %d bytes, over the %d limit", id, len(raw), MaxItemDataBytes)
		}
	}

	return s.emit(Event{Kind: EventItem, Item: id, ItemStatus: status, Message: msg, Data: raw})
}

// PreviousItems returns the results this step recorded for each target in
// earlier attempts, so a retry can skip the targets that already passed.
func (s *StepContext) PreviousItems() map[string]ItemState {
	if s.r == nil {
		return map[string]ItemState{}
	}

	return s.r.prior.Items(s.n.path)
}

// Inputs returns the value the run was given with [WithInputs], or nil. It
// is the value itself, not a decoded copy, so its Secret fields hold their
// values.
func (s *StepContext) Inputs() any {
	if s.r == nil {
		return nil
	}

	return s.r.inputs.live
}

// Prior returns where the group or step at path stood when this attempt
// started: whether an earlier attempt reached it, finished it, or called its
// Run. It does not change during the attempt. It is for steps that bring the
// system in line with how far an earlier attempt got, such as restoring a
// setting on the nodes that are already finished. In a forced run, and on a
// first attempt, nothing has been reached.
func (s *StepContext) Prior(path string) ElementState {
	if s.r == nil {
		return ElementState{Status: StatusNotReached}
	}

	return s.r.prior.Element(path)
}

// Record stores a small value, durably, before it returns: a later step, a
// later attempt or a rollback can read it with Recorded, even if this step
// never finishes. The value is encoded as JSON, and must not be secret.
//
// It returns an error, and records nothing, if the value can't be encoded or
// would take the step past [MaxRecordBytes] in this attempt. Nothing is cut
// short to fit: record where a large result is kept, not the result.
func (s *StepContext) Record(key string, value any) error {
	_, err := s.record(key, value, false)

	return err
}

// RecordOnce records value under key, as Record does, unless the step
// already has a value under it: one recorded earlier in this attempt, or by
// an earlier attempt and not forgotten since. It reports whether it
// recorded. The check and the record are one step, so if several goroutines
// of a step call it with one key, exactly one of them records.
//
// It is for a value that has to be the first one seen, such as what a
// setting was before the step changed it. A later attempt finds the setting
// already changed, and must not record that as what was there before.
//
// A value that is not recorded takes nothing from the step's allowance. One
// that is recorded lasts as every recorded value does: through a forced
// attempt, and until a rollback undoes the step or a recovery step resets
// its group, after which RecordOnce records again.
//
// It goes by what the journal holds now. A reset forgets every value
// beneath its group, the recovery step's own from an earlier attempt among
// them, so a recovery step that runs again records again, though Recorded
// still reads it what the attempt started with.
func (s *StepContext) RecordOnce(key string, value any) (bool, error) {
	return s.record(key, value, true)
}

// Recorded decodes into v the latest value recorded under path and key, by
// any step, in this attempt or an earlier one. It reports whether there was
// one.
func (s *StepContext) Recorded(path, key string, v any) (bool, error) {
	s.mu.Lock()
	returned := s.closed || s.r == nil
	s.mu.Unlock()

	if returned {
		return false, ErrStepReturned
	}

	raw, ok := s.r.recalled(path, key)
	if !ok {
		return false, nil
	}

	if err := json.Unmarshal(raw, v); err != nil {
		return true, fmt.Errorf("pipeline: decode recorded %s %q: %w", path, key, err)
	}

	return true, nil
}

// Custom records a domain-specific event of type typ, such as
// "deploy.endpoint-ready", for observers that know how to render it. It is
// best-effort: anything resume or rollback needs belongs in Record or Item.
//
// The data is encoded as JSON when Custom is called, and observers receive it
// as [json.RawMessage]. Custom returns an error, and records nothing, if the
// data can't be encoded or is larger than [MaxCustomBytes].
func (s *StepContext) Custom(typ string, data any) error {
	if err := validateName(typ); err != nil {
		return fmt.Errorf("pipeline: custom event type: %w", err)
	}

	raw, err := encodeEmbeddable(data)
	if err != nil {
		return fmt.Errorf("pipeline: custom event %q: %w", typ, err)
	}

	if len(raw) > MaxCustomBytes {
		return fmt.Errorf("pipeline: custom event %q: data is %d bytes, over the %d limit", typ, len(raw), MaxCustomBytes)
	}

	return s.emit(Event{Kind: EventCustom, Type: typ, Data: raw})
}

// Stdout returns a writer for the step's standard output, such as a
// subprocess's: cmd.Stdout = s.Stdout(). Each line is recorded as an output
// event, best-effort. A line ends at "\n", "\r\n" or a lone "\r", so progress
// that redraws a line is recorded once per update. What is left without a
// line break is recorded when the step returns. Writes after that fail with
// [ErrStepReturned].
func (s *StepContext) Stdout() io.Writer {
	return s.writer(&s.stdout, "stdout")
}

// Stderr is [StepContext.Stdout] for standard error.
func (s *StepContext) Stderr() io.Writer {
	return s.writer(&s.stderr, "stderr")
}

// record stores value under key, and reports whether it did. If once is
// set, it stores nothing when the step already has a value under key.
func (s *StepContext) record(key string, value any, once bool) (bool, error) {
	if key == "" {
		return false, errors.New("pipeline: record key is empty")
	}

	raw, err := encodeEmbeddable(value)
	if err != nil {
		return false, fmt.Errorf("pipeline: record %q: %w", key, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed || s.r == nil {
		return false, ErrStepReturned
	}

	if once && s.r.holds(s.n.path, key) {
		return false, nil
	}

	if s.recorded+len(raw) > MaxRecordBytes {
		return false, fmt.Errorf("pipeline: record %q: the step's recorded values would exceed %d bytes", key, MaxRecordBytes)
	}

	if err := s.emitLocked(Event{Kind: EventValue, Key: key, Value: raw}); err != nil {
		return false, err
	}

	s.recorded += len(raw)
	s.r.remember(s.n.path, key, raw)

	return true, nil
}

func (s *StepContext) writer(w **linesplit.Writer, stream string) io.Writer {
	s.mu.Lock()
	defer s.mu.Unlock()

	if *w == nil {
		// A redactor has to see each line whole, so with one set the writer
		// holds lines up to MaxRedactLine and output splits them afterwards.
		limit := MaxOutputLine
		if s.r != nil && s.r.redact.active() {
			limit = MaxRedactLine
		}

		*w = linesplit.NewWriter(limit, func(l linesplit.Line) { s.output(stream, l) })
	}

	return &stepWriter{s: s, w: *w}
}

// output records one line from a writer. It is called with the writer's lock
// held, and takes the context's.
func (s *StepContext) output(stream string, l linesplit.Line) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed || s.r == nil {
		return
	}

	for _, piece := range s.pieces(l) {
		s.recordOutput(stream, piece)
	}
}

// pieces returns what to record for a line from a writer. With no redactor
// that is the line itself, already cut to MaxOutputLine. With one, the line
// arrives whole: it is redacted and then cut, so a secret that would have
// straddled a cut is seen in one piece. A line too long to arrive whole is
// replaced by a notice, because it cannot be redacted safely.
func (s *StepContext) pieces(l linesplit.Line) []linesplit.Line {
	if !s.r.redact.active() {
		return []linesplit.Line{l}
	}

	switch {
	case l.Continued:
		// The rest of a line that was already replaced.
		return nil
	case len(l.Text) >= MaxRedactLine:
		return []linesplit.Line{{Text: fmt.Sprintf(
			"[output line of %d bytes or more hidden: too long to redact]", MaxRedactLine)}}
	}

	chunks := linesplit.Chunks(s.r.redact.text(l.Text), MaxOutputLine)
	out := make([]linesplit.Line, len(chunks))

	for i, c := range chunks {
		out[i] = linesplit.Line{Text: c, Warn: i == 1, Continued: i > 0}
	}

	return out
}

// recordOutput records one line or piece, within the step's output budget.
// The caller holds s.mu.
func (s *StepContext) recordOutput(stream string, l linesplit.Line) {
	// Each line or piece also costs one byte for its line break, so a flood
	// of blank lines is bounded too.
	cost := len(l.Text) + 1

	if !l.Continued {
		delete(s.cut, stream)
	}

	if s.cut[stream] || s.outBytes+cost > s.r.budget {
		if s.dropped == 0 {
			_ = s.emitLocked(Event{Kind: EventMessage, Level: LevelWarn, Message: fmt.Sprintf(
				"output past %d bytes is dropped", s.r.budget)})
		}

		if s.cut == nil {
			s.cut = map[string]bool{}
		}

		s.cut[stream] = true
		s.dropped++

		return
	}

	s.outBytes += cost

	if l.Warn {
		_ = s.emitLocked(Event{Kind: EventMessage, Level: LevelWarn, Message: fmt.Sprintf(
			"output line longer than %d bytes was split into pieces", MaxOutputLine)})
	}

	_ = s.emitLocked(Event{Kind: EventOutput, Stream: stream, Line: l.Text, Continued: l.Continued})
}

// stepWriter refuses writes once its step has returned. A write it admits is
// always recorded: the step's close waits for it before flushing.
type stepWriter struct {
	s *StepContext
	w *linesplit.Writer
}

func (sw *stepWriter) Write(p []byte) (int, error) {
	s := sw.s

	s.mu.Lock()

	if s.closing || s.closed || s.r == nil {
		s.mu.Unlock()

		return 0, ErrStepReturned
	}

	s.writes.Add(1)
	s.mu.Unlock()

	defer s.writes.Done()

	return sw.w.Write(p)
}

func (s *StepContext) emit(e Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.emitLocked(e)
}

func (s *StepContext) emitLocked(e Event) error {
	if s.closed || s.r == nil {
		return ErrStepReturned
	}

	e.Path = s.n.path

	return s.r.emit(e)
}

// close stops new output writes, waits for those already admitted, records
// what is left in the writers, and then makes the context inert.
func (s *StepContext) close() {
	s.mu.Lock()
	s.closing = true
	writers := []*linesplit.Writer{s.stdout, s.stderr}
	s.mu.Unlock()

	// Admitted writes record their lines through output, which takes s.mu,
	// so wait and flush without holding it.
	s.writes.Wait()

	for _, w := range writers {
		if w != nil {
			w.Flush()
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.dropped > 0 && !s.closed && s.r != nil {
		_ = s.emitLocked(Event{Kind: EventMessage, Level: LevelWarn, Message: fmt.Sprintf(
			"dropped %d output lines past the step's %d-byte budget", s.dropped, s.r.budget)})
	}

	s.closed = true
}

// remember keeps a value recorded in this attempt, for Recorded.
func (r *runner) remember(path, key string, raw json.RawMessage) {
	r.valuesMu.Lock()
	defer r.valuesMu.Unlock()

	if r.values[path] == nil {
		r.values[path] = map[string]json.RawMessage{}
	}

	r.values[path][key] = raw

	if r.standing == nil {
		r.standing = map[string]map[string]bool{}
	}

	if r.standing[path] == nil {
		r.standing[path] = map[string]bool{}
	}

	r.standing[path][key] = true
}

// forget notes that a recovery step has reset the group at path: the journal
// no longer holds the values recorded beneath it, in this attempt or before.
// Recorded still reads them, so that the recovery step can.
func (r *runner) forget(path string) {
	r.valuesMu.Lock()
	defer r.valuesMu.Unlock()

	if r.reset == nil {
		r.reset = map[string]bool{}
	}

	r.reset[path] = true

	maps.DeleteFunc(r.standing, func(p string, _ map[string]bool) bool { return strings.HasPrefix(p, path+"/") })
}

// holds reports whether the journal, as it is now, holds a value under path
// and key: one recorded in this attempt that no reset has forgotten since,
// or one from before it that no reset of this attempt has reached.
func (r *runner) holds(path, key string) bool {
	r.valuesMu.Lock()
	defer r.valuesMu.Unlock()

	if r.standing[path][key] {
		return true
	}

	for group := range r.reset {
		if strings.HasPrefix(path, group+"/") {
			return false
		}
	}

	_, ok := r.prev.Value(path, key)

	return ok
}

// recalled returns the latest value recorded under path and key: from this
// attempt if there is one, otherwise from the journal as it was at the start.
func (r *runner) recalled(path, key string) (json.RawMessage, bool) {
	r.valuesMu.Lock()
	raw, ok := r.values[path][key]
	r.valuesMu.Unlock()

	if ok {
		return raw, true
	}

	return r.prev.Value(path, key)
}

// encodeEmbeddable encodes v as JSON, and checks it still parses one level
// deeper, as it is inside an event: a value at the JSON depth limit encodes on
// its own but not within the event.
func encodeEmbeddable(v any) (json.RawMessage, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}

	if !json.Valid(append(append([]byte{'['}, raw...), ']')) {
		return nil, errors.New("value nests too deeply to embed")
	}

	return raw, nil
}
