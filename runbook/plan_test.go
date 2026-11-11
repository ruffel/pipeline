package runbook_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ruffel/pipeline/runbook"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// noop is a step with Run only.
func noop() runbook.Step {
	return runbook.StepFunc(func(context.Context, *runbook.StepContext) error { return nil })
}

// checked is a step with Check.
type checked struct{}

func (checked) Run(context.Context, *runbook.StepContext) error           { return nil }
func (checked) Check(context.Context, *runbook.StepContext) (bool, error) { return false, nil }

// pointerStep has a pointer receiver, so a nil *pointerStep is a nil Step value.
type pointerStep struct{}

func (*pointerStep) Run(context.Context, *runbook.StepContext) error { return nil }

// reversible is a step with Check and Undo.
type reversible struct{ checked }

func (reversible) Undo(context.Context, *runbook.StepContext) error { return nil }

func TestNewPlan_PathsAndInheritedLabels(t *testing.T) {
	t.Parallel()

	plan, err := runbook.NewPlan("upgrade",
		runbook.Group("region1",
			runbook.Group("DC1",
				runbook.Effect("packages", noop()).Labels(runbook.L{"node": "n1", "dc": "override"}),
			).Labels(runbook.L{"dc": "DC1"}),
		).Labels(runbook.L{"region": "r1"}),
	)
	require.NoError(t, err)

	root := plan.Spec()
	assert.Equal(t, "upgrade", plan.Name())
	assert.Equal(t, "upgrade", root.Path)
	assert.Equal(t, runbook.KindGroup, root.Kind)
	assert.Nil(t, root.Labels)

	region := root.Children[0]
	dc := region.Children[0]
	step := dc.Children[0]

	assert.Equal(t, "upgrade/region1", region.Path)
	assert.Equal(t, "upgrade/region1/DC1", dc.Path)
	assert.Equal(t, "upgrade/region1/DC1/packages", step.Path)

	assert.Equal(t, runbook.L{"region": "r1"}, region.Labels)
	assert.Equal(t, runbook.L{"region": "r1", "dc": "DC1"}, dc.Labels)
	assert.Equal(t, runbook.L{"region": "r1", "dc": "override", "node": "n1"}, step.Labels, "own value wins")
}

func TestNewPlan_ReportsEveryProblemWithItsPath(t *testing.T) {
	t.Parallel()

	_, err := runbook.NewPlan("p",
		runbook.Effect("a", noop()),
		runbook.Effect("a", noop()),
		runbook.Effect("", noop()),
		runbook.Effect("bad/name", noop()),
		runbook.Effect("-leading", noop()),
		runbook.Effect("nil-step", nil),
		runbook.Effect("nil-func", runbook.StepFunc(nil)),
		runbook.Effect("nil-pointer", (*pointerStep)(nil)),
		runbook.Observe("probe", checked{}),
		runbook.Effect("retry", noop()).Retry(0, time.Second),
		runbook.Effect("timeout", noop()).Timeout(-time.Second),
		runbook.Group("g").Parallel(-1),
	)
	require.Error(t, err)

	for _, want := range []string{
		"p/a: duplicate name",
		"p/: name is empty",
		`p/bad/name: name "bad/name" may only use`,
		`p/-leading: name "-leading" may only use`,
		"p/nil-step: step is nil",
		"p/nil-func: step is nil",
		"p/nil-pointer: step is nil",
		"p/probe: an observe step may not have Check or Undo",
		"p/retry: retry allows 0 tries",
		"p/timeout: backoff and timeout may not be negative",
		"p/g: parallel limit -1 is negative",
	} {
		assert.Contains(t, err.Error(), want)
	}
}

func TestNewPlan_AcceptsValidNamesAndEmptyGroups(t *testing.T) {
	t.Parallel()

	_, err := runbook.NewPlan("p",
		runbook.Effect("core-packages_v1.2", noop()),
		runbook.Effect("9lives", noop()),
		runbook.Group("empty"),
	)
	require.NoError(t, err)

	_, err = runbook.NewPlan("ünicode")
	require.Error(t, err, "names are ASCII so they are safe as file names")
}

func TestPlan_SpecCarriesSettingsAndCapabilities(t *testing.T) {
	t.Parallel()

	plan, err := runbook.NewPlan("p",
		runbook.Group("checks",
			runbook.Observe("ring", noop()),
		).Parallel(8).ContinueOnError().Description("Read-only checks"),
		runbook.Effect("backup", reversible{}).Retry(3, time.Second).Timeout(time.Minute),
		runbook.Ensure("maintenance", checked{}),
		runbook.Group("nodes").Parallel(0).CancelOnError(),
	)
	require.NoError(t, err)

	children := plan.Spec().Children

	assert.Equal(t, runbook.NodeSpec{
		Name: "checks", Path: "p/checks", Kind: runbook.KindGroup, Description: "Read-only checks",
		Parallel: true, Limit: 8, Failure: runbook.FailureContinue,
		Children: []runbook.NodeSpec{{Name: "ring", Path: "p/checks/ring", Kind: runbook.KindObserve, Tries: 1}},
	}, children[0])
	assert.Equal(t, runbook.NodeSpec{
		Name: "backup", Path: "p/backup", Kind: runbook.KindEffect, Check: true, Undo: true,
		Tries: 3, Backoff: time.Second, Timeout: time.Minute,
	}, children[1])
	assert.Equal(t, runbook.NodeSpec{
		Name: "maintenance", Path: "p/maintenance", Kind: runbook.KindEnsure, Check: true, Tries: 1,
	}, children[2])
	assert.Equal(t, runbook.FailureCancel, children[3].Failure)
	assert.True(t, children[3].Parallel)
	assert.Equal(t, runbook.FailureStop, plan.Spec().Failure, "the root is a sequential group with the default policy")
}

