package runbook

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sync"
)

// Size limits on what a step writes, so the journal stays quick to replay.
const (
	// MaxRecordBytes caps the encoded values one step records in one attempt.
	MaxRecordBytes = 64 << 10
	// MaxCustomBytes caps the encoded data of one custom event.
	MaxCustomBytes = 64 << 10
)

// StepContext gives a running step access to the run: messages, progress,
// per-target results, recorded values and custom events. The executor makes
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
	_ = s.emit(Event{Kind: EventMessage, Level: "info", Message: msg})
}

// Warn records a warning. It is best-effort.
func (s *StepContext) Warn(msg string) {
	_ = s.emit(Event{Kind: EventMessage, Level: "warn", Message: msg})
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

	return s.r.prev.Items(s.n.path)
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

// close makes the context inert, after waiting for any write in flight.
func (s *StepContext) close() {
	s.mu.Lock()
	defer s.mu.Unlock()

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
