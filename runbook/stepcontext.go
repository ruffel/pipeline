package runbook

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"sync"

	"github.com/ruffel/pipeline/internal/linesplit"
)

// Size limits on what a step writes, so the journal stays quick to replay.
const (
	// MaxRecordBytes caps the encoded values one step records in one attempt.
	MaxRecordBytes = 64 << 10
	// MaxCustomBytes caps the encoded data of one custom event.
	MaxCustomBytes = 64 << 10
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
// output, per-target results, recorded values and custom events. The executor makes
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

	// Output writers, created on first use, and the step's output so far.
	stdout, stderr *linesplit.Writer
	outBytes       int
	dropped        int

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

// Warn records a warning. It is best-effort.
func (s *StepContext) Warn(msg string) {
	_ = s.emit(Event{Kind: EventMessage, Level: LevelWarn, Message: msg})
}

// Progress reports progress on the task named key. A total of zero means the
// total is unknown. It is best-effort, and viewers keep only the latest value
// for each key.
func (s *StepContext) Progress(key, msg string, current, total int) {
	_ = s.emit(Event{Kind: EventProgress, Key: key, Message: msg, Current: current, Total: total})
}

// Item records the step's result for one target, such as one host of a call
// that fans out over many. It is durable: a later attempt reads it back with
// PreviousItems.
func (s *StepContext) Item(id string, status ItemStatus, msg string) error {
	if id == "" {
		return errors.New("runbook: item id is empty")
	}

	switch status {
	case ItemPassed, ItemFailed, ItemSkipped:
	default:
		return fmt.Errorf("runbook: unknown item status %q", status)
	}

	return s.emit(Event{Kind: EventItem, Item: id, Status: status, Message: msg})
}

// PreviousItems returns the results this step recorded for each target in
// earlier attempts, so a retry can skip the targets that already passed.
func (s *StepContext) PreviousItems() map[string]ItemState {
	if s.r == nil {
		return map[string]ItemState{}
	}

	return s.r.prior.Items(s.n.path)
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
// would take the step past [MaxRecordBytes] in this attempt.
func (s *StepContext) Record(key string, value any) error {
	if key == "" {
		return errors.New("runbook: record key is empty")
	}

	raw, err := encodeEmbeddable(value)
	if err != nil {
		return fmt.Errorf("runbook: record %q: %w", key, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.recorded+len(raw) > MaxRecordBytes {
		return fmt.Errorf("runbook: record %q: the step's recorded values would exceed %d bytes", key, MaxRecordBytes)
	}

	if err := s.emitLocked(Event{Kind: EventValue, Key: key, Value: raw}); err != nil {
		return err
	}

	s.recorded += len(raw)
	s.r.remember(s.n.path, key, raw)

	return nil
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
		return true, fmt.Errorf("runbook: decode recorded %s %q: %w", path, key, err)
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
		return fmt.Errorf("runbook: custom event type: %w", err)
	}

	raw, err := encodeEmbeddable(data)
	if err != nil {
		return fmt.Errorf("runbook: custom event %q: %w", typ, err)
	}

	if len(raw) > MaxCustomBytes {
		return fmt.Errorf("runbook: custom event %q: data is %d bytes, over the %d limit", typ, len(raw), MaxCustomBytes)
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

	if s.outBytes+cost > s.r.budget {
		if s.dropped == 0 {
			_ = s.emitLocked(Event{Kind: EventMessage, Level: LevelWarn, Message: fmt.Sprintf(
				"output past %d bytes is dropped", s.r.budget)})
		}

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
