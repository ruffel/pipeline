package pipeline_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/ruffel/pipeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const adminPassword = "correct-horse-battery"

// upgradeInputs is what a consumer's plan is built from.
type upgradeInputs struct {
	Nodes         []string
	TargetVersion string
	AdminPassword pipeline.Secret
	Tokens        map[string]*pipeline.Secret
}

func someInputs() upgradeInputs {
	token := pipeline.NewSecret("tok-9f8e7d6c")

	return upgradeInputs{
		Nodes:         []string{"node1", "node2"},
		TargetVersion: "8.2.0",
		AdminPassword: pipeline.NewSecret(adminPassword),
		Tokens:        map[string]*pipeline.Secret{"api": &token},
	}
}

func TestSecret_NeverShowsItsValue(t *testing.T) {
	t.Parallel()

	s := pipeline.NewSecret(adminPassword)

	// %p, and a field fmt can't call methods on, are printed by fmt itself
	// without asking the value.
	holders := map[string]any{
		"alone":                  s,
		"by pointer":             &s,
		"in a struct":            struct{ P pipeline.Secret }{s},
		"in an unexported field": struct{ p pipeline.Secret }{s},
		"behind a pointer field": struct{ p *pipeline.Secret }{&s},
		"in a slice":             []pipeline.Secret{s},
		"in a map":               map[string]any{"k": s},
	}

	for _, format := range []string{"%v", "%s", "%q", "%+v", "%#v", "%x", "%X", "%d", "%p", "%T", "%c", "%U", "%e"} {
		for name, holder := range holders {
			assert.NotContains(t, fmt.Sprintf(format, holder), adminPassword, "%s, %s", name, format)
		}
	}

	assert.Equal(t, "[secret]", fmt.Sprint(s))
	assert.Equal(t, "[secret]", s.String())
	assert.Equal(t, adminPassword, s.Reveal())
	assert.Empty(t, pipeline.Secret{}.Reveal(), "the zero Secret is empty")

	type holder struct {
		Value pipeline.Secret `json:"value"`
	}

	encoded, err := json.Marshal(holder{Value: s})
	require.NoError(t, err)
	assert.JSONEq(t, `{"value": null}`, string(encoded))

	var back holder
	require.NoError(t, json.Unmarshal(encoded, &back))
	assert.Empty(t, back.Value, "a secret comes back empty")

	text, err := s.MarshalText()
	require.NoError(t, err)
	assert.Equal(t, "[secret]", string(text))

	// A consumer's own configuration decodes into one.
	require.NoError(t, json.Unmarshal([]byte(`{"value": "from-a-file"}`), &back))
	assert.Equal(t, "from-a-file", back.Value.Reveal())

	require.NoError(t, json.Unmarshal([]byte(`{"value": null}`), &back))
	assert.Equal(t, "from-a-file", back.Value.Reveal(), "null leaves it alone")

	require.Error(t, json.Unmarshal([]byte(`{"value": 42}`), &back))
}

func TestInputs_AreRecordedWithoutSecrets(t *testing.T) {
	t.Parallel()

	in := someInputs()

	var seen any

	out := runPlan(t, t.Context(), mustPlan(t, pipeline.Effect("a", pipeline.StepFunc(
		func(_ context.Context, s *pipeline.StepContext) error {
			seen = s.Inputs()

			return nil
		}))), pipeline.WithInputs(in))

	require.NoError(t, out.err)

	started := out.events[0]
	assert.JSONEq(t, `{
		"Nodes": ["node1", "node2"], "TargetVersion": "8.2.0", "AdminPassword": null, "Tokens": {"api": null}
	}`, string(started.Inputs))
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, started.InputsDigest)

	for _, e := range out.events {
		line, err := json.Marshal(e)
		require.NoError(t, err)
		assert.NotContains(t, string(line), adminPassword)
		assert.NotContains(t, string(line), "tok-9f8e7d6c")
	}

	// A resuming tool reads the answers back; the secrets are not there.
	var back upgradeInputs

	found, err := out.state.DecodeInputs(&back)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, in.Nodes, back.Nodes)
	assert.Equal(t, "8.2.0", back.TargetVersion)
	assert.Empty(t, back.AdminPassword)
	assert.Equal(t, out.events[0].InputsDigest, out.state.InputsDigest)

	// A step gets the value itself, secrets included.
	live, ok := seen.(upgradeInputs)
	require.True(t, ok)
	assert.Equal(t, adminPassword, live.AdminPassword.Reveal())
}

