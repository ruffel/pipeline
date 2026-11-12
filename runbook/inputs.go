package runbook

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
)

// MaxInputsBytes caps the encoded inputs of a run. They are stored with
// every attempt.
const MaxInputsBytes = 4 << 20

// secretMask is what a [Secret] shows in place of its value.
const secretMask = "[secret]"

// minMaskedSecret is the shortest input secret masked in text. Masking every
// occurrence of a one- or two-character value would garble everything else.
const minMaskedSecret = 4

// Secret is an input that must never be stored or shown: a password, a
// token, a licence key. Use it for such fields of the value given to
// [WithInputs]. [NewSecret] makes one, and Reveal returns its value. The zero
// Secret is empty.
//
// A Secret is never written to the journal: it encodes as JSON null, so it
// comes back empty from [State.DecodeInputs], and has to be supplied again
// on every attempt. It decodes from a string, so a consumer's own
// configuration can be read straight into one.
//
// fmt prints a Secret as "[secret]" with every verb, and never prints the
// value: not for %p, and not when the Secret is an unexported field of a
// struct being printed. Tools that follow pointers themselves, such as deep
// pretty-printers and debuggers, can still reach it.
//
// A Secret must not be used as a map key. [WithInputs] refuses inputs that
// do.
//
// Every occurrence of an input Secret's value (of four bytes or more) is
// also masked in what steps write: messages, output, errors and custom
// event data. Output is recorded a line at a time, so each line of a secret
// that spans several, such as a private key, is masked as well.
//
// Two Secrets made separately are not equal under ==, whatever they hold.
// Compare what Reveal returns.
type Secret struct {
	// A pointer, because fmt prints a struct's fields itself where it cannot
	// or does not call Format, and prints a pointer as an address.
	value *string
}

// NewSecret returns a Secret holding value.
func NewSecret(value string) Secret {
	return Secret{value: &value}
}

// Reveal returns the secret's value.
func (s Secret) Reveal() string {
	if s.value == nil {
		return ""
	}

	return *s.value
}

// String implements [fmt.Stringer], hiding the value.
func (Secret) String() string {
	return secretMask
}

// Format implements [fmt.Formatter], so that no verb shows the value.
func (Secret) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, secretMask)
}

// MarshalJSON encodes every Secret as null.
func (Secret) MarshalJSON() ([]byte, error) {
	return []byte("null"), nil
}

// MarshalText encodes every Secret as "[secret]", for encoders that write
// text.
func (Secret) MarshalText() ([]byte, error) {
	return []byte(secretMask), nil
}

// UnmarshalText sets the secret to text. JSON and YAML decoders use it for a
// string; a JSON null leaves the Secret as it is.
func (s *Secret) UnmarshalText(text []byte) error {
	*s = NewSecret(string(text))

	return nil
}

// inputs is a run's inputs, ready to record.
type inputs struct {
	live    any
	encoded json.RawMessage
	digest  string
	secrets []string
}

// newInputs encodes v, the value given to WithInputs. A run with no inputs
// has an empty digest.
func newInputs(v any) (*inputs, error) {
	if v == nil {
		return &inputs{}, nil
	}

	encoded, err := encodeEmbeddable(v)
	if err != nil {
		return nil, fmt.Errorf("runbook: inputs: %w", err)
	}

	if len(encoded) > MaxInputsBytes {
		return nil, fmt.Errorf("runbook: inputs are %d bytes, over the %d limit", len(encoded), MaxInputsBytes)
	}

	secrets, err := secretsIn(v)
	if err != nil {
		return nil, fmt.Errorf("runbook: inputs: %w", err)
	}

	digest, err := inputsDigest(encoded)
	if err != nil {
		return nil, fmt.Errorf("runbook: inputs: %w", err)
	}

	return &inputs{live: v, encoded: encoded, digest: digest, secrets: secrets}, nil
}

// inputsDigest identifies encoded inputs, for comparing one attempt's with
// the next. Before hashing, everything that holds nothing is left out: null,
// and any object or array left empty without them.
//
// A Secret encodes as null whatever it holds, but whether the field is
// written at all can depend on whether it is set: omitempty and omitzero
// leave out an unset one, and a nil pointer, or an empty map or slice of
// secrets, can be left out too. Without this, supplying a secret on resume
// that was empty before would look like changed inputs.
func inputsDigest(encoded []byte) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(encoded))
	dec.UseNumber()

	var v any

	if err := dec.Decode(&v); err != nil {
		return "", err
	}

	// Marshal writes object keys in sorted order.
	canonical, err := json.Marshal(dropEmpty(v))
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(canonical)

	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// dropEmpty returns v without its null members, or nil if that leaves
