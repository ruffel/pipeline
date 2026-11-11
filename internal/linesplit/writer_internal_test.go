package linesplit

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
)

// One huge write must not be buffered whole, nor keep a large backing array
// alive for the tail it leaves.
func TestWriter_BoundsItsBuffer(t *testing.T) {
	t.Parallel()

	const limit = 64

	var lines int

	w := NewWriter(limit, func(Line) { lines++ })

	_, _ = w.Write(append(bytes.Repeat([]byte("x"), 1000*limit), "tail"...))

	assert.Equal(t, 1000, lines)
	assert.LessOrEqual(t, cap(w.buf), 4*limit)
	assert.Equal(t, "tail", string(w.buf))
}
