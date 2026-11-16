package pipeline_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ruffel/pipeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type hostInput struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

type installInputs struct {
	Version  string            `json:"version"`
	Hosts    []hostInput       `json:"hosts"`
	Options  map[string]string `json:"options,omitempty"`
	Password pipeline.Secret   `json:"password"`
}

func twoHosts() installInputs {
	return installInputs{
		Version: "8.2",
		Hosts:   []hostInput{{"node1", "10.0.0.1"}, {"node2", "10.0.0.2"}},
	}
}

// installPlan installs node1, then node2, which fails while fails is set,
// then finishes.
func installPlan(t *testing.T, c *calls, fails *bool) *pipeline.Plan {
	t.Helper()

	return mustPlan(t,
		effect(c, "prepare", nil),
		pipeline.Group("node1", effect(c, "install1", nil)),
		pipeline.Group("node2", effect(c, "check2", fails), effect(c, "install2", nil)),
		effect(c, "finish", nil),
	)
}

func installConsumers() pipeline.Option {
	return pipeline.WithInputConsumers(map[string][]string{
		"/hosts/0": {"p/node1"},
		"/hosts/1": {"p/node2/install2", "p/finish"},
		"/options": {"p/finish"},
	})
}

// The usual fix after a failed run: correct an input nothing has used yet,
// and run again.
func TestInputChange_AcceptedWhenNoConsumerHasStarted(t *testing.T) {
	t.Parallel()

	var c calls

	fails := true
	plan := installPlan(t, &c, &fails)
	j := pipeline.MemoryJournal()

	in := twoHosts()
	require.Error(t, resumeOn(t, j, plan, pipeline.WithInputs(in)).err)

	in.Hosts[1].Address = "10.0.0.22"
	in.Options = map[string]string{"tls": "on"}
	in.Password = pipeline.NewSecret("hunter2-hunter2")

	require.ErrorIs(t, resumeOn(t, j, plan, pipeline.WithInputs(in)).err, pipeline.ErrInputsChanged,
		"without the option any change is refused, as before")

	fails = false
	out := resumeOn(t, j, plan, pipeline.WithInputs(in), installConsumers())
	require.NoError(t, out.err)

	want := []string{"/hosts/1/address", "/options"}
	assert.Equal(t, want, out.events[0].InputsChanged, "recorded on run.started, as pointers")

	state, err := j.State()
	require.NoError(t, err)
	assert.Equal(t, want, state.Attempts[1].InputsChanged)
	assert.Empty(t, state.Attempts[0].InputsChanged)

	var recorded installInputs

	found, err := state.DecodeInputs(&recorded)
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "10.0.0.22", recorded.Hosts[1].Address)
}

// A field whose consumer has run is refused, and the error says which
// field, what it was and is, and what used it.
func TestInputChange_RefusedWhenAConsumerHasStarted(t *testing.T) {
	t.Parallel()

	var c calls

	fails := true
	plan := installPlan(t, &c, &fails)
	j := pipeline.MemoryJournal()

	in := twoHosts()
	require.Error(t, resumeOn(t, j, plan, pipeline.WithInputs(in)).err)

	in.Hosts[0].Address = "10.0.0.11"
	in.Hosts[1].Address = "10.0.0.22"

	out := resumeOn(t, j, plan, pipeline.WithInputs(in), installConsumers())
	require.ErrorIs(t, out.err, pipeline.ErrInputsChanged)
	assert.Nil(t, out.res)
	assert.Empty(t, out.events, "nothing is written")

	var refused *pipeline.InputChangeError

	require.ErrorAs(t, out.err, &refused)
	require.Len(t, refused.Fields, 1, "the field nothing has used is not listed")
	assert.Equal(t, pipeline.InputFieldChange{
		Pointer: "/hosts/0/address", Old: []byte(`"10.0.0.1"`), New: []byte(`"10.0.0.11"`), Consumer: "p/node1",
	}, refused.Fields[0])
	require.ErrorContains(t, out.err,
		`/hosts/0/address changed from "10.0.0.1" to "10.0.0.11", and p/node1 has already used it`)
}

