package pipeline_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ruffel/pipeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// noop is a step with Run only.
func noop() pipeline.Step {
	return pipeline.StepFunc(func(context.Context, *pipeline.StepContext) error { return nil })
}

// checked is a step with Check.
type checked struct{}

func (checked) Run(context.Context, *pipeline.StepContext) error           { return nil }
func (checked) Check(context.Context, *pipeline.StepContext) (bool, error) { return false, nil }

// pointerStep has a pointer receiver, so a nil *pointerStep is a nil Step value.
type pointerStep struct{}

func (*pointerStep) Run(context.Context, *pipeline.StepContext) error { return nil }

// reversible is a step with Check and Undo.
type reversible struct{ checked }

func (reversible) Undo(context.Context, *pipeline.StepContext) error { return nil }

func TestNewPlan_PathsAndInheritedLabels(t *testing.T) {
	t.Parallel()

	plan, err := pipeline.NewPlan("upgrade",
		pipeline.Group("region1",
			pipeline.Group("DC1",
				pipeline.Effect("packages", noop()).Labels(pipeline.L{"node": "n1", "dc": "override"}),
			).Labels(pipeline.L{"dc": "DC1"}),
		).Labels(pipeline.L{"region": "r1"}),
	)
	require.NoError(t, err)

	root := plan.Spec()
	assert.Equal(t, "upgrade", plan.Name())
	assert.Equal(t, "upgrade", root.Path)
	assert.Equal(t, pipeline.KindGroup, root.Kind)
	assert.Nil(t, root.Labels)

	region := root.Children[0]
	dc := region.Children[0]
	step := dc.Children[0]

	assert.Equal(t, "upgrade/region1", region.Path)
	assert.Equal(t, "upgrade/region1/DC1", dc.Path)
	assert.Equal(t, "upgrade/region1/DC1/packages", step.Path)

	assert.Equal(t, pipeline.L{"region": "r1"}, region.Labels)
	assert.Equal(t, pipeline.L{"region": "r1", "dc": "DC1"}, dc.Labels)
	assert.Equal(t, pipeline.L{"region": "r1", "dc": "override", "node": "n1"}, step.Labels, "own value wins")
}

