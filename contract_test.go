//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package pipeline_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/ruffel/pipeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests show how a tool keeps a build from carrying on from a journal
// that an incompatible build wrote, with what the package already has. The
// fingerprint can't do it: it is the plan's shape, and two builds can have
// the same paths and mean different things by what is recorded under them.
//
// The tool says what its record means in its inputs, which every attempt
// stores, and which a resume and a rollback must bring unchanged. Everything
// in this file but the pipeline package's own names is the tool's.

// contracts is the part of a tool's inputs that says what its journal means:
// an identifier for the operation, and one for each step whose record a
// later build could read differently, by path. They name what is compatible,
// not which build wrote them. A rebuilt tool keeps them, and changes one
// only when it changes what the record means: what a step records, or what
// its Check and Undo take for granted.
type contracts struct {
	Operation string            `json:"operation"`
	Steps     map[string]string `json:"steps,omitempty"`
	// Migration names the reviewed transition that let this attempt carry
	// on from other contracts, if one did.
	Migration string `json:"migration,omitempty"`
}

type toolInputs struct {
	Target    string    `json:"target"`
	Contracts contracts `json:"contracts"`
}

// legacyInputs is what a build from before the tool had contracts recorded.
type legacyInputs struct {
	Target string `json:"target"`
}

func v1() toolInputs {
	return toolInputs{Target: "8.2", Contracts: contracts{
		Operation: "upgrade/1",
		Steps:     map[string]string{"p/node1/install": "1"},
	}}
}

// v2 is a build whose install step records something else under the same
// path. The plan's shape is the same.
func v2(migration string) toolInputs {
	in := v1()
	in.Contracts.Steps = map[string]string{"p/node1/install": "2"}
	in.Contracts.Migration = migration

	return in
}

// contractError is the tool's own refusal. It names what differs.
type contractError struct {
	Intent  string
	Changed []string
}

func (e *contractError) Error() string {
	return fmt.Sprintf("%s refused: this build can't carry on from the journal's %s", e.Intent, strings.Join(e.Changed, ", "))
}

// transition is one change of contracts that the tool supports, and the
// name it is recorded under.
type transition struct {
	from, to contracts
}

// supporting returns the tool's validator: it lets an attempt carry on from
// other contracts only by a transition it knows, recorded under its name.
// intent is what the tool is about to do, which only the tool knows.
func supporting(intent string, known ...transition) pipeline.Option {
	return pipeline.WithInputChange(func(c pipeline.InputChange) error {
		var was, now toolInputs

		if err := json.Unmarshal(c.New, &now); err != nil {
			return err
		}

		// A journal from before the tool recorded any inputs has none, and
		// so no target to have changed.
		was.Target = now.Target

		if len(c.Old) > 0 {
			if err := json.Unmarshal(c.Old, &was); err != nil {
				return err
			}
		}

		for _, t := range known {
			if reflect.DeepEqual(was.Contracts, t.from) && reflect.DeepEqual(now.Contracts, t.to) && was.Target == now.Target {
				return nil
			}
		}

		return &contractError{Intent: intent, Changed: c.Changed}
	})
}

// probe counts every call a run or a rollback can make into a step.
type probe struct {
	c     *calls
	name  string
	fails *bool
}

func (p probe) Check(context.Context, *pipeline.StepContext) (bool, error) {
	p.c.add("check " + p.name)

	return false, nil
}

func (p probe) Run(context.Context, *pipeline.StepContext) error {
	p.c.add("run " + p.name)

	if p.fails != nil && *p.fails {
		return errors.New(p.name + " failed")
	}

	return nil
}

func (p probe) Undo(context.Context, *pipeline.StepContext) error {
	p.c.add("undo " + p.name)

	return nil
}

func (c *calls) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	n := 0
	for _, v := range c.n {
		n += v
	}

	return n
}

// nodePlan is one node's install and start, and a step after it that fails
// while fails is set. extra are more steps for the node.
func nodePlan(t *testing.T, c *calls, fails *bool, extra ...pipeline.Element) *pipeline.Plan {
	t.Helper()

	node := append([]pipeline.Element{
		pipeline.Effect("install", probe{c: c, name: "install"}),
		pipeline.Effect("start", probe{c: c, name: "start"}),
	}, extra...)

	return mustPlan(t,
		pipeline.Group("node1", node...),
		pipeline.Effect("finish", probe{c: c, name: "finish", fails: fails}),
	)
}