// A field with no consumer declared counts as used by every step.
func TestInputChange_AnUndeclaredFieldIsRefused(t *testing.T) {
	t.Parallel()

	var c calls

	fails := true
	plan := installPlan(t, &c, &fails)
	j := pipeline.MemoryJournal()

	in := twoHosts()
	require.Error(t, resumeOn(t, j, plan, pipeline.WithInputs(in)).err)

	in.Version = "8.3"
	in.Hosts = append(in.Hosts, hostInput{"node3", "10.0.0.3"})

	out := resumeOn(t, j, plan, pipeline.WithInputs(in), installConsumers())

	var refused *pipeline.InputChangeError

	require.ErrorAs(t, out.err, &refused)
	require.Len(t, refused.Fields, 2)
	assert.Equal(t, "/hosts/2", refused.Fields[0].Pointer)
	assert.Nil(t, refused.Fields[0].Old, "it was not there")
	assert.JSONEq(t, `{"name":"node3","address":"10.0.0.3"}`, string(refused.Fields[0].New))
	assert.Empty(t, refused.Fields[0].Consumer)
	assert.Equal(t, "/version", refused.Fields[1].Pointer)
	require.ErrorContains(t, out.err, `/version changed from "8.2" to "8.3", and no consumer is declared for it`)
}

// A group has started once anything beneath it has, and a failed or
// interrupted step has started as much as a passed one.
func TestInputChange_AGroupConsumerHasStartedWhenAnythingInItHas(t *testing.T) {
	t.Parallel()

	var c calls

	fails := true
	plan := installPlan(t, &c, &fails)
	j := pipeline.MemoryJournal()

	in := twoHosts()
	require.Error(t, resumeOn(t, j, plan, pipeline.WithInputs(in)).err)

	in.Hosts[1].Address = "10.0.0.22"

	byGroup := pipeline.WithInputConsumers(map[string][]string{"/hosts": {"p/node2"}})
	out := resumeOn(t, j, plan, pipeline.WithInputs(in), byGroup)

	var refused *pipeline.InputChangeError

	require.ErrorAs(t, out.err, &refused, "check2 failed, so node2 has started")
	assert.Equal(t, "p/node2", refused.Fields[0].Consumer)

	// The pointer "" covers every field.
	all := pipeline.WithInputConsumers(map[string][]string{"": {"p/finish"}})
	fails = false

	require.NoError(t, resumeOn(t, j, plan, pipeline.WithInputs(in), all).err)
}

// A step that a rollback undid has not used its inputs any more.
func TestInputChange_AnUndoneConsumerHasNotStarted(t *testing.T) {
	t.Parallel()

	var c calls

	fails := true
	plan := installPlan(t, &c, &fails)
	j := pipeline.MemoryJournal()

	in := twoHosts()
	require.Error(t, resumeOn(t, j, plan, pipeline.WithInputs(in)).err)
	require.NoError(t, resumeRollback(t, j, plan, pipeline.WithInputs(in)).err)

	in.Hosts[0].Address = "10.0.0.11"
	fails = false

	require.NoError(t, resumeOn(t, j, plan, pipeline.WithInputs(in), installConsumers()).err)
}

func TestInputChange_AConsumerThePlanLacksIsAnError(t *testing.T) {
	t.Parallel()

	var c calls

	fails := true
	plan := installPlan(t, &c, &fails)
	j := pipeline.MemoryJournal()

	in := twoHosts()
	require.Error(t, resumeOn(t, j, plan, pipeline.WithInputs(in)).err)

	in.Version = "8.3"
	out := resumeOn(t, j, plan, pipeline.WithInputs(in),
		pipeline.WithInputConsumers(map[string][]string{"/version": {"p/nowhere"}}))
	require.ErrorContains(t, out.err, "input consumer p/nowhere, of /version, is not in the plan")
}

// WithInputChange is the rule in the consumer's own hands.
func TestInputChange_TheHookDecides(t *testing.T) {
	t.Parallel()

	var c calls

	fails := true
	plan := installPlan(t, &c, &fails)
	j := pipeline.MemoryJournal()

	in := twoHosts()
	require.Error(t, resumeOn(t, j, plan, pipeline.WithInputs(in)).err)

	in.Version = "8.3"
	in.Options = map[string]string{"a/b": "x", "c~d": "y"}

	var seen pipeline.InputChange

	errNo := errors.New("the version is fixed once prepare has run")
	hook := func(accept bool) pipeline.Option {
		return pipeline.WithInputChange(func(change pipeline.InputChange) error {
			seen = change

			if accept {
				return nil
			}

			return errNo
		})
	}

	out := resumeOn(t, j, plan, pipeline.WithInputs(in), hook(false))
	require.ErrorIs(t, out.err, pipeline.ErrInputsChanged)
	require.ErrorIs(t, out.err, errNo)
	assert.Empty(t, out.events)

	assert.Equal(t, []string{"/options", "/version"}, seen.Changed, "a map that was empty is one field")
	assert.JSONEq(t, `{"version":"8.2","hosts":[{"name":"node1","address":"10.0.0.1"},{"name":"node2","address":"10.0.0.2"}],"password":null}`,
		string(seen.Old))
	assert.Contains(t, string(seen.New), `"version":"8.3"`)
	assert.Equal(t, pipeline.StatusPassed, seen.State.Element("p/prepare").Status)

	// Every hook must accept, and the declared consumers too.
	out = resumeOn(t, j, plan, pipeline.WithInputs(in), hook(true), hook(false))
	require.ErrorIs(t, out.err, errNo)

	out = resumeOn(t, j, plan, pipeline.WithInputs(in), hook(true), installConsumers())
	require.ErrorIs(t, out.err, pipeline.ErrInputsChanged)

	fails = false
	out = resumeOn(t, j, plan, pipeline.WithInputs(in), hook(true))
	require.NoError(t, out.err)
	assert.Equal(t, []string{"/options", "/version"}, out.events[0].InputsChanged)

	// Keys are escaped as a JSON pointer's are.
	in.Options["a/b"] = "z"
	out = resumeOn(t, j, plan, pipeline.WithInputs(in), pipeline.Force(), hook(true))
	require.NoError(t, out.err)
	assert.Equal(t, []string{"/options/a~1b"}, seen.Changed)
}

