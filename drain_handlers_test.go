package pipeline_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ruffel/pipeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rolling is a plan shaped like a rolling upgrade, for what becomes of a
// node that fails. The root has a cleanup step. Under it is a datacenter of
// two nodes, and a step after it. The first node is a recovery step around
// a step and a DrainOnStop group of three:
//
//	p
//	├── dc1
//	│   ├── node1
//	│   │   ├── prepare
//	│   │   ├── unit (finishes before a stop)
//	│   │   │   ├── stop
//	│   │   │   ├── install
//	│   │   │   └── start
//	│   │   └── rollback (recovers the group on failure)
//	│   └── node2
//	│       └── next
//	├── after
//	└── cleanup (cleans up on failure)
//
// Every step counts its runs under its name, then does what during has for
// it, and fails if failing says so.
type rolling struct {
	c       calls
	during  map[string]func()
	failing map[string]bool
	// shape changes the steps it names, and datacenter the datacenter.
	shape      map[string]func(pipeline.StepNode) pipeline.StepNode
	datacenter func(pipeline.GroupNode) pipeline.GroupNode
}

func (r *rolling) step(name string) pipeline.StepNode {
	step := bounded(pipeline.Effect(name, pipeline.StepFunc(func(context.Context, *pipeline.StepContext) error {
		r.c.add(name)

		if fn := r.during[name]; fn != nil {
			fn()
		}

		if r.failing[name] {
			return errors.New(name + " failed")
		}

		return nil
	})))

	if shape := r.shape[name]; shape != nil {
		step = shape(step)
	}

	return step
}

func (r *rolling) plan(t *testing.T) *pipeline.Plan {
	t.Helper()

	dc := pipeline.Group("dc1",
		pipeline.Group("node1",
			r.step("prepare"),
			pipeline.Group("unit", r.step("stop"), r.step("install"), r.step("start")).DrainOnStop(),
		).RecoverOnFailure(r.step("rollback")),
		pipeline.Group("node2", r.step("next")),
	)

	if r.datacenter != nil {
		dc = r.datacenter(dc)
	}

	plan, err := pipeline.NewPlanFromGroup(pipeline.Group("p", dc, r.step("after")).OnFailure(r.step("cleanup")))
	require.NoError(t, err)

	return plan
}

// said returns the messages in events, each after the path it is about.
func said(events []pipeline.Event) []string {
	messages := kinds(events, pipeline.EventMessage)
	out := make([]string, 0, len(messages))

	for _, e := range messages {
		out = append(out, e.Path+": "+e.Message)
	}

	return out
}

// A stop waits for a DrainOnStop group, and a group that then ends in
// failure is not left as it fell because a stop happened to be pending. The
// failure handlers of the groups around it run, as they would have with no
// stop, and the run has failed. A later attempt starts the node that its
// recovery reset from its first step.
func TestDrain_AFailureUnderAStopIsHandled(t *testing.T) {
	t.Parallel()

	for name, during := range map[string]string{
		"a stop before the step that fails": "stop",
		"a stop during the step that fails": "install",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			stopper := pipeline.NewStopper()
			r := &rolling{
				during:  map[string]func(){during: func() { stopper.Stop("alice") }},
				failing: map[string]bool{"install": true},
			}
			plan, j := r.plan(t), pipeline.MemoryJournal()

			out := resumeOn(t, j, plan, pipeline.WithStopper(stopper))

			require.ErrorContains(t, out.err, "install failed")
			require.NotErrorIs(t, out.err, pipeline.ErrStopped, "the failure ended the run, not the stop")
			assert.Equal(t, pipeline.OutcomeFailed, out.res.Outcome)
			assert.Zero(t, r.c.get("start"), "the failure still ends the group at once")
			assert.Zero(t, r.c.get("next"), "nothing else starts")
			assert.Zero(t, r.c.get("after"))

			steps := trace(out.events)
			require.Greater(t, len(steps), 11)
			assert.Equal(t, []string{
				"step.finished p/dc1/node1/unit/install failed",
				"group.finished p/dc1/node1/unit failed",
				"group.reset p/dc1/node1",
				"step.started p/dc1/node1/rollback",
				"step.running p/dc1/node1/rollback",
				"step.finished p/dc1/node1/rollback passed",
				"group.finished p/dc1/node1 failed",
				"group.finished p/dc1 failed",
				"step.started p/cleanup",
				"step.running p/cleanup",
				"step.finished p/cleanup passed",
			}, steps[len(steps)-11:])

			assert.Subset(t, said(out.events), []string{
				"p/dc1/node1: a stop was requested: this group's failure is handled first",
				"p: a stop was requested: this group's failure is handled first",
			}, "whoever is following is told why steps start after the stop")

			state, err := j.State()
			require.NoError(t, err)
			require.NotNil(t, state.Attempts[0].Stop, "the stop was asked for, and is recorded")
			assert.Empty(t, state.Draining())

			r.failing = nil
			out = resumeOn(t, j, plan)

			require.NoError(t, out.err)
			assert.Equal(t, 2, r.c.get("prepare"), "the node starts again from its first step")
			assert.Equal(t, 2, r.c.get("stop"))
			assert.Equal(t, 2, r.c.get("install"))
			assert.Equal(t, 1, r.c.get("start"))
			assert.Equal(t, 1, r.c.get("rollback"))
			assert.Equal(t, 1, r.c.get("cleanup"))
		})
	}
}

