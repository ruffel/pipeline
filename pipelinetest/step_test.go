package pipelinetest_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/ruffel/pipeline"
	"github.com/ruffel/pipeline/pipelinetest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunStep_ReturnsWhatTheStepRecorded(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("x", pipeline.MaxOutputLine+10)

	r := pipelinetest.RunStep(t, pipeline.Effect("install", pipeline.StepFunc(
		func(_ context.Context, sc *pipeline.StepContext) error {
			sc.Info("unpacking")
			sc.Warn("disk nearly full")
			_, _ = fmt.Fprintln(sc.Stdout(), "line one")
			_, _ = fmt.Fprintln(sc.Stdout(), long)
			_, _ = fmt.Fprintln(sc.Stderr(), "E: oops")

			if err := sc.Item("node1", pipeline.ItemPassed, "ok"); err != nil {
				return err
			}

			return sc.Record("version", "8.2")
		})))

	require.NoError(t, r.Err)
	assert.Equal(t, "test/install", r.Path)
	assert.Equal(t, pipeline.StatusPassed, r.Element().Status)
	// The long line comes back whole, and the run says it was split.
	assert.Equal(t, []string{
		"unpacking", "disk nearly full",
		fmt.Sprintf("output line longer than %d bytes was split into pieces", pipeline.MaxOutputLine),
	}, r.Messages())
	assert.Equal(t, []string{"line one", long}, r.Stdout())
	assert.Equal(t, []string{"E: oops"}, r.Stderr())
	assert.Equal(t, pipeline.ItemPassed, r.Items()["node1"].Status)

	var version string

	found, err := r.Recorded("version", &version)
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "8.2", version)

	found, err = r.Recorded("missing", &version)
	require.NoError(t, err)
	assert.False(t, found)

	for _, e := range r.Events() {
		assert.Equal(t, "test/install", e.Path)
	}
}

// done is a step whose Check says the work is done, and whose Run fails
// the test.
type done struct{ t *testing.T }

func (done) Check(context.Context, *pipeline.StepContext) (bool, error) { return true, nil }

func (d done) Run(context.Context, *pipeline.StepContext) error {
	d.t.Error("Run was called though Check found the work done")

	return nil
}

func TestRunStep_ChecksBeforeRunning(t *testing.T) {
	t.Parallel()

	r := pipelinetest.RunStep(t, pipeline.Effect("install", done{t}))

	require.NoError(t, r.Err)
	assert.Equal(t, pipeline.StatusSkipped, r.Element().Status)
	assert.Equal(t, pipeline.ReasonDone, r.Element().Reason)
	assert.False(t, r.Element().RunCalled)
}

func TestRunStep_ReturnsTheStepsError(t *testing.T) {
	t.Parallel()

	boom := errors.New("exit status 1")

	r := pipelinetest.RunStep(t, pipeline.Effect("install", pipeline.StepFunc(
		func(context.Context, *pipeline.StepContext) error { return boom })))

	require.ErrorIs(t, r.Err, boom)

	var se *pipeline.StepError

	require.ErrorAs(t, r.Err, &se)
	assert.Equal(t, "test/install", se.Path)
	assert.Equal(t, pipeline.StatusFailed, r.Element().Status)
}

func TestRunStep_RetriesAsARunDoes(t *testing.T) {
	t.Parallel()

	tries := 0

	r := pipelinetest.RunStep(t, pipeline.Effect("install", pipeline.StepFunc(
		func(context.Context, *pipeline.StepContext) error {
			tries++
			if tries == 1 {
				return errors.New("flaky")
			}

			return nil
		})).Retry(2, 0))

	require.NoError(t, r.Err)
	assert.Equal(t, 2, tries)
}

func TestRunStep_UnderAndGiven(t *testing.T) {
	t.Parallel()

	// A step in a node group reads what the backup and its sibling recorded.
	step := pipeline.Effect("restart", pipeline.StepFunc(func(_ context.Context, sc *pipeline.StepContext) error {
		var archive, version string

		if found, err := sc.Recorded("upgrade/backup", "archive", &archive); err != nil || !found {
			return fmt.Errorf("archive: found %v, %w", found, err)
		}

		if found, err := sc.Recorded("upgrade/nodes/node1/packages", "version", &version); err != nil || !found {
			return fmt.Errorf("version: found %v, %w", found, err)
		}

		sc.Info(sc.Path() + " " + archive + " " + version)

		return nil
	}))

	r := pipelinetest.RunStep(t, step,
		pipelinetest.Under("upgrade/nodes/node1"),
		pipelinetest.Given("upgrade/backup", "archive", "/var/backup.tar"),
		pipelinetest.Given("upgrade/nodes/node1/packages", "version", "8.2"))

	require.NoError(t, r.Err)
	assert.Equal(t, "upgrade/nodes/node1/restart", r.Path)
	assert.Equal(t, []string{"upgrade/nodes/node1/restart /var/backup.tar 8.2"}, r.Messages())
}