// nothing. An array keeps its nulls unless it holds nothing else: they give
// the other elements their places.
func dropEmpty(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, inner := range x {
			if inner = dropEmpty(inner); inner == nil {
				delete(x, k)
			} else {
				x[k] = inner
			}
		}

		if len(x) == 0 {
			return nil
		}
	case []any:
		empty := true

		for i := range x {
			x[i] = dropEmpty(x[i])
			empty = empty && x[i] == nil
		}

		if empty {
			return nil
		}
	}

	return v
}

// errSecretMapKey is returned for inputs that use a Secret as a map key.
var errSecretMapKey = errors.New("a Secret is used as a map key")

// secretsIn returns the text to mask for the Secrets anywhere in v: each
// value long enough to mask, and each line of a value that spans several.
// They are ordered longest first, so one that contains another is masked
// whole. It refuses a Secret used as a map key.
func secretsIn(v any) ([]string, error) {
	w := secretWalk{visited: map[visit]bool{}}

	if err := w.walk(reflect.ValueOf(v)); err != nil {
		return nil, err
	}

	var masks []string

	for _, secret := range w.found {
		masks = append(masks, secret)

		if strings.ContainsAny(secret, "\r\n") {
			masks = append(masks, strings.FieldsFunc(secret, func(r rune) bool { return r == '\r' || r == '\n' })...)
		}
	}

	masks = slices.DeleteFunc(masks, func(s string) bool { return len(s) < minMaskedSecret })

	slices.SortFunc(masks, func(a, b string) int {
		return cmp.Or(cmp.Compare(len(b), len(a)), cmp.Compare(a, b))
	})

	return slices.Compact(masks), nil
}

// secretWalk finds every Secret reachable from a value. It has no depth
// limit, so nothing is left unchecked; it visits each pointer, map and slice
// once, so inputs that point back at themselves end.
type secretWalk struct {
	found   []string
	visited map[visit]bool
}

// visit identifies a pointer, map or slice already walked.
type visit struct {
	typ reflect.Type
	ptr uintptr
	len int
}

func (w *secretWalk) walk(v reflect.Value) error {
	if !v.IsValid() {
		return nil
	}

	if v.Type() == reflect.TypeFor[Secret]() {
		if p := v.Field(0); !p.IsNil() {
			w.found = append(w.found, p.Elem().String())
		}

		return nil
	}

	if v.Kind() == reflect.Map && v.Type().Key() == reflect.TypeFor[Secret]() {
		return errSecretMapKey
	}

	if w.seen(v) {
		return nil
	}

	for _, inner := range valuesIn(v) {
		if err := w.walk(inner); err != nil {
			return err
		}
	}

	return nil
}

// seen reports whether v is a pointer, map or slice walked before, and
// remembers it if not.
func (w *secretWalk) seen(v reflect.Value) bool {
	at := visit{typ: v.Type()}

	switch v.Kind() { //nolint:exhaustive // Only kinds that can lead back to themselves.
	case reflect.Pointer, reflect.Map:
		at.ptr = v.Pointer()
	case reflect.Slice:
		at.ptr, at.len = v.Pointer(), v.Len()
	default:
		return false
	}

	if at.ptr == 0 {
		return false
	}

	if w.visited[at] {
		return true
	}

	w.visited[at] = true

	return false
}

// valuesIn returns the values directly inside v: what a pointer or interface
// holds, a struct's fields, and the elements of a slice, array or map.
func valuesIn(v reflect.Value) []reflect.Value {
	var inner []reflect.Value

	switch v.Kind() { //nolint:exhaustive // Only kinds that hold other values.
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			inner = append(inner, v.Elem())
		}
	case reflect.Struct:
		for _, field := range v.Fields() {
			inner = append(inner, field)
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			inner = append(inner, v.Index(i))
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			inner = append(inner, v.MapIndex(k))
		}
	}

	return inner
}