// A secret is never part of the comparison, and WithNewInputs still accepts
// anything, now saying what changed.
func TestInputChange_SecretsAndNewInputs(t *testing.T) {
	t.Parallel()

	var c calls

	fails := true
	plan := installPlan(t, &c, &fails)
	j := pipeline.MemoryJournal()

	in := twoHosts()
	in.Password = pipeline.NewSecret("first-password")
	require.Error(t, resumeOn(t, j, plan, pipeline.WithInputs(in)).err)

	in.Password = pipeline.NewSecret("second-password")
	called := false
	hook := pipeline.WithInputChange(func(pipeline.InputChange) error {
		called = true

		return nil
	})

	require.Error(t, resumeOn(t, j, plan, pipeline.WithInputs(in), hook).err)
	assert.False(t, called, "only a secret differs, so nothing has changed")

	in.Version = "8.3"
	fails = false
	out := resumeOn(t, j, plan, pipeline.WithInputs(in), pipeline.WithNewInputs())
	require.NoError(t, out.err)
	assert.Equal(t, []string{"/version"}, out.events[0].InputsChanged)
}

// Removing a whole list changes every field in it, so a consumer declared
// for one of its fields counts, though the pointer that changed is above it.
func TestInputChange_AContainerChangeAsksTheConsumersBeneathIt(t *testing.T) {
	t.Parallel()

	var c calls

	fails := true
	plan := installPlan(t, &c, &fails)
	j := pipeline.MemoryJournal()

	in := twoHosts()
	require.Error(t, resumeOn(t, j, plan, pipeline.WithInputs(in)).err)

	in.Hosts = nil
	out := resumeOn(t, j, plan, pipeline.WithInputs(in), pipeline.WithInputConsumers(map[string][]string{
		"":         {"p/finish"},
		"/hosts/0": {"p/node1"},
	}))

	var refused *pipeline.InputChangeError

	require.ErrorAs(t, out.err, &refused)
	require.Len(t, refused.Fields, 1)
	assert.Equal(t, "/hosts", refused.Fields[0].Pointer)
	assert.Equal(t, "p/node1", refused.Fields[0].Consumer)
}

// In an array a null holds a place, so one more or one fewer is a change,
// and it is named.
func TestInputChange_ATrailingNullElementIsAChange(t *testing.T) {
	t.Parallel()

	type listed struct {
		Items []*int `json:"items"`
	}

	var c calls

	one := 1
	fails := true
	plan := installPlan(t, &c, &fails)
	j := pipeline.MemoryJournal()

	require.Error(t, resumeOn(t, j, plan, pipeline.WithInputs(listed{[]*int{&one}})).err)

	longer := pipeline.WithInputs(listed{[]*int{&one, nil}})
	consumers := pipeline.WithInputConsumers(map[string][]string{"/items": {"p/prepare"}})

	out := resumeOn(t, j, plan, longer, consumers)

	var refused *pipeline.InputChangeError

	require.ErrorAs(t, out.err, &refused, "prepare has used /items")
	assert.Equal(t, "/items/1", refused.Fields[0].Pointer)
	assert.Nil(t, refused.Fields[0].Old)
	assert.Equal(t, "null", string(refused.Fields[0].New))

	fails = false
	out = resumeOn(t, j, plan, longer, pipeline.WithNewInputs())
	require.NoError(t, out.err)
	assert.Equal(t, []string{"/items/1"}, out.events[0].InputsChanged)
}