func TestPlan_SpecRoundTripsThroughJSON(t *testing.T) {
	t.Parallel()

	plan, err := runbook.NewPlan("p",
		runbook.Group("g",
			runbook.Effect("e", reversible{}).Retry(2, 500*time.Millisecond).Timeout(time.Second),
		).Parallel(2).Labels(runbook.L{"k": "v"}),
	)
	require.NoError(t, err)

	raw, err := json.Marshal(plan.Spec())
	require.NoError(t, err)

	var back runbook.NodeSpec
	require.NoError(t, json.Unmarshal(raw, &back))
	assert.Equal(t, plan.Spec(), back)
	assert.JSONEq(t, `{
		"name": "p", "path": "p", "kind": "group", "failure": "stop",
		"children": [{
			"name": "g", "path": "p/g", "kind": "group", "labels": {"k": "v"},
			"parallel": true, "limit": 2, "failure": "stop",
			"children": [{
				"name": "e", "path": "p/g/e", "kind": "effect", "labels": {"k": "v"}, "check": true, "undo": true,
				"tries": 2, "backoff": 500000000, "timeout": 1000000000
			}]
		}]
	}`, string(raw))
}

func TestPlan_Fingerprint(t *testing.T) {
	t.Parallel()

	build := func(elems ...runbook.Element) string {
		t.Helper()

		plan, err := runbook.NewPlan("p", elems...)
		require.NoError(t, err)

		return plan.Fingerprint()
	}

	base := build(runbook.Group("g", runbook.Effect("a", noop()), runbook.Observe("b", noop())))

	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, base)

	t.Run("ignores wording and settings", func(t *testing.T) {
		t.Parallel()

		assert.Equal(t, base, build(runbook.Group("g",
			runbook.Effect("a", noop()).Description("new words").Retry(3, time.Second).Timeout(time.Minute),
			runbook.Observe("b", noop()).Labels(runbook.L{"x": "y"}),
		).Parallel(4).ContinueOnError().Description("group words")))
	})

	t.Run("changes with identity", func(t *testing.T) {
		t.Parallel()

		for name, other := range map[string]string{
			"renamed step": build(runbook.Group("g", runbook.Effect("a2", noop()), runbook.Observe("b", noop()))),
			"changed kind": build(runbook.Group("g", runbook.Ensure("a", noop()), runbook.Observe("b", noop()))),
			"reordered":    build(runbook.Group("g", runbook.Observe("b", noop()), runbook.Effect("a", noop()))),
			"added step":   build(runbook.Group("g", runbook.Effect("a", noop()), runbook.Observe("b", noop()), runbook.Effect("c", noop()))),
			"moved step":   build(runbook.Group("g", runbook.Effect("a", noop())), runbook.Observe("b", noop())),
		} {
			assert.NotEqual(t, base, other, name)
		}
	})
}

func TestDescribe(t *testing.T) {
	t.Parallel()

	plan, err := runbook.NewPlan("upgrade",
		runbook.Group("prechecks",
			runbook.Observe("ring", noop()),
			runbook.Observe("disk", noop()),
		).Parallel(8).ContinueOnError(),
		runbook.Effect("backup", reversible{}).Description("Snapshot the data directory"),
		runbook.Group("node1",
			runbook.Ensure("maintenance", checked{}),
			runbook.Effect("packages", noop()).Retry(3, time.Second),
		).Parallel(0).CancelOnError(),
	)
	require.NoError(t, err)

	assert.Equal(t, `upgrade
├── prechecks (parallel, limit 8, continue on error)
│   ├── ring (observe)
│   └── disk (observe)
├── backup (effect, check, undo) — Snapshot the data directory
└── node1 (parallel, cancel on error)
    ├── maintenance (ensure, check)
    └── packages (effect, 3 tries)
`, runbook.Describe(plan))
}

func TestBuilders_ReturnCopies(t *testing.T) {
	t.Parallel()

	labels := runbook.L{"dc": "DC1"}
	g := runbook.Group("g", runbook.Effect("a", noop())).Labels(labels)
	_ = g.Parallel(2).ContinueOnError().Description("changed")

	labels["dc"] = "mutated after the call"

	plan, err := runbook.NewPlan("p", g)
	require.NoError(t, err)

	spec := plan.Spec().Children[0]
	assert.False(t, spec.Parallel)
	assert.Equal(t, runbook.FailureStop, spec.Failure)
	assert.Empty(t, spec.Description)
	assert.Equal(t, runbook.L{"dc": "DC1"}, spec.Labels)
}

func TestStepFunc_Run(t *testing.T) {
	t.Parallel()

	called := false
	step := runbook.StepFunc(func(context.Context, *runbook.StepContext) error {
		called = true

		return nil
	})

	require.NoError(t, step.Run(t.Context(), &runbook.StepContext{}))
	assert.True(t, called)
}