func TestInputs_ARunWithoutAny(t *testing.T) {
	t.Parallel()

	out := runPlan(t, t.Context(), mustPlan(t, pipeline.Effect("a", pipeline.StepFunc(
		func(_ context.Context, s *pipeline.StepContext) error {
			if s.Inputs() != nil {
				return errors.New("no inputs were given")
			}

			return nil
		}))))

	require.NoError(t, out.err)
	assert.Empty(t, out.events[0].Inputs)
	assert.Empty(t, out.events[0].InputsDigest)

	found, err := out.state.DecodeInputs(new(upgradeInputs))
	require.NoError(t, err)
	assert.False(t, found)
}

func TestInputs_AResumeMustBringTheSameInputs(t *testing.T) {
	t.Parallel()

	failing := true
	c := &calls{}
	plan := mustPlan(t, pipeline.Effect("a", counted(c, "a", &failing)))
	j := pipeline.MemoryJournal()

	in := someInputs()
	require.Error(t, resumeOn(t, j, plan, pipeline.WithInputs(in)).err)

	failing = false

	// The resume flow: read the stored answers, supply the secrets again.
	state, err := j.State()
	require.NoError(t, err)

	var stored upgradeInputs

	found, err := state.DecodeInputs(&stored)
	require.NoError(t, err)
	require.True(t, found)

	// Changed inputs are refused; so is leaving them out, and so is forcing.
	changed := stored
	changed.TargetVersion = "8.3.0"

	require.ErrorIs(t, resumeOn(t, j, plan, pipeline.WithInputs(changed)).err, pipeline.ErrInputsChanged)
	require.ErrorIs(t, resumeOn(t, j, plan).err, pipeline.ErrInputsChanged)
	require.ErrorIs(t, resumeOn(t, j, plan, pipeline.WithInputs(changed), pipeline.Force()).err, pipeline.ErrInputsChanged)

	// The stored inputs with a new secret are the same inputs.
	stored.AdminPassword = pipeline.NewSecret("a-rotated-password")

	second := resumeOn(t, j, plan, pipeline.WithInputs(stored))
	require.NoError(t, second.err)
	assert.Equal(t, 2, second.res.Attempt)
}

func TestInputs_WithNewInputsAcceptsAChange(t *testing.T) {
	t.Parallel()

	failing := true
	c := &calls{}
	plan := mustPlan(t, pipeline.Effect("a", counted(c, "a", &failing)))
	j := pipeline.MemoryJournal()

	require.Error(t, resumeOn(t, j, plan, pipeline.WithInputs(someInputs())).err)

	failing = false

	changed := someInputs()
	changed.TargetVersion = "8.3.0"

	second := resumeOn(t, j, plan, pipeline.WithInputs(changed), pipeline.WithNewInputs())
	require.NoError(t, second.err)

	state, err := j.State()
	require.NoError(t, err)

	var recorded upgradeInputs

	_, err = state.DecodeInputs(&recorded)
	require.NoError(t, err)
	assert.Equal(t, "8.3.0", recorded.TargetVersion, "the new attempt records the new inputs")
}

// Input secrets are masked in everything a step writes, with no redactor
// set, wherever in the inputs they were.
func TestInputs_SecretsAreMaskedInWhatStepsWrite(t *testing.T) {
	t.Parallel()

	plan := mustPlan(t, pipeline.Effect("login", pipeline.StepFunc(
		func(_ context.Context, s *pipeline.StepContext) error {
			in, _ := s.Inputs().(upgradeInputs)
			password := in.AdminPassword.Reveal()

			s.Info("logging in with " + password)
			_, _ = s.Stdout().Write([]byte("PASSWORD=" + password + " TOKEN=" + in.Tokens["api"].Reveal() + "\n"))

			if err := s.Custom("auth.try", map[string]string{"password": password}); err != nil {
				return err
			}

			return errors.New("rejected password " + password)
		})))

	out := runPlan(t, t.Context(), plan, pipeline.WithInputs(someInputs()))

	require.Error(t, out.err)
	assert.Equal(t, "p/login: rejected password [secret]", out.err.Error())

	for _, e := range out.events {
		line, err := json.Marshal(e)
		require.NoError(t, err)
		assert.NotContains(t, string(line), adminPassword, "%s event", e.Kind)
		assert.NotContains(t, string(line), "tok-9f8e7d6c", "%s event", e.Kind)
	}

	lines := outputText(out.events)
	require.Len(t, lines, 1)
	assert.Equal(t, "PASSWORD=[secret] TOKEN=[secret]", lines[0])
}

