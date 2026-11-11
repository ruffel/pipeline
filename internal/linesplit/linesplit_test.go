package linesplit_test

import (
	"bufio"
	"bytes"
	"math/rand/v2"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ruffel/pipeline/internal/linesplit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const limit = 64

// scanned splits input with a bufio.Scanner, the path v1's OutputWriter uses.
func scanned(t *testing.T, input string) []linesplit.Line {
	t.Helper()

	s := linesplit.New(limit)
	scanner := bufio.NewScanner(strings.NewReader(input))
	scanner.Buffer(make([]byte, 0, limit), limit)
	scanner.Split(s.Split)

	var lines []linesplit.Line

	for scanner.Scan() {
		lines = append(lines, linesplit.Line{Text: scanner.Text(), Warn: s.Warn(), Continued: s.Continued()})
	}

	require.NoError(t, scanner.Err())

	return lines
}

// written splits input with a Writer, fed in chunks of the given sizes.
func written(input string, sizes func() int) []linesplit.Line {
	var lines []linesplit.Line

	w := linesplit.NewWriter(limit, func(l linesplit.Line) { lines = append(lines, l) })

	for rest := input; rest != ""; {
		n := min(max(sizes(), 1), len(rest))
		_, _ = w.Write([]byte(rest[:n]))
		rest = rest[n:]
	}

	w.Flush()

	return lines
}

func x(n int) string { return strings.Repeat("x", n) }

func TestWriter_MatchesTheScanner(t *testing.T) {
	t.Parallel()

	inputs := map[string]string{
		"plain lines":                    "one\ntwo\nthree\n",
		"no final newline":               "one\ntwo",
		"CRLF":                           "a\r\nb\r\n",
		"progress":                       "\r10%\r20%\r100%\r\ndone\n",
		"empty lines":                    "\n\nx\n\n",
		"exactly the limit":              x(limit) + "\nnext\n",
		"exactly the limit at EOF":       x(limit),
		"one under the limit then CRLF":  x(limit-1) + "\r\nnext\n",
		"one under the limit then CR":    x(limit-1) + "\rnext\n",
		"twice the limit":                x(2*limit) + "\n" + x(2*limit) + "\n",
		"three pieces":                   x(2*limit+1) + "\n",
		"long line ending in CRLF":       x(limit) + "\r\n" + x(limit*3) + "\r\nend\n",
		"long line without a line break": x(limit*3 + 5),
		"2-byte character at the cut":    x(limit-1) + "é" + x(limit) + "\n",
		"4-byte character at the cut":    x(limit-2) + "😀" + "tail\n",
		"multibyte characters only":      strings.Repeat("é", limit*2) + "\n",
	}

	for name, input := range inputs {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			want := scanned(t, input)

			assert.Equal(t, want, written(input, func() int { return len(input) }), "one write")
			assert.Equal(t, want, written(input, func() int { return 1 }), "a byte at a time")

			rng := rand.New(rand.NewPCG(1, uint64(len(input)))) //nolint:gosec // A seeded generator keeps the chunking reproducible.
			for range 20 {
				assert.Equal(t, want, written(input, func() int { return rng.IntN(2 * limit) }), "random chunks")
			}
		})
	}
}

func TestWriter_MarksLongLines(t *testing.T) {
	t.Parallel()

	lines := written(x(2*limit+1)+"\nshort\n", func() int { return 1000 })

	assert.Equal(t, []linesplit.Line{
		{Text: x(limit)},
		{Text: x(limit), Warn: true, Continued: true},
		{Text: "x", Continued: true},
		{Text: "short"},
	}, lines)
}

func TestWriter_KeepsUTF8CharactersWhole(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		input string
		want  []string
	}{
		"2-byte":  {x(limit-1) + "é\n", []string{x(limit - 1), "é"}},
		"3-byte":  {x(limit-2) + "€\n", []string{x(limit - 2), "€"}},
		"4-byte":  {x(limit-3) + "😀\n", []string{x(limit - 3), "😀"}},
		"fits":    {x(limit-2) + "é\n", []string{x(limit-2) + "é"}},
		"invalid": {x(limit-1) + "\xff\xff\n", []string{x(limit-1) + "\xff", "\xff"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			lines := written(tc.input, func() int { return 1000 })
			texts := make([]string, 0, len(lines))

			for _, l := range lines {
				texts = append(texts, l.Text)

				if name != "invalid" {
					assert.True(t, utf8.ValidString(l.Text), "piece %q splits a character", l.Text)
				}
			}

			assert.Equal(t, tc.want, texts)
		})
	}
}

func TestWriter_IsSafeForConcurrentWrites(t *testing.T) {
	t.Parallel()

	var (
		lines []linesplit.Line
		done  = make(chan struct{})
	)

	w := linesplit.NewWriter(limit, func(l linesplit.Line) { lines = append(lines, l) })

	for range 4 {
		go func() {
			defer func() { done <- struct{}{} }()

			for range 100 {
				_, _ = w.Write([]byte("line\n"))
			}
		}()
	}

	for range 4 {
		<-done
	}

	w.Flush()
	assert.Len(t, lines, 400)

	var all bytes.Buffer
	for _, l := range lines {
		all.WriteString(l.Text)
	}

	assert.Equal(t, strings.Repeat("line", 400), all.String())
}
