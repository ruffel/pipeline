package pipeline

import (
	"bytes"
	"reflect"
	"strings"
	"unicode/utf8"
)

// envelope is the start of an event line: what every event carries, in the
// order the journal writes it.
type envelope struct {
	v   int
	seq int64
	// chatter reports a best-effort kind: output, progress, a message, a
	// custom event or a retry. Those change no state.
	chatter bool
}

// peekEvent reads an event line's envelope without decoding the line. Output
// is nearly all of a long journal and changes no state, so replay uses this
// to pass over it at the speed of a scan.
//
// It recognises only the exact layout the journal writes. For anything else
// it reports false, and the caller decodes the line in full. That includes a
// line where a later key could also set an envelope field: the decoder would
// use the later value.
func peekEvent(line []byte) (envelope, bool) {
	var env envelope

	rest, ok := bytes.CutPrefix(line, []byte(`{"v":`))
	if !ok {
		return env, false
	}

	v, rest, ok := leadingInt(rest, maxVersionDigits)
	if !ok {
		return env, false
	}

	if rest, ok = bytes.CutPrefix(rest, []byte(`,"seq":`)); !ok {
		return env, false
	}

	seq, rest, ok := leadingInt(rest, maxSeqDigits)
	if !ok {
		return env, false
	}

	if rest, ok = bytes.CutPrefix(rest, []byte(`,"ts":"`)); !ok {
		return env, false
	}

	// A timestamp holds no quotes or escapes, so it ends at the next quote.
	// One that does hold an escape is not the journal's, and is not trusted
	// to end there.
	end := bytes.IndexByte(rest, '"')
	if end < 0 || bytes.IndexByte(rest[:end], '\\') >= 0 {
		return env, false
	}

	if rest, ok = bytes.CutPrefix(rest[end:], []byte(`","kind":"`)); !ok {
		return env, false
	}

	end = bytes.IndexByte(rest, '"')
	if end < 0 {
		return env, false
	}

	kind := EventKind(rest[:end])

	if !onlyOtherKeys(rest[end+1:]) {
		return env, false
	}

	env.v, env.seq = int(v), seq

	switch kind { //nolint:exhaustive // Every other kind is decoded in full.
	case EventOutput, EventProgress, EventMessage, EventCustom, EventStepRetry:
		env.chatter = true
	}

	return env, true
}

// Digits allowed in the envelope's numbers. A version of up to nine digits
// fits an int on every platform; a sequence number of up to 18 fits an
// int64. Anything longer is left to the decoder.
const (
	maxVersionDigits = 9
	maxSeqDigits     = 18
)

// leadingInt parses the unsigned decimal number of at most limit digits at
// the start of b.
func leadingInt(b []byte, limit int) (int64, []byte, bool) {
	var n int64

	i := 0
	for i < len(b) && b[i] >= '0' && b[i] <= '9' {
		if i == limit {
			return 0, b, false
		}

		n = n*10 + int64(b[i]-'0')
		i++
	}

	return n, b[i:], i > 0
}

// journalKeys are the keys the journal writes after the envelope, each as
// it appears in a line (`,"name":`), by first letter. A key that matches one
// whole is not an envelope key, which is much cheaper to establish than
// reading the key and folding its case.
var journalKeys = func() [256][][]byte { //nolint:gochecknoglobals // Read-only, computed once from Event.
	var keys [256][][]byte

	for _, field := range reflect.VisibleFields(reflect.TypeFor[Event]()) {
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")

		if name == "" || name == "-" || isEnvelopeKey([]byte(name)) {
			continue
		}

		keys[name[0]] = append(keys[name[0]], []byte(`,"`+name+`":`))
	}

	return keys
}()

// onlyOtherKeys reports whether rest, the remainder of a line after the
// envelope, is the rest of an object whose keys cannot set an envelope field
// again. It walks the object's top-level keys and steps over their values.
// Anything unexpected, such as whitespace or an escaped key, gives false, so
// the line is decoded in full.
func onlyOtherKeys(rest []byte) bool {
	for {
		if len(rest) == 0 {
			return false
		}

		if rest[0] == '}' {
			return len(rest) == 1
		}

		var ok bool

		if rest, ok = journalKey(rest); !ok {
			if rest, ok = otherKey(rest); !ok {
				return false
			}
		}

		if rest, ok = skipValue(rest); !ok {
			return false
		}
	}
}

