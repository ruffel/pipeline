package runbook_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ruffel/pipeline/runbook"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const secret = "hunter2"

func hideSecret(s string) string {
	return strings.ReplaceAll(s, secret, "***")
}

// secretError is a typed error whose message holds the secret.
type secretError struct{ code int }

func (e *secretError) Error() string { return fmt.Sprintf("login with %s failed (%d)", secret, e.code) }

func TestRedactor_NothingASecretStepWritesReachesTheJournalOrObservers(t *testing.T) {
	t.Parallel()

	var tries int

	plan := mustPlan(t,
		runbook.Effect("leaky", runbook.StepFunc(func(_ context.Context, s *runbook.StepContext) error {
			s.Info("connecting with password " + secret)
			s.Warn("still using " + secret)
			s.Progress("login", "trying "+secret, 1, 2)
			_, _ = s.Stdout().Write([]byte("PASSWORD=" + secret + "\n"))
			_, _ = s.Stderr().Write([]byte("bad password " + secret + "\n"))

			if err := s.Item("node1", runbook.ItemFailed, "rejected "+secret); err != nil {
				return err
			}

			if err := s.Custom("auth.attempt", map[string]any{
				"password": secret,
				"tries":    3,
				"nested":   []any{"x", map[string]any{"again": "is " + secret}},
			}); err != nil {
				return err
			}

			if err := s.Record("note", "recorded values are not redacted: "+secret); err != nil {
				return err
			}

			tries++
			if tries == 1 {
				return errors.New("first try: " + secret)
			}

			return &secretError{code: 401}
		})).Retry(2, time.Millisecond),
	)

	out := runPlan(t, t.Context(), plan, runbook.WithRedactor(hideSecret))

	// Everything the journal holds, and so everything observers and viewers
	// see, except the recorded value.
	for _, e := range out.events {
		if e.Kind == runbook.EventValue {
			continue
		}

		line, err := json.Marshal(e)
		require.NoError(t, err)
		assert.NotContains(t, string(line), secret, "%s event", e.Kind)
	}

	byKind := map[runbook.EventKind][]runbook.Event{}
	for _, e := range out.events {
		byKind[e.Kind] = append(byKind[e.Kind], e)
	}

	assert.Equal(t, "connecting with password ***", byKind[runbook.EventMessage][0].Message)
	assert.Equal(t, "trying ***", byKind[runbook.EventProgress][0].Message)
	assert.Equal(t, "PASSWORD=***", byKind[runbook.EventOutput][0].Line)
	assert.Equal(t, "rejected ***", byKind[runbook.EventItem][0].Message)
	assert.Equal(t, "first try: ***", byKind[runbook.EventStepRetry][0].Error.Message)
	assert.JSONEq(t, `{"password": "***", "tries": 3, "nested": ["x", {"again": "is ***"}]}`,
		string(byKind[runbook.EventCustom][0].Data), "strings are redacted; keys, numbers and shape are kept")

	finished := byKind[runbook.EventStepFinished][0]
	assert.Equal(t, "login with *** failed (401)", finished.Error.Message)
	assert.Equal(t, "p/leaky: login with *** failed (401)", byKind[runbook.EventRunFinished][0].Error.Message)

	v, ok := out.state.Value("p/leaky", "note")
	require.True(t, ok)
	assert.Contains(t, string(v), secret, "recorded values are stored as given; they must not be secret")

	// The error Run returns is redacted too, and still unwraps to the
	// step's own error.
	require.Error(t, out.err)
	assert.Equal(t, "p/leaky: login with *** failed (401)", out.err.Error())

	var typed *secretError
	require.ErrorAs(t, out.err, &typed)
	assert.Equal(t, 401, typed.code)
}

func TestRedactor_PanicsAreRedacted(t *testing.T) {
	t.Parallel()

	out := runPlan(t, t.Context(), mustPlan(t, runbook.Effect("a", runbook.StepFunc(
		func(context.Context, *runbook.StepContext) error { panic("crashed holding " + secret) }))),
		runbook.WithRedactor(func(s string) string {
			// Stacks name functions; hide those too, to prove the stack goes
			// through the redactor.
			return strings.ReplaceAll(hideSecret(s), "runbook", "[pkg]")
		}))

	info := out.state.Element("p/a").Error
	require.NotNil(t, info)
	assert.Equal(t, "panic: crashed holding ***", info.Message)
	assert.NotEmpty(t, info.Stack)
	assert.NotContains(t, info.Stack, "runbook")
	assert.NotContains(t, out.err.Error(), secret)

	var pe *runbook.PanicError
	require.ErrorAs(t, out.err, &pe, "the original panic is still reachable")
}

func TestRedactor_EngineWarningsAreRedactedToo(t *testing.T) {
	t.Parallel()

	bad := runbook.ObserverFunc(func(_ context.Context, e runbook.Event) {
		if e.Kind == runbook.EventStepRunning {
			panic("renderer died showing " + secret)
		}
	})

	out := runPlan(t, t.Context(), mustPlan(t, runbook.Effect("a", ok())),
		runbook.WithObservers(bad), runbook.WithRedactor(hideSecret))

	require.NoError(t, out.err)

	var warned bool

	for _, e := range out.events {
		assert.NotContains(t, e.Message, secret)

		if e.Level == runbook.LevelWarn && strings.Contains(e.Message, "renderer died showing ***") {
			warned = true
		}
	}

	assert.True(t, warned)
}

// A redactor that fails must not let the text through.
func TestRedactor_APanickingRedactorHidesTheWholeText(t *testing.T) {
	t.Parallel()

	out := runPlan(t, t.Context(), mustPlan(t, runbook.Effect("a", runbook.StepFunc(
		func(_ context.Context, s *runbook.StepContext) error {
			s.Info("password " + secret)

			return s.Custom("x.y", map[string]string{"k": secret})
		}))),
		runbook.WithRedactor(func(string) string { panic("redactor bug") }))

	require.NoError(t, out.err)

	for _, e := range out.events {
		line, err := json.Marshal(e)
		require.NoError(t, err)
		assert.NotContains(t, string(line), secret)
	}

	for _, e := range out.events {
		switch e.Kind { //nolint:exhaustive // Only the two kinds the step wrote.
		case runbook.EventMessage:
			assert.Equal(t, "[redaction failed]", e.Message)
		case runbook.EventCustom:
			assert.JSONEq(t, `{"k": "[redaction failed]"}`, string(e.Data))
		}
	}
}

func TestRedactor_WithoutOneNothingChanges(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("plain " + secret)

	out := runPlan(t, t.Context(), mustPlan(t, runbook.Effect("a", runbook.StepFunc(
		func(_ context.Context, s *runbook.StepContext) error {
			s.Info(secret)

			return sentinel
		}))))

	require.ErrorIs(t, out.err, sentinel)
	assert.Equal(t, "p/a: plain "+secret, out.err.Error())

	var se *runbook.StepError
	require.ErrorAs(t, out.err, &se)
	assert.Same(t, sentinel, se.Err, "with no redactor the step's error is not wrapped")
}
