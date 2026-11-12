package runbook

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
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
// [WithInputs].
//
// A Secret is never written to the journal: it encodes as JSON null, so it
// comes back empty from [State.DecodeInputs], and has to be supplied again
// on every attempt. It prints as "[secret]" with any fmt verb. Reveal
// returns the value.
//
// A Secret must not be used as a map key: encoders write string keys as they
// are. [WithInputs] refuses inputs that do.
//
// Every occurrence of an input Secret's value (of four bytes or more) is
// also masked in what steps write: messages, output, errors and custom
// event data.
type Secret string

// Reveal returns the secret's value.
func (s Secret) Reveal() string {
	return string(s)
}

// String implements [fmt.Stringer], hiding the value.
func (Secret) String() string {
	return secretMask
}

// Format implements [fmt.Formatter], so that no verb shows the value: fmt
// prints the raw string for a verb it doesn't know, such as %d.
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

	sum := sha256.Sum256(encoded)

	return &inputs{
		live:    v,
		encoded: encoded,
		digest:  "sha256:" + hex.EncodeToString(sum[:]),
		secrets: secrets,
	}, nil
}

// maxInputsDepth bounds the walk for secrets, in case the inputs point back
// at themselves.
const maxInputsDepth = 32

// errSecretMapKey is returned for inputs that use a Secret as a map key.
var errSecretMapKey = errors.New("a Secret is used as a map key, where it would be written out")

// secretsIn returns the values of the Secrets anywhere in v that are long
// enough to mask, longest first, so one secret that contains another is
// masked whole. It refuses a Secret used as a map key.
func secretsIn(v any) ([]string, error) {
	var found []string

	if err := collectSecrets(reflect.ValueOf(v), 0, &found); err != nil {
		return nil, err
	}

	slices.SortFunc(found, func(a, b string) int { return cmp.Compare(len(b), len(a)) })

	return slices.Compact(found), nil
}

func collectSecrets(v reflect.Value, depth int, found *[]string) error {
	if !v.IsValid() || depth > maxInputsDepth {
		return nil
	}

	if v.Type() == reflect.TypeFor[Secret]() {
		if s := v.String(); len(s) >= minMaskedSecret {
			*found = append(*found, s)
		}

		return nil
	}

	if v.Kind() == reflect.Map && v.Type().Key() == reflect.TypeFor[Secret]() {
		return errSecretMapKey
	}

	for _, inner := range valuesIn(v) {
		if err := collectSecrets(inner, depth+1, found); err != nil {
			return err
		}
	}

	return nil
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