func TestRunStep_GivenStepsRunFirst(t *testing.T) {
	t.Parallel()

	// upgrade/late is given after a value in the step's own group: it still
	// runs before the step.
	step := pipeline.Effect("x", pipeline.StepFunc(func(_ context.Context, sc *pipeline.StepContext) error {
		var v int

		found, err := sc.Recorded("upgrade/late", "v", &v)
		if err != nil || !found {
			return fmt.Errorf("found %v, %w", found, err)
		}

		return nil
	}))

	r := pipelinetest.RunStep(t, step,
		pipelinetest.Under("upgrade/a"),
		pipelinetest.Given("upgrade/a/early", "v", 1),
		pipelinetest.Given("upgrade/late", "v", 2))

	require.NoError(t, r.Err)
}

func TestRunStep_PassesRunOptions(t *testing.T) {
	t.Parallel()

	type inputs struct{ Token pipeline.Secret }

	step := pipeline.Effect("login", pipeline.StepFunc(func(_ context.Context, sc *pipeline.StepContext) error {
		in, ok := sc.Inputs().(inputs)
		if !ok {
			return errors.New("no inputs")
		}

		sc.Info("token " + in.Token.Reveal())

		return nil
	}))

	r := pipelinetest.RunStep(t, step,
		pipelinetest.WithRunOptions(pipeline.WithInputs(inputs{Token: pipeline.NewSecret("hunter2")})))

	require.NoError(t, r.Err)
	require.Len(t, r.Messages(), 1)
	assert.NotContains(t, r.Messages()[0], "hunter2")
}

func TestRunStep_WithContextCancels(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	step := pipeline.Effect("wait", pipeline.StepFunc(func(ctx context.Context, _ *pipeline.StepContext) error {
		cancel()
		<-ctx.Done()

		return ctx.Err()
	}))

	r := pipelinetest.RunStep(t, step, pipelinetest.WithContext(ctx))

	require.ErrorIs(t, r.Err, context.Canceled)
}

// fatal records a Fatalf and ends the goroutine, as testing.T does.
type fatal struct {
	testing.TB

	msg string
}

func (f *fatal) Fatalf(format string, args ...any) {
	f.msg = fmt.Sprintf(format, args...)

	runtime.Goexit()
}

// runFatal calls RunStep with a TB that records Fatalf, and returns its
// message.
func runFatal(t *testing.T, step pipeline.StepNode, opts ...pipelinetest.StepOption) string {
	t.Helper()

	f := &fatal{TB: t}
	done := make(chan struct{})

	go func() {
		defer close(done)

		pipelinetest.RunStep(f, step, opts...)
	}()

	<-done

	return f.msg
}

func TestRunStep_RefusesPathsThatDontFit(t *testing.T) {
	t.Parallel()

	step := pipeline.Effect("restart", pipeline.StepFunc(func(context.Context, *pipeline.StepContext) error {
		return nil
	}))

	tests := []struct {
		name string
		opts []pipelinetest.StepOption
		want string
	}{
		{"another plan", []pipelinetest.StepOption{pipelinetest.Given("other/backup", "k", 1)}, "not in the plan"},
		{"the plan itself", []pipelinetest.StepOption{pipelinetest.Given("test", "k", 1)}, "not in the plan"},
		{"the step itself", []pipelinetest.StepOption{pipelinetest.Given("test/restart", "k", 1)}, "is the step under test"},
		{
			"a group of the step",
			[]pipelinetest.StepOption{pipelinetest.Under("u/nodes"), pipelinetest.Given("u/nodes", "k", 1)},
			`"nodes" is a step, not a group`,
		},
		{
			"a group of another given step",
			[]pipelinetest.StepOption{
				pipelinetest.Under("u/x"), pipelinetest.Given("u/nodes/a", "k", 1), pipelinetest.Given("u/nodes", "k", 1),
			},
			`"nodes" is a group, not a step`,
		},
		{"a bad name", []pipelinetest.StepOption{pipelinetest.Under("bad name")}, "bad name"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Contains(t, runFatal(t, step, tt.opts...), tt.want)
		})
	}
}

func TestRunStep_FailsWhenAGivenStepFails(t *testing.T) {
	t.Parallel()

	step := pipeline.Effect("x", pipeline.StepFunc(func(context.Context, *pipeline.StepContext) error { return nil }))

	msg := runFatal(t, step, pipelinetest.Given("test/big", "k", strings.Repeat("x", pipeline.MaxRecordBytes)))

	assert.Contains(t, msg, "test/x was not run")
}