func TestNewPlan_ReportsEveryProblemWithItsPath(t *testing.T) {
	t.Parallel()

	_, err := pipeline.NewPlan("p",
		pipeline.Effect("a", noop()),
		pipeline.Effect("a", noop()),
		pipeline.Effect("", noop()),
		pipeline.Effect("bad/name", noop()),
		pipeline.Effect("-leading", noop()),
		pipeline.Effect("nil-step", nil),
		pipeline.Effect("nil-func", pipeline.StepFunc(nil)),
		pipeline.Effect("nil-pointer", (*pointerStep)(nil)),
		pipeline.Observe("probe", checked{}),
		pipeline.Effect("retry", noop()).Retry(0, time.Second),
		pipeline.Effect("timeout", noop()).Timeout(-time.Second),
		pipeline.Group("g").Parallel(-1),
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

	_, err := pipeline.NewPlan("p",
		pipeline.Effect("core-packages_v1.2", noop()),
		pipeline.Effect("9lives", noop()),
		pipeline.Group("empty"),
	)
	require.NoError(t, err)

	_, err = pipeline.NewPlan("ünicode")
	require.Error(t, err, "names are ASCII so they are safe as file names")
}

func TestPlan_SpecCarriesSettingsAndCapabilities(t *testing.T) {
	t.Parallel()

	plan, err := pipeline.NewPlan("p",
		pipeline.Group("checks",
			pipeline.Observe("ring", noop()),
		).Parallel(8).ContinueOnError().Description("Read-only checks"),
		pipeline.Effect("backup", reversible{}).Retry(3, time.Second).Timeout(time.Minute),
		pipeline.Ensure("maintenance", checked{}),
		pipeline.Group("nodes").Parallel(0).CancelOnError(),
	)
	require.NoError(t, err)

	children := plan.Spec().Children

	assert.Equal(t, pipeline.NodeSpec{
		Name: "checks", Path: "p/checks", Kind: pipeline.KindGroup, Description: "Read-only checks",
		Parallel: true, Limit: 8, Failure: pipeline.FailureContinue,
		Children: []pipeline.NodeSpec{{Name: "ring", Path: "p/checks/ring", Kind: pipeline.KindObserve, Tries: 1}},
	}, children[0])
	assert.Equal(t, pipeline.NodeSpec{
		Name: "backup", Path: "p/backup", Kind: pipeline.KindEffect, Check: true, Undo: true,
		Tries: 3, Backoff: time.Second, Timeout: time.Minute,
	}, children[1])
	assert.Equal(t, pipeline.NodeSpec{
		Name: "maintenance", Path: "p/maintenance", Kind: pipeline.KindEnsure, Check: true, Tries: 1,
	}, children[2])
	assert.Equal(t, pipeline.FailureCancel, children[3].Failure)
	assert.True(t, children[3].Parallel)
	assert.Equal(t, pipeline.FailureStop, plan.Spec().Failure, "the root is a sequential group with the default policy")
}

func TestPlan_SpecRoundTripsThroughJSON(t *testing.T) {
	t.Parallel()

	plan, err := pipeline.NewPlan("p",
		pipeline.Group("g",
			pipeline.Effect("e", reversible{}).Retry(2, 500*time.Millisecond).Timeout(time.Second),
		).Parallel(2).Labels(pipeline.L{"k": "v"}),
	)
	require.NoError(t, err)

	raw, err := json.Marshal(plan.Spec())
	require.NoError(t, err)

	var back pipeline.NodeSpec
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

	build := func(elems ...pipeline.Element) string {
		t.Helper()

		plan, err := pipeline.NewPlan("p", elems...)
		require.NoError(t, err)

		return plan.Fingerprint()
	}

	base := build(pipeline.Group("g", pipeline.Effect("a", noop()), pipeline.Observe("b", noop())))

	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, base)

	t.Run("ignores wording and settings", func(t *testing.T) {
		t.Parallel()

		assert.Equal(t, base, build(pipeline.Group("g",
			pipeline.Effect("a", noop()).Description("new words").Retry(3, time.Second).Timeout(time.Minute),
			pipeline.Observe("b", noop()).Labels(pipeline.L{"x": "y"}),
		).Parallel(4).ContinueOnError().Description("group words")))
	})

	t.Run("changes with identity", func(t *testing.T) {
		t.Parallel()

		for name, other := range map[string]string{
			"renamed step": build(pipeline.Group("g", pipeline.Effect("a2", noop()), pipeline.Observe("b", noop()))),
			"changed kind": build(pipeline.Group("g", pipeline.Ensure("a", noop()), pipeline.Observe("b", noop()))),
			"reordered":    build(pipeline.Group("g", pipeline.Observe("b", noop()), pipeline.Effect("a", noop()))),
			"added step":   build(pipeline.Group("g", pipeline.Effect("a", noop()), pipeline.Observe("b", noop()), pipeline.Effect("c", noop()))),
			"moved step":   build(pipeline.Group("g", pipeline.Effect("a", noop())), pipeline.Observe("b", noop())),
		} {
			assert.NotEqual(t, base, other, name)
		}
	})
}

func TestDescribe(t *testing.T) {
	t.Parallel()

	plan, err := pipeline.NewPlan("upgrade",
		pipeline.Group("prechecks",
			pipeline.Observe("ring", noop()),
			pipeline.Observe("disk", noop()),
		).Parallel(8).ContinueOnError(),
		pipeline.Effect("backup", reversible{}).Description("Snapshot the data directory"),
		pipeline.Group("node1",
			pipeline.Ensure("maintenance", checked{}),
			pipeline.Effect("packages", noop()).Retry(3, time.Second),
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
`, pipeline.Describe(plan))
}

func TestBuilders_ReturnCopies(t *testing.T) {
	t.Parallel()

	labels := pipeline.L{"dc": "DC1"}
	g := pipeline.Group("g", pipeline.Effect("a", noop())).Labels(labels)
	_ = g.Parallel(2).ContinueOnError().Description("changed")

	labels["dc"] = "mutated after the call"

	plan, err := pipeline.NewPlan("p", g)
	require.NoError(t, err)

	spec := plan.Spec().Children[0]
	assert.False(t, spec.Parallel)
	assert.Equal(t, pipeline.FailureStop, spec.Failure)
	assert.Empty(t, spec.Description)
	assert.Equal(t, pipeline.L{"dc": "DC1"}, spec.Labels)
}

func TestStepFunc_Run(t *testing.T) {
	t.Parallel()

	called := false
	step := pipeline.StepFunc(func(context.Context, *pipeline.StepContext) error {
		called = true

		return nil
	})

	require.NoError(t, step.Run(t.Context(), &pipeline.StepContext{}))
	assert.True(t, called)
}