// journalWith opens a file journal and makes a first attempt on it, which
// fails at the last step.
func journalWith(t *testing.T, c *calls, opts ...pipeline.Option) (*pipeline.Journal, string) {
	t.Helper()

	fails := true
	path := journalPath(t)

	j, err := pipeline.OpenJournal(path)
	require.NoError(t, err)

	t.Cleanup(func() { _ = j.Close() })

	require.Error(t, resumeOn(t, j, nodePlan(t, c, &fails), opts...).err)

	return j, path
}

func fileBytes(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // A file in the test's own temp dir.
	require.NoError(t, err)

	return string(data)
}

// A build with the same paths and another contract is refused before it
// calls anything, whether it means to resume, to start over or to roll
// back, and the journal is left as it was. No option is needed for that:
// it is what a change of inputs does.
func TestContracts_AChangedContractIsRefusedBeforeAnythingIsCalled(t *testing.T) {
	t.Parallel()

	attempts := map[string]func(*testing.T, *pipeline.Journal, *pipeline.Plan, ...pipeline.Option) attemptResult{
		"run": resumeOn,
		"forced run": func(t *testing.T, j *pipeline.Journal, plan *pipeline.Plan, opts ...pipeline.Option) attemptResult {
			t.Helper()

			return resumeOn(t, j, plan, append(opts, pipeline.Force())...)
		},
		"rollback": resumeRollback,
	}

	for intent, attempt := range attempts {
		t.Run(intent, func(t *testing.T) {
			t.Parallel()

			var c calls

			j, path := journalWith(t, &c, pipeline.WithInputs(v1()))
			before, called := fileBytes(t, path), c.total()

			fails := false
			plan := nodePlan(t, &c, &fails)

			state, err := j.State()
			require.NoError(t, err)
			require.Equal(t, plan.Fingerprint(), state.Fingerprint, "the shape is the same")

			// With nothing said, the answer is no.
			out := attempt(t, j, plan, pipeline.WithInputs(v2("")))
			require.ErrorIs(t, out.err, pipeline.ErrInputsChanged)
			assert.Nil(t, out.res)

			// With the tool's validator, the refusal is the tool's, and says
			// which contract: a step's is named by its path.
			out = attempt(t, j, plan, pipeline.WithInputs(v2("")), supporting(intent))

			var refused *contractError

			require.ErrorAs(t, out.err, &refused)
			require.ErrorIs(t, out.err, pipeline.ErrInputsChanged)
			assert.Equal(t, intent, refused.Intent)
			assert.Equal(t, []string{"/contracts/steps/p~1node1~1install"}, refused.Changed)

			assert.Equal(t, called, c.total(), "no Check, Run or Undo is called")
			assert.Equal(t, before, fileBytes(t, path), "the journal is as it was")

			// The contracts it was written under still carry on.
			require.NoError(t, attempt(t, j, plan, pipeline.WithInputs(v1())).err)
		})
	}
}

// A transition the tool supports goes ahead, and the attempt's first event
// says what it carried on from, to what, and under which migration: before
// any step is called.
func TestContracts_ASupportedTransitionIsRecordedBeforeAnythingIsCalled(t *testing.T) {
	t.Parallel()

	var c calls

	j, _ := journalWith(t, &c, pipeline.WithInputs(v1()))

	fails := false
	to := v2("install-1-to-2")
	known := transition{from: v1().Contracts, to: to.Contracts}

	// The migration is part of what is approved: the same contracts without
	// its name are not the transition the tool knows.
	out := resumeOn(t, j, nodePlan(t, &c, &fails), pipeline.WithInputs(v2("")), supporting("run", known))
	require.ErrorIs(t, out.err, pipeline.ErrInputsChanged)

	out = resumeOn(t, j, nodePlan(t, &c, &fails), pipeline.WithInputs(to), supporting("run", known))
	require.NoError(t, out.err)

	started := out.events[0]
	require.Equal(t, pipeline.EventRunStarted, started.Kind)
	assert.Equal(t, []string{"/contracts/migration", "/contracts/steps/p~1node1~1install"}, started.InputsChanged)
	assert.JSONEq(t, `{"target":"8.2","contracts":{"operation":"upgrade/1","steps":{"p/node1/install":"2"},
		"migration":"install-1-to-2"}}`, string(started.Inputs))

	// What it carried on from is still in the journal, in the attempt
	// before: nothing is rewritten.
	all, err := j.Events()
	require.NoError(t, err)
	assert.JSONEq(t, `{"target":"8.2","contracts":{"operation":"upgrade/1","steps":{"p/node1/install":"1"}}}`,
		string(all[0].Inputs))

	state, err := j.State()
	require.NoError(t, err)
	assert.Equal(t, started.InputsChanged, state.Attempts[1].InputsChanged)

	var recorded toolInputs

	found, err := state.DecodeInputs(&recorded)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, to, recorded)
}

