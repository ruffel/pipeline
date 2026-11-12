package runbook

import "bytes"

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
// it reports false, and the caller decodes the line in full. It reads the
// first "kind" on the line; the journal never writes a key twice.
func peekEvent(line []byte) (envelope, bool) {
	var env envelope

	rest, ok := bytes.CutPrefix(line, []byte(`{"v":`))
	if !ok || len(line) == 0 || line[len(line)-1] != '}' {
		return env, false
	}

	v, rest, ok := leadingInt(rest)
	if !ok {
		return env, false
	}

	if rest, ok = bytes.CutPrefix(rest, []byte(`,"seq":`)); !ok {
		return env, false
	}

	seq, rest, ok := leadingInt(rest)
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

	env.v, env.seq = int(v), seq

	switch EventKind(rest[:end]) { //nolint:exhaustive // Every other kind is decoded in full.
	case EventOutput, EventProgress, EventMessage, EventCustom, EventStepRetry:
		env.chatter = true
	}

	return env, true
}

// maxPeekDigits keeps leadingInt well inside an int64.
const maxPeekDigits = 18

// leadingInt parses the unsigned decimal number at the start of b.
func leadingInt(b []byte) (int64, []byte, bool) {
	var n int64

	i := 0
	for i < len(b) && i < maxPeekDigits && b[i] >= '0' && b[i] <= '9' {
		n = n*10 + int64(b[i]-'0')
		i++
	}

	return n, b[i:], i > 0
}