// A refusal is redacted like every other error an attempt returns, and
// still matches what it wraps.
func TestInputChange_ARefusalIsRedacted(t *testing.T) {
	t.Parallel()

	var c calls

	fails := true
	plan := installPlan(t, &c, &fails)
	j := pipeline.MemoryJournal()

	in := twoHosts()
	in.Password = pipeline.NewSecret("hunter2-hunter2")
	require.Error(t, resumeOn(t, j, plan, pipeline.WithInputs(in)).err)

	in.Version = "8.3-" + secret
	errNo := errors.New("refused for " + in.Password.Reveal())

	out := resumeOn(t, j, plan, pipeline.WithInputs(in),
		pipeline.WithInputChange(func(pipeline.InputChange) error { return errNo }))
	require.ErrorIs(t, out.err, errNo)
	require.ErrorIs(t, out.err, pipeline.ErrInputsChanged)
	assert.NotContains(t, out.err.Error(), "hunter2-hunter2", "an input secret is masked")

	out = resumeOn(t, j, plan, pipeline.WithInputs(in), installConsumers(), pipeline.WithRedactor(hideSecret))

	var refused *pipeline.InputChangeError

	require.ErrorAs(t, out.err, &refused)
	assert.NotContains(t, out.err.Error(), secret, "and so is what the redactor covers")
	assert.Contains(t, out.err.Error(), "/version changed")
}

// A step that its group or the run never reached is recorded as skipped. It
// has not used its inputs.
func TestInputChange_ASkippedConsumerHasNotStarted(t *testing.T) {
	t.Parallel()

	var c calls

	fails := true
	skips := true
	early := pipeline.StepFunc(func(context.Context, *pipeline.StepContext) error {
		if skips {
			return pipeline.ErrSkipGroup
		}

		return nil
	})
	plan := mustPlan(t,
		pipeline.Group("node1", pipeline.Effect("early", early), effect(&c, "install1", nil)),
		effect(&c, "b", &fails),
	)
	j := pipeline.MemoryJournal()

	in := twoHosts()
	require.Error(t, resumeOn(t, j, plan, pipeline.WithInputs(in)).err)

	state, err := j.State()
	require.NoError(t, err)
	require.Equal(t, pipeline.StatusSkipped, state.Element("p/node1/install1").Status)

	consumers := pipeline.WithInputConsumers(map[string][]string{
		"/hosts/0": {"p/node1/install1"},
		"/hosts/1": {"p/node1"},
	})

	in.Hosts[0].Address = "10.0.0.11"
	require.Error(t, resumeOn(t, j, plan, pipeline.WithInputs(in), consumers).err)

	state, err = j.State()
	require.NoError(t, err)
	assert.Equal(t, []string{"/hosts/0/address"}, state.Attempts[1].InputsChanged, "install1 was skipped, not started")

	// The group has started: early ran in it.
	in.Hosts[1].Address = "10.0.0.22"
	require.ErrorIs(t, resumeOn(t, j, plan, pipeline.WithInputs(in), consumers).err, pipeline.ErrInputsChanged)
}

// A large number is reported as it was written, not rounded.
func TestInputChange_NumbersAreReportedAsWritten(t *testing.T) {
	t.Parallel()

	type sized struct {
		Bytes int64 `json:"bytes"`
	}

	var c calls

	fails := true
	plan := installPlan(t, &c, &fails)
	j := pipeline.MemoryJournal()

	require.Error(t, resumeOn(t, j, plan, pipeline.WithInputs(sized{9007199254740992})).err)

	out := resumeOn(t, j, plan, pipeline.WithInputs(sized{9007199254740993}),
		pipeline.WithInputConsumers(map[string][]string{"/bytes": {"p/prepare"}}))

	var refused *pipeline.InputChangeError

	require.ErrorAs(t, out.err, &refused)
	assert.Equal(t, "9007199254740992", string(refused.Fields[0].Old))
	assert.Equal(t, "9007199254740993", string(refused.Fields[0].New))
}

// A consumer the plan lacks is found on the first run, not only when the
// inputs change, and WithNewInputs doesn't hide it.
func TestInputChange_ConsumersAreCheckedOnEveryAttempt(t *testing.T) {
	t.Parallel()

	var c calls

	fails := false
	plan := installPlan(t, &c, &fails)
	typo := pipeline.WithInputConsumers(map[string][]string{"/version": {"p/nowhere"}})

	out := resumeOn(t, pipeline.MemoryJournal(), plan, pipeline.WithInputs(twoHosts()), typo, pipeline.WithNewInputs())
	require.ErrorContains(t, out.err, "input consumer p/nowhere, of /version, is not in the plan")
	assert.Empty(t, out.events)
	assert.Zero(t, c.get("prepare"), "nothing has run")
}