// A journal from before the tool had contracts has none, and none is made
// up for it: a build that has them is refused until the tool says, for that
// journal, that it may carry on.
func TestContracts_AJournalWithoutThemIsRefusedUntilTheToolApprovesIt(t *testing.T) {
	t.Parallel()

	for name, legacy := range map[string][]pipeline.Option{
		"inputs with no contracts": {pipeline.WithInputs(legacyInputs{Target: "8.2"})},
		"no inputs at all":         nil,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var c calls

			j, path := journalWith(t, &c, legacy...)
			before := fileBytes(t, path)

			fails := false
			plan := nodePlan(t, &c, &fails)

			require.ErrorIs(t, resumeOn(t, j, plan, pipeline.WithInputs(v1())).err, pipeline.ErrInputsChanged)
			require.ErrorIs(t, resumeOn(t, j, plan, pipeline.WithInputs(v1()), supporting("run")).err,
				pipeline.ErrInputsChanged)
			assert.Equal(t, before, fileBytes(t, path))

			// The tool adopts a journal it has looked at, under a name.
			adopted := v1()
			adopted.Contracts.Migration = "adopt-legacy"

			out := resumeOn(t, j, plan, pipeline.WithInputs(adopted),
				supporting("run", transition{from: contracts{}, to: adopted.Contracts}))
			require.NoError(t, out.err)
			assert.Contains(t, string(out.events[0].Inputs), `"migration":"adopt-legacy"`)
		})
	}
}

// It works the other way too: a build from before the tool had contracts
// brings none, and is refused a journal that has them. What gets it through
// is WithNewInputs, which accepts any change: a tool that needs this must
// not give that option to a resume.
func TestContracts_AnOlderBuildIsRefusedAJournalThatHasThem(t *testing.T) {
	t.Parallel()

	var c calls

	j, path := journalWith(t, &c, pipeline.WithInputs(v1()))
	before, called := fileBytes(t, path), c.total()

	fails := false
	plan := nodePlan(t, &c, &fails)

	require.ErrorIs(t, resumeOn(t, j, plan, pipeline.WithInputs(legacyInputs{Target: "8.2"})).err, pipeline.ErrInputsChanged)
	require.ErrorIs(t, resumeOn(t, j, plan).err, pipeline.ErrInputsChanged)
	require.ErrorIs(t, resumeRollback(t, j, plan).err, pipeline.ErrInputsChanged)

	assert.Equal(t, called, c.total())
	assert.Equal(t, before, fileBytes(t, path))

	require.NoError(t, resumeOn(t, j, plan, pipeline.WithInputs(legacyInputs{Target: "8.2"}), pipeline.WithNewInputs()).err)
}