func TestInputs_SecretMaskingRunsBeforeTheRedactor(t *testing.T) {
	t.Parallel()

	var given []string

	out := runPlan(t, t.Context(), mustPlan(t, pipeline.Effect("a", pipeline.StepFunc(
		func(_ context.Context, s *pipeline.StepContext) error {
			s.Info("password " + adminPassword + " and ssn 123-45-6789")

			return nil
		}))),
		pipeline.WithInputs(someInputs()),
		pipeline.WithRedactor(func(text string) string {
			given = append(given, text)

			return hideSSN(text)
		}))

	require.NoError(t, out.err)

	for _, text := range given {
		assert.NotContains(t, text, adminPassword, "the redactor never sees an input secret")
	}

	var message string

	for _, e := range out.events {
		if e.Kind == pipeline.EventMessage {
			message = e.Message
		}
	}

	assert.Equal(t, "password [secret] and ssn ***", message)
}

func hideSSN(s string) string {
	const ssn = "123-45-6789"

	for i := 0; i+len(ssn) <= len(s); i++ {
		if s[i:i+len(ssn)] == ssn {
			return s[:i] + "***" + s[i+len(ssn):]
		}
	}

	return s
}

// A very short secret would turn ordinary text into noise if it were masked
// everywhere, so it is only kept out of the stored inputs.
func TestInputs_AVeryShortSecretIsNotMaskedInText(t *testing.T) {
	t.Parallel()

	type pin struct{ PIN pipeline.Secret }

	out := runPlan(t, t.Context(), mustPlan(t, pipeline.Effect("a", pipeline.StepFunc(
		func(_ context.Context, s *pipeline.StepContext) error {
			s.Info("a banana")

			return nil
		}))), pipeline.WithInputs(pin{PIN: pipeline.NewSecret("an")}))

	require.NoError(t, out.err)
	assert.JSONEq(t, `{"PIN": null}`, string(out.events[0].Inputs))

	var message string

	for _, e := range out.events {
		if e.Kind == pipeline.EventMessage {
			message = e.Message
		}
	}

	assert.Equal(t, "a banana", message)
}

func TestInputs_ThatCannotBeRecordedStopTheRunBeforeItStarts(t *testing.T) {
	t.Parallel()

	ran := false
	plan := mustPlan(t, pipeline.Effect("a", pipeline.StepFunc(func(context.Context, *pipeline.StepContext) error {
		ran = true

		return nil
	})))

	for name, bad := range map[string]any{
		"unencodable": map[string]any{"ratio": math.NaN()},
		"too large":   map[string]string{"blob": strings.Repeat("x", pipeline.MaxInputsBytes)},
		"secret key":  map[pipeline.Secret]int{pipeline.NewSecret("hunter2-key"): 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			j := pipeline.MemoryJournal()

			_, err := pipeline.Run(t.Context(), plan, j, pipeline.WithInputs(bad))
			require.ErrorContains(t, err, "inputs")

			events, err := j.Events()
			require.NoError(t, err)
			assert.Empty(t, events, "nothing was written")
		})
	}

	assert.False(t, ran)
}

// A secret of several lines, such as a private key: output is recorded a
// line at a time, so each of its lines has to be masked, not only the whole.
func TestInputs_EveryLineOfAMultilineSecretIsMasked(t *testing.T) {
	t.Parallel()

	type in struct{ Key pipeline.Secret }

	const (
		first  = "MIIEvQIBADANBgkqhkiG9w0BAQEFAASC"
		second = "BKcwggSjAgEAAoIBAQC7"
		key    = "-----BEGIN KEY-----\n" + first + "\r\n" + second + "\n-----END KEY-----\n"
	)

	out := runPlan(t, t.Context(), mustPlan(t, pipeline.Effect("a", pipeline.StepFunc(
		func(_ context.Context, s *pipeline.StepContext) error {
			s.Info("the key is " + key)
			s.Warn("trimmed: " + strings.TrimSpace(key))
			_, _ = s.Stdout().Write([]byte("cat key.pem\n" + key))
			_, _ = s.Stderr().Write([]byte("bad line: " + second + "\n"))

			return errors.New("could not parse " + first)
		}))), pipeline.WithInputs(in{Key: pipeline.NewSecret(key)}))

	require.Error(t, out.err)
	assert.Equal(t, "p/a: could not parse [secret]", out.err.Error())

	for _, e := range out.events {
		line, err := json.Marshal(e)
		require.NoError(t, err)

		for _, part := range []string{first, second, "BEGIN KEY", "END KEY"} {
			assert.NotContains(t, string(line), part, "%s event", e.Kind)
		}
	}

	assert.Equal(t, []string{"cat key.pem", "[secret]", "[secret]", "[secret]", "[secret]", "bad line: [secret]"},
		outputText(out.events))
}

// nest wraps v in n slices.
func nest(v any, n int) any {
	for range n {
		v = []any{v}
	}

	return v
}

