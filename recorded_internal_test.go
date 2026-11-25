package pipeline

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// holds says what the journal holds now, which is the journal as the attempt
// found it, less what a reset in this attempt has forgotten, plus what the
// attempt has recorded since. A reset reaches only what is beneath its group.
func TestRecorded_HoldsFollowsTheResetsOfItsAttempt(t *testing.T) {
	t.Parallel()

	prev, err := Replay([]Event{
		{V: 1, Seq: 1, Kind: EventValue, Path: "p/node/a", Key: "prior", Value: json.RawMessage(`1`)},
		{V: 1, Seq: 2, Kind: EventValue, Path: "p/node2/a", Key: "prior", Value: json.RawMessage(`1`)},
	})
	require.NoError(t, err)

	r := &recorded{prev: prev}

	assert.True(t, r.holds("p/node/a", "prior"), "an earlier attempt recorded it")
	assert.False(t, r.holds("p/node/a", "other"))

	r.remember("p/node/b", "prior", json.RawMessage(`2`))
	assert.True(t, r.holds("p/node/b", "prior"), "this attempt recorded it")

	r.forget("p/node")

	assert.False(t, r.holds("p/node/a", "prior"), "the reset forgot what was there before the attempt")
	assert.False(t, r.holds("p/node/b", "prior"), "and what the attempt had recorded")
	assert.True(t, r.holds("p/node2/a", "prior"), "a group whose name only begins the same is not beneath it")

	_, read := r.recalled("p/node/b", "prior")
	assert.True(t, read, "Recorded still reads what the reset forgot")

	r.remember("p/node/restore", "prior", json.RawMessage(`3`))
	assert.True(t, r.holds("p/node/restore", "prior"), "what is recorded after the reset stands")
}