// The tool's approval is about its contracts. It does not outrank what the
// journal itself refuses: a rollback past an irreversible step, a step with
// no Undo under WithStrictUndo, or a path whose kind has changed.
func TestContracts_ApprovalDoesNotOverrideTheJournalsOwnRefusals(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		irreversible bool
		attempt      func(*testing.T, *pipeline.Journal, *pipeline.Plan, ...pipeline.Option) attemptResult
		next         func(string, pipeline.Step) pipeline.StepNode
		opts         []pipeline.Option
		want         error
		asked        int
	}{
		"an irreversible step": {
			irreversible: true, attempt: resumeRollback, next: pipeline.Effect,
			want: pipeline.ErrIrreversible, asked: 1,
		},
		"a step with no Undo": {
			attempt: resumeRollback, next: pipeline.Effect, opts: []pipeline.Option{pipeline.WithStrictUndo()},
			want: pipeline.ErrNoUndo, asked: 1,
		},
		// A changed kind is refused before the tool is asked at all.
		"a changed kind": {
			attempt: resumeOn, next: pipeline.Ensure, opts: []pipeline.Option{matchByPath()},
			want: pipeline.ErrPlanChanged, asked: 0,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var c calls

			asked := 0
			approves := pipeline.WithInputChange(func(pipeline.InputChange) error {
				asked++

				return nil
			})

			fails := true
			plan := func(kind func(string, pipeline.Step) pipeline.StepNode) *pipeline.Plan {
				plain := pipeline.Effect("plain", counted(&c, "run plain", nil))
				if tc.irreversible {
					plain = plain.Irreversible()
				}

				return mustPlan(t,
					pipeline.Effect("backup", probe{c: &c, name: "backup"}),
					plain,
					kind("setting", counted(&c, "run setting", nil)),
					pipeline.Effect("finish", probe{c: &c, name: "finish", fails: &fails}),
				)
			}

			path := journalPath(t)

			j, err := pipeline.OpenJournal(path)
			require.NoError(t, err)

			t.Cleanup(func() { _ = j.Close() })

			require.Error(t, resumeOn(t, j, plan(pipeline.Effect), pipeline.WithInputs(v1())).err)

			before, called := fileBytes(t, path), c.total()

			opts := append([]pipeline.Option{pipeline.WithInputs(v2("install-1-to-2")), approves}, tc.opts...)

			out := tc.attempt(t, j, plan(tc.next), opts...)
			require.ErrorIs(t, out.err, tc.want)
			assert.Equal(t, tc.asked, asked)
			assert.Equal(t, called, c.total(), "nothing is called")
			assert.Equal(t, before, fileBytes(t, path), "and nothing is written")
		})
	}
}

// The shape of the plan and what its record means are two questions.
// WithPlanChange answers the first, and only that: a reordered plan has the
// same paths, so it is let through. A tool for which the order means
// something says so with its contract, and is then refused whatever
// WithPlanChange allows.
func TestContracts_ShapeAndContractAreDecidedSeparately(t *testing.T) {
	t.Parallel()

	var c calls

	j, path := journalWith(t, &c, pipeline.WithInputs(v1()))
	before := fileBytes(t, path)

	fails := false
	reordered := mustPlan(t,
		pipeline.Group("node1",
			pipeline.Effect("start", probe{c: &c, name: "start"}),
			pipeline.Effect("install", probe{c: &c, name: "install"}),
		),
		pipeline.Effect("finish", probe{c: &c, name: "finish", fails: &fails}),
	)

	require.ErrorIs(t, resumeOn(t, j, reordered, pipeline.WithInputs(v1())).err, pipeline.ErrPlanChanged)

	next := v1()
	next.Contracts.Operation = "upgrade/2"

	// The shape is asked about first. Until WithPlanChange accepts it, the
	// refusal is about the shape, and the validator is never asked.
	asked := false
	validator := pipeline.WithInputChange(func(pipeline.InputChange) error {
		asked = true

		return nil
	})

	out := resumeOn(t, j, reordered, pipeline.WithInputs(next), validator)
	require.ErrorIs(t, out.err, pipeline.ErrPlanChanged)
	require.NotErrorIs(t, out.err, pipeline.ErrInputsChanged)
	assert.False(t, asked)

	out = resumeOn(t, j, reordered, pipeline.WithInputs(next), matchByPath())
	require.ErrorIs(t, out.err, pipeline.ErrInputsChanged)
	assert.Equal(t, before, fileBytes(t, path))

	// With the contract it had, the shape alone is WithPlanChange's to allow.
	require.NoError(t, resumeOn(t, j, reordered, pipeline.WithInputs(v1()), matchByPath()).err)
}

// within returns what a group has in it: its children, and its failure
// handler, which is as much a part of it.
func within(n pipeline.NodeSpec) []pipeline.NodeSpec {
	if n.OnFailure == nil {
		return n.Children
	}

	return append(append([]pipeline.NodeSpec(nil), n.Children...), *n.OnFailure)
}

