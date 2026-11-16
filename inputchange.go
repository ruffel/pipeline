package pipeline

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// InputChange is a change of inputs on resume: what the journal's latest
// attempt was given with [WithInputs], and what this one was.
type InputChange struct {
	// Old and New are the encoded inputs, without their secrets.
	Old, New json.RawMessage
	// Changed are the fields that differ, each named by its JSON pointer
	// into the encoded inputs, such as "/hosts/2/address", in sorted order.
	// A pointer names the deepest value that differs: a field of an object,
	// or an element of an array. A field that is null, empty or left out is
	// the same field, as it is to [ErrInputsChanged], and a [Secret] never
	// differs.
	Changed []string
	// State is the journal, replayed: what has started, and what was
	// recorded.
	State *State
}

// InputFieldChange is one changed field of the inputs that a resume could
// not accept.
type InputFieldChange struct {
	// Pointer is the field's JSON pointer into the encoded inputs.
	Pointer string
	// Old and New are its values, encoded. One that was not there is nil.
	Old, New json.RawMessage
	// Consumer is the path of a step or group that uses the field and has
	// already started. It is empty when the field has no consumer declared,
	// and so counts as used by every step.
	Consumer string
}

// InputChangeError is why a resume under [WithInputConsumers] refused its
// inputs: each changed field that something has already used. It matches
// [ErrInputsChanged].
type InputChangeError struct {
	Fields []InputFieldChange
}

// Error implements error.
func (e *InputChangeError) Error() string {
	parts := make([]string, 0, len(e.Fields))

	for _, f := range e.Fields {
		by := "no consumer is declared for it"
		if f.Consumer != "" {
			by = f.Consumer + " has already used it"
		}

		parts = append(parts, fmt.Sprintf("%s changed from %s to %s, and %s",
			shownPointer(f.Pointer), shownValue(f.Old), shownValue(f.New), by))
	}

	return ErrInputsChanged.Error() + ": " + strings.Join(parts, "; ")
}

// Unwrap returns [ErrInputsChanged].
func (e *InputChangeError) Unwrap() error {
	return ErrInputsChanged
}

func shownPointer(p string) string {
	if p == "" {
		return "the inputs"
	}

	return p
}

func shownValue(v json.RawMessage) string {
	if v == nil {
		return "nothing"
	}

	return string(v)
}

// checkConsumers reports the first consumer that plan does not have. It is
// checked on every attempt, whether or not the inputs have changed, so a
// mistyped path is found before the run that needs it.
func checkConsumers(plan *Plan, consumers map[string][]string) error {
	known := map[string]bool{}

	plan.root.walk(func(n *node) { known[n.path] = true })

	for _, pointer := range slices.Sorted(maps.Keys(consumers)) {
		for _, path := range consumers[pointer] {
			if !known[path] {
				return fmt.Errorf("pipeline: input consumer %s, of %s, is not in the plan", path, shownPointer(pointer))
			}
		}
	}

	return nil
}

// consumersHook is the rule of WithInputConsumers, as a WithInputChange hook.
func consumersHook(consumers map[string][]string) func(InputChange) error {
	return func(c InputChange) error {
		var refused []InputFieldChange

		for _, pointer := range c.Changed {
			used, declared := usedBy(c.State, consumers, pointer)
			if declared && used == "" {
				continue
			}

			refused = append(refused, InputFieldChange{
				Pointer: pointer, Old: valueAt(c.Old, pointer), New: valueAt(c.New, pointer), Consumer: used,
			})
		}

		if len(refused) > 0 {
			return &InputChangeError{Fields: refused}
		}

		return nil
	}
}

