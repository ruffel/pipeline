package runbook

import (
	"bytes"
	"encoding/json"
)

// redactionFailed replaces a string the redactor could not process. The
// original is never written in its place.
const redactionFailed = "[redaction failed]"

// redactor applies a consumer's redaction function, safely.
type redactor struct {
	fn func(string) string
}

// text redacts s. If the function panics, s is replaced whole: a string that
// could not be checked must not be written.
func (r *redactor) text(s string) string {
	if r == nil || r.fn == nil || s == "" {
		return s
	}

	out := redactionFailed

	func() {
		defer func() { _ = recover() }()

		out = r.fn(s)
	}()

	return out
}

// event redacts the free text in e: messages, output lines, errors, and the
// strings inside a custom event's data. Names, paths, labels, keys and
// recorded values are left alone: they are the plan's and the step's own
// data, which must not be secret.
func (r *redactor) event(e Event) Event {
	if r == nil || r.fn == nil {
		return e
	}

	e.Message = r.text(e.Message)
	e.Line = r.text(e.Line)

	if e.Error != nil {
		info := *e.Error
		info.Message = r.text(info.Message)
		info.Stack = r.text(info.Stack)
		e.Error = &info
	}

	if e.Kind == EventCustom && len(e.Data) > 0 {
		e.Data = r.json(e.Data)
	}

	return e
}

// json redacts every string value in raw, leaving keys, numbers and
// structure as they are. If raw can't be rewritten it is replaced whole.
func (r *redactor) json(raw json.RawMessage) json.RawMessage {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	var v any

	if err := dec.Decode(&v); err != nil {
		return failedJSON()
	}

	out, err := json.Marshal(r.value(v))
	if err != nil {
		return failedJSON()
	}

	return out
}

func (r *redactor) value(v any) any {
	switch x := v.(type) {
	case string:
		return r.text(x)
	case []any:
		for i := range x {
			x[i] = r.value(x[i])
		}

		return x
	case map[string]any:
		for k := range x {
			x[k] = r.value(x[k])
		}

		return x
	default:
		return v
	}
}

// failedJSON is redactionFailed as a JSON string.
func failedJSON() json.RawMessage {
	return json.RawMessage(`"` + redactionFailed + `"`)
}

// redactedError is an error whose message has been redacted. It still
// unwraps to the original, so errors.Is and errors.As work on it.
type redactedError struct {
	text string
	err  error
}

func (e *redactedError) Error() string {
	return e.text
}

func (e *redactedError) Unwrap() error {
	return e.err
}

// err returns err with its message redacted.
func (r *redactor) err(err error) error {
	if r == nil || r.fn == nil || err == nil {
		return err
	}

	return &redactedError{text: r.text(err.Error()), err: err}
}
