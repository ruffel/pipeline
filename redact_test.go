package pipeline_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ruffel/pipeline"
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
		pipeline.Effect("leaky", pipeline.StepFunc(func(_ context.Context, s *pipeline.StepContext) error {
			s.Info("connecting with password " + secret)
			s.Warn("still using " + secret)
			s.Progress("login", "trying "+secret, 1, 2)
			_, _ = s.Stdout().Write([]byte("PASSWORD=" + secret + "\n"))
			_, _ = s.Stderr().Write([]byte("bad password " + secret + "\n"))

			if err := s.Item("node1", pipeline.ItemFailed, "rejected "+secret); err != nil {
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

	out := runPlan(t, t.Context(), plan, pipeline.WithRedactor(hideSecret))

	// Everything the journal holds, and so everything observers and viewers
	// see, except the recorded value.
	for _, e := range out.events {
		if e.Kind == pipeline.EventValue {
			continue
		}

		line, err := json.Marshal(e)
		require.NoError(t, err)
		assert.NotContains(t, string(line), secret, "%s event", e.Kind)
	}

	byKind := map[pipeline.EventKind][]pipeline.Event{}
	for _, e := range out.events {
		byKind[e.Kind] = append(byKind[e.Kind], e)
	}

	assert.Equal(t, "connecting with password ***", byKind[pipeline.EventMessage][0].Message)
	assert.Equal(t, "trying ***", byKind[pipeline.EventProgress][0].Message)
	assert.Equal(t, "PASSWORD=***", byKind[pipeline.EventOutput][0].Line)
	assert.Equal(t, "rejected ***", byKind[pipeline.EventItem][0].Message)
	assert.Equal(t, "first try: ***", byKind[pipeline.EventStepRetry][0].Error.Message)
	assert.JSONEq(t, `{"password": "***", "tries": 3, "nested": ["x", {"again": "is ***"}]}`,
		string(byKind[pipeline.EventCustom][0].Data), "strings are redacted; keys, numbers and shape are kept")

	finished := byKind[pipeline.EventStepFinished][0]
	assert.Equal(t, "login with *** failed (401)", finished.Error.Message)
	assert.Equal(t, "p/leaky: login with *** failed (401)", byKind[pipeline.EventRunFinished][0].Error.Message)

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

	out := runPlan(t, t.Context(), mustPlan(t, pipeline.Effect("a", pipeline.StepFunc(
		func(context.Context, *pipeline.StepContext) error { panic("crashed holding " + secret) }))),
		pipeline.WithRedactor(func(s string) string {
			// Stacks name functions; hide those too, to prove the stack goes
			// through the redactor.
			return strings.ReplaceAll(hideSecret(s), "pipeline", "[pkg]")
		}))

	info := out.state.Element("p/a").Error
	require.NotNil(t, info)
	assert.Equal(t, "panic: crashed holding ***", info.Message)
	assert.NotEmpty(t, info.Stack)
	assert.NotContains(t, info.Stack, "pipeline")
	assert.NotContains(t, out.err.Error(), secret)

	var pe *pipeline.PanicError
	require.ErrorAs(t, out.err, &pe, "the original panic is still reachable")
}

func TestRedactor_EngineWarningsAreRedactedToo(t *testing.T) {
	t.Parallel()

	bad := pipeline.ObserverFunc(func(_ context.Context, e pipeline.Event) {
		if e.Kind == pipeline.EventStepRunning {
			panic("renderer died showing " + secret)
		}
	})

	out := runPlan(t, t.Context(), mustPlan(t, pipeline.Effect("a", ok())),
		pipeline.WithObservers(bad), pipeline.WithRedactor(hideSecret))

	require.NoError(t, out.err)

	var warned bool

	for _, e := range out.events {
		assert.NotContains(t, e.Message, secret)

		if e.Level == pipeline.LevelWarn && strings.Contains(e.Message, "renderer died showing ***") {
			warned = true
		}
	}

	assert.True(t, warned)
}

// A redactor that fails must not let the text through.
func TestRedactor_APanickingRedactorHidesTheWholeText(t *testing.T) {
	t.Parallel()

	out := runPlan(t, t.Context(), mustPlan(t, pipeline.Effect("a", pipeline.StepFunc(
		func(_ context.Context, s *pipeline.StepContext) error {
			s.Info("password " + secret)

			return s.Custom("x.y", map[string]string{"k": secret})
		}))),
		pipeline.WithRedactor(func(string) string { panic("redactor bug") }))

	require.NoError(t, out.err)

	for _, e := range out.events {
		line, err := json.Marshal(e)
		require.NoError(t, err)
		assert.NotContains(t, string(line), secret)
	}

	for _, e := range out.events {
		switch e.Kind { //nolint:exhaustive // Only the two kinds the step wrote.
		case pipeline.EventMessage:
			assert.Equal(t, "[redaction failed]", e.Message)
		case pipeline.EventCustom:
			assert.JSONEq(t, `{"k": "[redaction failed]"}`, string(e.Data))
		}
	}
}

func TestRedactor_WithoutOneNothingChanges(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("plain " + secret)

	out := runPlan(t, t.Context(), mustPlan(t, pipeline.Effect("a", pipeline.StepFunc(
		func(_ context.Context, s *pipeline.StepContext) error {
			s.Info(secret)

			return sentinel
		}))))

	require.ErrorIs(t, out.err, sentinel)
	assert.Equal(t, "p/a: plain "+secret, out.err.Error())

	var se *pipeline.StepError
	require.ErrorAs(t, out.err, &se)
	assert.Same(t, sentinel, se.Err, "with no redactor the step's error is not wrapped")
}

// outputText joins a step's output events back into lines: a continued
// piece belongs to the line before it.
func outputText(events []pipeline.Event) []string {
	var lines []string

	for _, e := range events {
		if e.Kind != pipeline.EventOutput {
			continue
		}

		if e.Continued && len(lines) > 0 {
			lines[len(lines)-1] += e.Line

			continue
		}

		lines = append(lines, e.Line)
	}

	return lines
}

// A secret that straddles the point where a long line is split must still be
// redacted: the redactor has to see the line whole.
func TestRedactor_SeesALongOutputLineWholeBeforeItIsSplit(t *testing.T) {
	t.Parallel()

	filler := strings.Repeat("x", pipeline.MaxOutputLine-2)

	out := runPlan(t, t.Context(), mustPlan(t, pipeline.Effect("a", pipeline.StepFunc(
		func(_ context.Context, s *pipeline.StepContext) error {
			_, err := s.Stdout().Write([]byte(filler + secret + "\nnext line\n"))

			return err
		}))), pipeline.WithRedactor(hideSecret))

	require.NoError(t, out.err)

	lines := outputText(out.events)
	require.Len(t, lines, 2)
	assert.Equal(t, filler+"***", lines[0], "redacted whole, then split")
	assert.Equal(t, "next line", lines[1])

	for _, e := range out.events {
		assert.LessOrEqual(t, len(e.Line), pipeline.MaxOutputLine, "pieces still respect the line cap")
	}
}

// A line too long to hold whole can't be redacted safely, so it is hidden.
func TestRedactor_HidesAnOutputLineTooLongToRedact(t *testing.T) {
	t.Parallel()

	out := runPlan(t, t.Context(), mustPlan(t, pipeline.Effect("a", pipeline.StepFunc(
		func(_ context.Context, s *pipeline.StepContext) error {
			w := s.Stdout()

			_, _ = w.Write([]byte(strings.Repeat("y", pipeline.MaxRedactLine-3)))
			_, _ = w.Write([]byte(secret + strings.Repeat("z", 100) + "\n"))
			_, err := w.Write([]byte("after\n"))

			return err
		}))), pipeline.WithRedactor(hideSecret))

	require.NoError(t, out.err)

	lines := outputText(out.events)
	require.Len(t, lines, 2)
	assert.Contains(t, lines[0], "too long to redact")
	assert.Equal(t, "after", lines[1])

	for _, e := range out.events {
		line, err := json.Marshal(e)
		require.NoError(t, err)
		assert.NotContains(t, string(line), "hu", "not even part of the secret")
		assert.NotContains(t, string(line), "yyyy")
	}
}

// Every error Run returns is redacted, not only step failures.
func TestRedactor_CoversEveryErrorRunReturns(t *testing.T) {
	t.Parallel()

	t.Run("a cancel cause", func(t *testing.T) {
		t.Parallel()

		cause := errors.New("aborted by operator with token " + secret)
		ctx, cancel := context.WithCancelCause(t.Context())

		out := runPlan(t, ctx, mustPlan(t,
			pipeline.Effect("a", pipeline.StepFunc(func(context.Context, *pipeline.StepContext) error {
				cancel(cause)

				return nil
			})),
			pipeline.Effect("b", ok()),
		), pipeline.WithRedactor(hideSecret))

		require.ErrorIs(t, out.err, cause, "the cause is still reachable")
		assert.NotContains(t, out.err.Error(), secret)
		assert.Contains(t, out.err.Error(), "token ***")
	})

	t.Run("a journal failure", func(t *testing.T) {
		t.Parallel()

		diskErr := errors.New("write /mnt/" + secret + "/journal: disk full")
		j := pipeline.MemoryJournal(pipeline.WithWriteHook(func(e pipeline.Event) error {
			if e.Kind == pipeline.EventStepStarted {
				return diskErr
			}

			return nil
		}))

		_, err := pipeline.Run(t.Context(), mustPlan(t, pipeline.Effect("a", ok())), j, pipeline.WithRedactor(hideSecret))
		require.ErrorIs(t, err, diskErr)
		assert.NotContains(t, err.Error(), secret)
	})
}