// A cancel still keeps every handler from starting. It waits for nothing,
// whether it is the run's context that is cancelled or a stop made a cancel.
func TestDrain_ACancelKeepsAFailureFromBeingHandled(t *testing.T) {
	t.Parallel()

	for name, cancelled := range map[string]func(context.CancelFunc, *pipeline.Stopper){
		"the context is cancelled":  func(cancel context.CancelFunc, _ *pipeline.Stopper) { cancel() },
		"the stop is made a cancel": func(_ context.CancelFunc, s *pipeline.Stopper) { s.Cancel("alice") },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			stopper := pipeline.NewStopper()
			r := &rolling{
				during: map[string]func(){
					"stop":    func() { stopper.Stop("alice") },
					"install": func() { cancelled(cancel, stopper) },
				},
				failing: map[string]bool{"install": true},
			}

			res, err := pipeline.Run(ctx, r.plan(t), pipeline.MemoryJournal(), pipeline.WithStopper(stopper))

			require.ErrorContains(t, err, "install failed")
			require.ErrorIs(t, err, pipeline.ErrStopped)
			assert.Equal(t, pipeline.OutcomeCancelled, res.Outcome)
			assert.Zero(t, r.c.get("rollback"))
			assert.Zero(t, r.c.get("cleanup"))
		})
	}
}

// Only the failure of a DrainOnStop group is handled under a stop. A step
// elsewhere in the groups around one, failing once a stop has been
// requested, is handled as in any group: not at all, and the run is stopped.
func TestDrain_AFailureOutsideTheGroupIsNotHandledUnderAStop(t *testing.T) {
	t.Parallel()

	stopper := pipeline.NewStopper()
	r := &rolling{
		during:  map[string]func(){"prepare": func() { stopper.Stop("alice") }},
		failing: map[string]bool{"prepare": true},
	}

	out := resumeOn(t, pipeline.MemoryJournal(), r.plan(t), pipeline.WithStopper(stopper))

	require.ErrorContains(t, out.err, "prepare failed")
	require.ErrorIs(t, out.err, pipeline.ErrStopped)
	assert.Equal(t, pipeline.OutcomeStopped, out.res.Outcome)
	assert.Zero(t, r.c.get("stop"), "the group never started")
	assert.Zero(t, r.c.get("rollback"))
	assert.Zero(t, r.c.get("cleanup"))
	assert.Empty(t, kinds(out.events, pipeline.EventGroupReset))
}

// A stop that comes while a failure is being handled lets the handler
// finish, as it does any running step, and the handlers further out still
// run.
func TestDrain_AStopWhileAFailureIsHandled(t *testing.T) {
	t.Parallel()

	stopper := pipeline.NewStopper()
	r := &rolling{
		during:  map[string]func(){"rollback": func() { stopper.Stop("alice") }},
		failing: map[string]bool{"install": true},
	}

	out := resumeOn(t, pipeline.MemoryJournal(), r.plan(t), pipeline.WithStopper(stopper))

	require.ErrorContains(t, out.err, "install failed")
	require.NotErrorIs(t, out.err, pipeline.ErrStopped)
	assert.Equal(t, pipeline.OutcomeFailed, out.res.Outcome)
	assert.Contains(t, trace(out.events), "step.finished p/dc1/node1/rollback passed")
	assert.Equal(t, 1, r.c.get("cleanup"))
	assert.Zero(t, r.c.get("next"))
	assert.Zero(t, r.c.get("after"))
}

// Under a stop a failure is handled once, not worked at, as the group itself
// is finished and not worked at: a handler that fails is not tried again,
// and it is its failure that the run ends with.
func TestDrain_AHandlerIsNotTriedAgainUnderAStop(t *testing.T) {
	t.Parallel()

	stopper := pipeline.NewStopper()
	r := &rolling{
		during:  map[string]func(){"stop": func() { stopper.Stop("alice") }},
		failing: map[string]bool{"install": true, "rollback": true},
		shape: map[string]func(pipeline.StepNode) pipeline.StepNode{
			"rollback": func(s pipeline.StepNode) pipeline.StepNode { return s.Retry(3, time.Hour) },
		},
	}

	out := resumeOn(t, pipeline.MemoryJournal(), r.plan(t), pipeline.WithStopper(stopper))

	require.ErrorContains(t, out.err, "install failed")
	require.ErrorContains(t, out.err, "rollback failed")
	require.NotErrorIs(t, out.err, pipeline.ErrStopped)
	assert.Equal(t, pipeline.OutcomeFailed, out.res.Outcome)
	assert.Equal(t, 1, r.c.get("rollback"))
	assert.Empty(t, kinds(out.events, pipeline.EventStepRetry))
	assert.Equal(t, 1, r.c.get("cleanup"), "a handler further out runs whatever became of the one before")
}

