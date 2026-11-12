package runbook_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ruffel/pipeline/runbook"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A file created while the gate waits approves it, as whoever its first
// line names, and is removed.
func TestFileApprover_ApprovesWhenTheFileAppears(t *testing.T) {
	t.Parallel()

	flag := filepath.Join(t.TempDir(), "continue")
	create := runbook.ObserverFunc(func(_ context.Context, e runbook.Event) {
		if e.Kind == runbook.EventGateWaiting {
			assert.NoError(t, os.WriteFile(flag, []byte("alice\nanything else\n"), 0o600))
		}
	})

	out := runPlan(t, t.Context(), mustPlan(t,
		runbook.Gate("go", runbook.GateOptions{Approvers: []runbook.Approver{runbook.FileApprover(flag, 1)}}),
		runbook.Effect("after", ok()),
	), runbook.WithObservers(create))

	require.NoError(t, out.err)

	released := kinds(out.events, runbook.EventGateReleased)
	require.Len(t, released, 1)
	assert.Equal(t, "file", released[0].By)
	assert.Equal(t, "alice", released[0].Who)
	assert.NoFileExists(t, flag)
}

// A file already there approves the gate as soon as it waits; one file
// approves one gate.
func TestFileApprover_AFileAlreadyThereApprovesOneGate(t *testing.T) {
	t.Parallel()

	flag := filepath.Join(t.TempDir(), "continue")
	require.NoError(t, os.WriteFile(flag, nil, 0o600))

	approver := runbook.FileApprover(flag, 0)
	second := make(chan struct{})

	out := runPlan(t, t.Context(), mustPlan(t,
		runbook.Gate("first", runbook.GateOptions{Approvers: []runbook.Approver{approver}}),
		runbook.Gate("second", runbook.GateOptions{Approvers: []runbook.Approver{approver, never(second)}}),
	), runbook.WithObservers(runbook.ObserverFunc(func(_ context.Context, e runbook.Event) {
		if e.Kind == runbook.EventGateWaiting && e.Path == "p/second" {
			assert.NoFileExists(t, flag, "the first gate took the file")
			assert.NoError(t, os.WriteFile(flag, []byte("bob"), 0o600))
		}
	})))

	require.NoError(t, out.err)

	released := kinds(out.events, runbook.EventGateReleased)
	require.Len(t, released, 2)
	assert.Equal(t, "p/first", released[0].Path)
	assert.Empty(t, released[0].Who, "an empty file names nobody")
	assert.Equal(t, "p/second", released[1].Path)
	assert.Equal(t, "bob", released[1].Who)
}