// addedToSettled returns the paths that next adds inside a group that prior
// has as done. It is a check a tool makes itself, from the journal's state
// and its plan's spec.
func addedToSettled(prior *pipeline.State, next pipeline.NodeSpec) []string {
	had := map[string]bool{}

	var index func(n pipeline.NodeSpec)

	index = func(n pipeline.NodeSpec) {
		had[n.Path] = true

		for _, inner := range within(n) {
			index(inner)
		}
	}

	if prior.Plan != nil {
		index(*prior.Plan)
	}

	var (
		added []string
		walk  func(n pipeline.NodeSpec, settled bool)
	)

	walk = func(n pipeline.NodeSpec, settled bool) {
		if !had[n.Path] && settled {
			added = append(added, n.Path)
		}

		for _, inner := range within(n) {
			walk(inner, settled || prior.Element(n.Path).Done && n.Kind == pipeline.KindGroup && had[n.Path])
		}
	}

	walk(next, false)

	return added
}

// The validator is asked only when the inputs differ. A check that must be
// made on every attempt is the tool's to make before it calls Run, from the
// journal it holds open: OpenJournal's lock keeps every other writer out, so
// nothing is written between the check and the attempt.
//
// The example is a step added to a group that had finished. WithPlanChange
// enters such a group again: the new step runs, and so do the group's Ensure
// steps. A tool that wants its finished groups left alone refuses that here.
func TestContracts_ACheckOnEveryAttemptIsMadeFromTheOpenJournal(t *testing.T) {
	t.Parallel()

	var c calls

	j, path := journalWith(t, &c, pipeline.WithInputs(v1()))
	before, called := fileBytes(t, path), c.total()

	fails := false
	hotfix := nodePlan(t, &c, &fails, pipeline.Effect("repair", probe{c: &c, name: "repair"}))

	// The journal as it stood, read with the lock held.
	prior, err := j.State()
	require.NoError(t, err)
	require.True(t, prior.Element("p/node1").Done)

	_, err = pipeline.OpenJournal(path)
	require.ErrorIs(t, err, pipeline.ErrJournalBusy, "no other process can write to it meanwhile")

	assert.Equal(t, []string{"p/node1/repair"}, addedToSettled(prior, hotfix.Spec()))
	assert.Empty(t, addedToSettled(prior, nodePlan(t, &c, &fails).Spec()))

	// A failure handler given to the group is an addition like any other:
	// the library enters the group again for it too.
	handled := mustPlan(t,
		pipeline.Group("node1",
			pipeline.Effect("install", probe{c: &c, name: "install"}),
			pipeline.Effect("start", probe{c: &c, name: "start"}),
		).OnFailure(pipeline.Effect("cleanup", probe{c: &c, name: "cleanup"})),
		pipeline.Effect("finish", probe{c: &c, name: "finish", fails: &fails}),
	)
	assert.Equal(t, []string{"p/node1/cleanup"}, addedToSettled(prior, handled.Spec()))

	assert.Equal(t, called, c.total())
	assert.Equal(t, before, fileBytes(t, path))

	// The validator can't make this check: by the time it is asked, the
	// state it is given has the change of plan applied, and the group is
	// no longer done.
	var seen *pipeline.State

	next := v1()
	next.Contracts.Operation = "upgrade/2"

	out := resumeOn(t, j, hotfix, pipeline.WithInputs(next), matchByPath(),
		pipeline.WithInputChange(func(change pipeline.InputChange) error {
			seen = change.State

			return errors.New("not this time")
		}))
	require.ErrorIs(t, out.err, pipeline.ErrInputsChanged)
	require.NotNil(t, seen)
	assert.False(t, seen.Element("p/node1").Done)
	assert.Equal(t, before, fileBytes(t, path))

	// Left to itself, the library enters the group again for the new step.
	require.NoError(t, resumeOn(t, j, hotfix, pipeline.WithInputs(v1()), matchByPath()).err)
	assert.Equal(t, 1, c.get("run repair"))
	assert.Equal(t, 2, c.get("check install"), "a step of the group with a Check has it called again")
	assert.Equal(t, 2, c.get("run install"), "and its Run, if the Check doesn't find the work done")
}