// When the stop comes makes no difference. One that arrives while a step of
// the group, or a handler of its failure, is waiting to be tried again ends
// the wait, and what follows is what follows a failure under a stop already
// pending: the failure is handled, and the run has failed.
func TestDrain_AStopBetweenTriesIsHandledTheSame(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		waits   string
		failing map[string]bool
	}{
		"a step of the group waits": {waits: "install", failing: map[string]bool{"install": true}},
		"a handler waits":           {waits: "rollback", failing: map[string]bool{"install": true, "rollback": true}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			stopper := pipeline.NewStopper()
			retrying := pipeline.ObserverFunc(func(_ context.Context, e pipeline.Event) {
				if e.Kind == pipeline.EventStepRetry {
					stopper.Stop("alice")
				}
			})

			r := &rolling{
				failing: tc.failing,
				shape: map[string]func(pipeline.StepNode) pipeline.StepNode{
					tc.waits: func(s pipeline.StepNode) pipeline.StepNode { return s.Retry(3, time.Hour) },
				},
			}

			out := resumeOn(t, pipeline.MemoryJournal(), r.plan(t), pipeline.WithStopper(stopper), pipeline.WithObservers(retrying))

			require.ErrorContains(t, out.err, "install failed")
			require.NotErrorIs(t, out.err, pipeline.ErrStopped)
			assert.Equal(t, pipeline.OutcomeFailed, out.res.Outcome)
			assert.Len(t, kinds(out.events, pipeline.EventStepRetry), 1, "the wait the stop ended was the first")
			assert.Equal(t, 1, r.c.get(tc.waits), "it is not tried again")
			assert.Equal(t, 1, r.c.get("rollback"))
			assert.Equal(t, 1, r.c.get("cleanup"))
			assert.Zero(t, r.c.get("next"))
		})
	}
}

// The stop still holds for whatever would come next. In a group that
// carries on past failures, it keeps the next child from starting, and the
// run is stopped, with the failure in its error.
func TestDrain_AfterAHandledFailureTheStopStillHolds(t *testing.T) {
	t.Parallel()

	stopper := pipeline.NewStopper()
	r := &rolling{
		during:     map[string]func(){"stop": func() { stopper.Stop("alice") }},
		failing:    map[string]bool{"install": true},
		datacenter: func(g pipeline.GroupNode) pipeline.GroupNode { return g.ContinueOnError() },
	}

	out := resumeOn(t, pipeline.MemoryJournal(), r.plan(t), pipeline.WithStopper(stopper))

	require.ErrorContains(t, out.err, "install failed")
	require.ErrorIs(t, out.err, pipeline.ErrStopped)
	assert.Equal(t, pipeline.OutcomeStopped, out.res.Outcome)
	assert.Equal(t, 1, r.c.get("rollback"))
	assert.Zero(t, r.c.get("next"), "the stop keeps the next node from starting")
	assert.Equal(t, 1, r.c.get("cleanup"))
}

// A stop waits for the failure handlers of the groups around a DrainOnStop
// group, so each needs a timeout, however far out it is.
func TestNewPlan_AHandlerAroundADrainOnStopGroup(t *testing.T) {
	t.Parallel()

	unit := pipeline.Group("unit", bounded(pipeline.Effect("a", ok()))).DrainOnStop()
	handler := pipeline.Effect("handler", ok())

	const want = "handler: the failure handler of a group around a DrainOnStop group needs a Timeout"

	for name, root := range map[string]pipeline.GroupNode{
		"recovery around it":  pipeline.Group("p", pipeline.Group("node", unit).RecoverOnFailure(handler)),
		"cleanup around it":   pipeline.Group("p", pipeline.Group("node", unit).OnFailure(handler)),
		"cleanup on the root": pipeline.Group("p", pipeline.Group("dc", pipeline.Group("node", unit))).OnFailure(handler),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := pipeline.NewPlanFromGroup(root)
			require.ErrorContains(t, err, want)
		})
	}

	// One with a timeout is accepted, and so is one with none on a group
	// that has no DrainOnStop group beneath it.
	_, err := pipeline.NewPlanFromGroup(pipeline.Group("p",
		pipeline.Group("node", unit).RecoverOnFailure(bounded(handler)),
		pipeline.Group("other", pipeline.Effect("a", ok())).OnFailure(handler),
	).OnFailure(bounded(pipeline.Effect("cleanup", ok()))))
	require.NoError(t, err)
}