// However deep a Secret is, it is found: nothing in the inputs goes
// unchecked.
func TestInputs_SecretsAreFoundAtAnyDepth(t *testing.T) {
	t.Parallel()

	t.Run("a map key is refused", func(t *testing.T) {
		t.Parallel()

		j := pipeline.MemoryJournal()
		bad := nest(map[pipeline.Secret]int{pipeline.NewSecret("hunter2-key"): 1}, 100)

		_, err := pipeline.Run(t.Context(), mustPlan(t, pipeline.Effect("a", ok())), j, pipeline.WithInputs(bad))
		require.ErrorContains(t, err, "map key")

		events, err := j.Events()
		require.NoError(t, err)
		assert.Empty(t, events, "nothing was written")
	})

	t.Run("a value is masked", func(t *testing.T) {
		t.Parallel()

		out := runPlan(t, t.Context(), mustPlan(t, pipeline.Effect("a", pipeline.StepFunc(
			func(_ context.Context, s *pipeline.StepContext) error {
				s.Info("using " + adminPassword)

				return nil
			}))), pipeline.WithInputs(nest(pipeline.NewSecret(adminPassword), 100)))

		require.NoError(t, out.err)

		for _, e := range out.events {
			assert.NotContains(t, e.Message, adminPassword)
		}
	})
}

// looped points back at itself through a field that is not encoded.
type looped struct {
	Name     string
	Password pipeline.Secret
	self     *looped
	peers    []*looped
}

func TestInputs_ThatPointBackAtThemselvesAreStillSearched(t *testing.T) {
	t.Parallel()

	in := &looped{Name: "a", Password: pipeline.NewSecret(adminPassword)}
	in.self = in
	in.peers = []*looped{in, {Name: "b", Password: pipeline.NewSecret("another-password"), self: in}}

	out := runPlan(t, t.Context(), mustPlan(t, pipeline.Effect("a", pipeline.StepFunc(
		func(_ context.Context, s *pipeline.StepContext) error {
			s.Info(adminPassword + " and another-password")

			return nil
		}))), pipeline.WithInputs(in))

	require.NoError(t, out.err)
	assert.JSONEq(t, `{"Name": "a", "Password": null}`, string(out.events[0].Inputs))

	for _, e := range out.events {
		if e.Kind == pipeline.EventMessage {
			assert.Equal(t, "[secret] and [secret]", e.Message)
		}
	}
}

// optionalSecrets holds secrets in every way a field can be left out of the
// encoding when it is empty.
type optionalSecrets struct {
	Name     string                           `json:"name"`
	Plain    pipeline.Secret                  `json:"plain"`
	Zero     pipeline.Secret                  `json:"zero,omitzero"`
	Pointer  *pipeline.Secret                 `json:"pointer,omitempty"`
	Map      map[string]pipeline.Secret       `json:"map,omitempty"`
	List     []pipeline.Secret                `json:"list,omitempty"`
	Nested   *struct{ Token pipeline.Secret } `json:"nested,omitempty"`
	Optional *int                             `json:"optional,omitempty"`
}

// Supplying a secret that was empty before is not a change of inputs,
// however the field is tagged. Anything else still is.
func TestInputs_WhetherASecretIsSetIsNotPartOfTheComparison(t *testing.T) {
	t.Parallel()

	failing := true
	plan := mustPlan(t, pipeline.Effect("a", counted(&calls{}, "a", &failing)))
	j := pipeline.MemoryJournal()

	_, err := pipeline.Run(t.Context(), plan, j, pipeline.WithInputs(optionalSecrets{Name: "x"}))
	require.Error(t, err)

	secret := pipeline.NewSecret(adminPassword)
	filled := optionalSecrets{
		Name: "x", Plain: secret, Zero: secret, Pointer: &secret,
		Map:    map[string]pipeline.Secret{"api": secret},
		List:   []pipeline.Secret{secret, secret},
		Nested: &struct{ Token pipeline.Secret }{secret},
	}

	changed := filled
	changed.Name = "y"

	_, err = pipeline.Run(t.Context(), plan, j, pipeline.WithInputs(changed))
	require.ErrorIs(t, err, pipeline.ErrInputsChanged, "a change to anything else is still a change")

	one := 1
	changed = filled
	changed.Optional = &one

	_, err = pipeline.Run(t.Context(), plan, j, pipeline.WithInputs(changed))
	require.ErrorIs(t, err, pipeline.ErrInputsChanged, "so is a field that was left out and now has a value")

	failing = false

	_, err = pipeline.Run(t.Context(), plan, j, pipeline.WithInputs(filled))
	require.NoError(t, err)

	state, err := j.State()
	require.NoError(t, err)
	assert.NotContains(t, string(state.Inputs), adminPassword)
}
