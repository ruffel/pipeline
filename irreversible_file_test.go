//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package pipeline_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ruffel/pipeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixture copies a journal that v0.4.0 wrote into the test's own directory.
func fixture(t *testing.T, name string) string {
	t.Helper()

	//nolint:gosec // A fixture in this package's own testdata.
	data, err := os.ReadFile(filepath.Join("testdata", "v0.4.0", name))
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), name)
	//nolint:gosec // A file in the test's own temp dir.
	require.NoError(t, os.WriteFile(path, data, 0o600))

	return path
}

// A journal that v0.4.0 wrote says everything that is needed: each attempt's
// run.started has its plan, markers and all. So the point of no return is
// read from journals written before the fix, with no change to the format.
func TestRollback_AJournalFromV040KeepsItsPointOfNoReturn(t *testing.T) {
	t.Parallel()

	for name, attempts := range map[string]int{
		// Attempt 1 ran alter-table, marked. Attempt 2 had it unmarked.
		"marker-dropped.jsonl": 2,
		// Attempt 1 ran it, attempt 2 had no such step, attempt 3 had it
		// back, unmarked.
		"path-put-back.jsonl": 3,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var c calls

			fails := true
			path := fixture(t, name)

			before, err := os.ReadFile(path) //nolint:gosec // A file in the test's own temp dir.
			require.NoError(t, err)

			state, err := pipeline.Inspect(path)
			require.NoError(t, err)
			require.Len(t, state.Attempts, attempts)
			require.False(t, state.Plan.Children[1].Irreversible, "the latest plan has no marker")
			assert.Equal(t, "p/alter-table", state.PointOfNoReturn())

			j, err := pipeline.OpenJournal(path)
			require.NoError(t, err)

			refusesAt(t, &c, j, laterPlan(t, &c, &fails, true))
			require.NoError(t, j.Close())

			after, err := os.ReadFile(path) //nolint:gosec // A file in the test's own temp dir.
			require.NoError(t, err)
			assert.True(t, bytes.Equal(before, after), "the refusal leaves the journal as it was")
		})
	}
}

// A journal that does not say what a step was when it ran is not taken to
// say it could be undone. No run writes such a journal: this one has had
// the plans cut out of it.
func TestRollback_AJournalWithoutItsPlansIsRefused(t *testing.T) {
	t.Parallel()

	var c calls

	fails := true
	plan := laterPlan(t, &c, &fails, true)
	path := journalPath(t)

	runToFile(t, path, plan)

	data, err := os.ReadFile(path) //nolint:gosec // A file in the test's own temp dir.
	require.NoError(t, err)

	var cut bytes.Buffer

	for line := range bytes.Lines(data) {
		var e map[string]json.RawMessage

		require.NoError(t, json.Unmarshal(line, &e))
		delete(e, "plan")

		out, err := json.Marshal(e)
		require.NoError(t, err)

		cut.Write(out)
		cut.WriteByte('\n')
	}

	require.NoError(t, os.WriteFile(path, cut.Bytes(), 0o600))

	j, err := pipeline.OpenJournal(path)
	require.NoError(t, err)

	t.Cleanup(func() { _ = j.Close() })

	back := resumeRollback(t, j, plan)
	require.ErrorIs(t, back.err, pipeline.ErrIrreversible)
	require.ErrorContains(t, back.err, "or may have: attempt 1 ran p/backup")
	assert.Empty(t, back.events)

	for _, name := range []string{"backup", "alter-table", "node1"} {
		assert.Zero(t, c.get("undo "+name), "nothing is undone")
	}
}