// journalKey steps over the key at the start of rest if it is one of the
// journal's own.
func journalKey(rest []byte) ([]byte, bool) {
	const first = len(`,"`) // Where the key's first letter is.

	if len(rest) <= first {
		return rest, false
	}

	for _, k := range journalKeys[rest[first]] {
		if bytes.HasPrefix(rest, k) {
			return rest[len(k):], true
		}
	}

	return rest, false
}

// otherKey steps over a key that is not one of the journal's, if it cannot
// be an envelope key either.
func otherKey(rest []byte) ([]byte, bool) {
	if rest[0] != ',' {
		return rest, false
	}

	key, after, ok := quoted(rest[1:])
	if !ok || bytes.IndexByte(key, '\\') >= 0 || isEnvelopeKey(key) {
		return rest, false
	}

	return bytes.CutPrefix(after, []byte(":"))
}

// maxFoldedKey is the longest an envelope key can be in UTF-8 once case is
// set aside: "kind" spelt with the Kelvin sign, which folds to k.
const maxFoldedKey = 6

func isEnvelopeKey(key []byte) bool {
	if len(key) == 0 || len(key) > maxFoldedKey {
		return false
	}

	for _, c := range key {
		if c >= utf8.RuneSelf {
			return foldsToEnvelopeKey(key)
		}
	}

	// An ASCII key: compare in lower case. Every envelope key is lower-case
	// letters, and only a letter's two cases map to one under | 0x20.
	const lowerCase = 0x20 // The bit that sets an ASCII letter in lower case.

	var lower [maxFoldedKey]byte

	for i, c := range key {
		lower[i] = c | lowerCase
	}

	switch string(lower[:len(key)]) {
	case "v", "seq", "ts", "kind":
		return true
	default:
		return false
	}
}

// foldsToEnvelopeKey compares a key that is not plain ASCII as the decoder
// does: Unicode simple folding, so "\u017feq" (long s) is "seq".
func foldsToEnvelopeKey(key []byte) bool {
	for _, k := range [...]string{"v", "seq", "ts", "kind"} {
		if strings.EqualFold(string(key), k) {
			return true
		}
	}

	return false
}

// quoted returns the content of the JSON string at the start of b, still
// escaped, and what follows it.
func quoted(b []byte) ([]byte, []byte, bool) {
	if len(b) == 0 || b[0] != '"' {
		return nil, b, false
	}

	for i := 1; ; {
		j := bytes.IndexByte(b[i:], '"')
		if j < 0 {
			return nil, b, false
		}

		end := i + j
		if escapes(b[1:end]) {
			// This quote is escaped: it is part of the string.
			i = end + 1

			continue
		}

		return b[1:end], b[end+1:], true
	}
}

// escapes reports whether s ends in an odd number of backslashes, which
// escape whatever follows it.
func escapes(s []byte) bool {
	n := 0
	for n < len(s) && s[len(s)-1-n] == '\\' {
		n++
	}

	return n%2 == 1
}

// skipValue steps over the JSON value at the start of b, returning what
// follows it. It does not check the value is well-formed, only where it
// ends: its strings, and its brackets, which must balance.
func skipValue(b []byte) ([]byte, bool) {
	if len(b) == 0 || b[0] == ',' || b[0] == '}' {
		return b, false
	}

	if after, ok := plainString(b); ok {
		return after, true
	}

	depth := 0

	for len(b) > 0 {
		var ok bool

		if b, depth, ok = skipToken(b, depth); !ok {
			return b, false
		}

		if depth == 0 && len(b) > 0 && (b[0] == ',' || b[0] == '}') {
			return b, true
		}
	}

	return b, false
}

// plainString steps over a string at the start of b with no escaped quote
// before its end, as nearly every value is.
func plainString(b []byte) ([]byte, bool) {
	if b[0] != '"' {
		return b, false
	}

	end := bytes.IndexByte(b[1:], '"') + 1
	if end == 0 || b[end-1] == '\\' {
		return b, false
	}

	return b[end+1:], true
}

// skipToken steps over one string, bracket or other byte of a value at the
// given depth of nesting, and returns the new depth.
func skipToken(b []byte, depth int) ([]byte, int, bool) {
	switch b[0] {
	case '"':
		_, after, ok := quoted(b)

		return after, depth, ok
	case '{', '[':
		return b[1:], depth + 1, true
	case '}', ']':
		return b[1:], depth - 1, depth > 0
	default:
		return b[1:], depth, true
	}
}