// usedBy returns the first consumer of the field at pointer that has
// started, and whether the field has any consumer declared: under its own
// pointer, or under one that covers it.
//
// A change to a whole container, such as a list that was removed, changes
// every field beneath it too. So the consumers declared beneath pointer are
// asked as well, though they do not make the field itself declared.
func usedBy(s *State, consumers map[string][]string, pointer string) (string, bool) {
	declared, used := false, ""

	for _, key := range slices.Sorted(maps.Keys(consumers)) {
		covers := key == "" || key == pointer || strings.HasPrefix(pointer, key+"/")
		beneath := pointer == "" || strings.HasPrefix(key, pointer+"/")

		if !covers && !beneath {
			continue
		}

		declared = declared || covers

		if i := slices.IndexFunc(consumers[key], s.started); i >= 0 && used == "" {
			used = consumers[key][i]
		}
	}

	return used, declared
}

// started reports whether the step or gate at path, or any beneath it, has
// been started by an attempt and not taken back since: by an Undo, or by its
// group's reset. One that was only recorded as skipped, because its group or
// the run ended before reaching it, has not.
func (s *State) started(path string) bool {
	for p := range s.begun {
		if p == path || strings.HasPrefix(p, path+"/") {
			return true
		}
	}

	return false
}

// changedInputs returns the JSON pointers of the fields that differ between
// two encoded inputs, sorted. They are compared as their digests are:
// without anything that holds nothing.
func changedInputs(before, after json.RawMessage) ([]string, error) {
	was, err := decodeInputs(before)
	if err != nil {
		return nil, err
	}

	now, err := decodeInputs(after)
	if err != nil {
		return nil, err
	}

	var changed []string

	diffInputs("", was, now, &changed)
	slices.Sort(changed)

	return changed, nil
}

func decodeInputs(encoded json.RawMessage) (any, error) {
	if len(encoded) == 0 {
		return nil, nil //nolint:nilnil // No inputs hold nothing.
	}

	dec := json.NewDecoder(bytes.NewReader(encoded))
	dec.UseNumber()

	var v any

	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("pipeline: inputs: %w", err)
	}

	return dropEmpty(v), nil
}

// diffInputs appends the pointer of every value that differs between was
// and now, both found at pointer at.
func diffInputs(at string, was, now any, changed *[]string) {
	switch w := was.(type) {
	case map[string]any:
		n, ok := now.(map[string]any)
		if !ok {
			break
		}

		keys := map[string]bool{}
		for k := range w {
			keys[k] = true
		}

		for k := range n {
			keys[k] = true
		}

		for k := range keys {
			diffInputs(at+"/"+escapePointer(k), w[k], n[k], changed)
		}

		return
	case []any:
		n, ok := now.([]any)
		if !ok {
			break
		}

		for i := range max(len(w), len(n)) {
			index := at + "/" + strconv.Itoa(i)

			// An element that one array has and the other lacks differs,
			// even if it is null: in an array, a null holds a place.
			if i >= len(w) || i >= len(n) {
				*changed = append(*changed, index)

				continue
			}

			diffInputs(index, w[i], n[i], changed)
		}

		return
	}

	// A scalar, or a value whose kind changed. Numbers are json.Number, so
	// they compare as written.
	if was != now {
		*changed = append(*changed, at)
	}
}

// escapePointer escapes an object key for a JSON pointer (RFC 6901).
func escapePointer(key string) string {
	return strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
}

// valueAt returns the value at pointer in encoded, or nil if there is none.
func valueAt(encoded json.RawMessage, pointer string) json.RawMessage {
	if len(encoded) == 0 {
		return nil
	}

	// Numbers stay as they were written: a float64 would round a large one.
	dec := json.NewDecoder(bytes.NewReader(encoded))
	dec.UseNumber()

	var v any

	if dec.Decode(&v) != nil {
		return nil
	}

	if pointer != "" {
		for token := range strings.SplitSeq(pointer[1:], "/") {
			token = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")

			switch x := v.(type) {
			case map[string]any:
				inner, ok := x[token]
				if !ok {
					return nil
				}

				v = inner
			case []any:
				i, err := strconv.Atoi(token)
				if err != nil || i < 0 || i >= len(x) {
					return nil
				}

				v = x[i]
			default:
				return nil
			}
		}
	}

	out, err := json.Marshal(v)
	if err != nil {
		return nil
	}

	return out
}
