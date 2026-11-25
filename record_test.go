package pipeline_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ruffel/pipeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// prior is a step that records, once, the version it finds, as a step does
// with what was there before it changed anything. It notes what each call
// reported, fails while fails is set, and can be undone.
type prior struct {
	version  *string
	fails    *bool
	recorded *[]bool
}

func (p prior) Run(_ context.Context, s *pipeline.StepContext) error {
	recorded, err := s.RecordOnce("prior", *p.version)
	if err != nil {
		return err
	}

	*p.recorded = append(*p.recorded, recorded)

	if *p.fails {
		return errors.New("boom")
	}

	return nil
}

func (prior) Undo(context.Context, *pipeline.StepContext) error { return nil }

func valueOf(t *testing.T, j *pipeline.Journal, path, key string) string {
	t.Helper()

	raw, ok := stateOf(t, j).Value(path, key)
	require.True(t, ok)

	return string(raw)
}

// RecordOnce keeps the first value a step sees under a key: a later call,
// try or attempt records nothing, and the value stands.
func TestRecordOnce_KeepsTheFirstValue(t *testing.T) {
	t.Parallel()

	var recorded []bool

	version, fails := "1.0", true
	plan := mustPlan(t, pipeline.Effect("a", prior{version: &version, fails: &fails, recorded: &recorded}).Retry(2, 0))
	j := pipeline.MemoryJournal()

	out := resumeOn(t, j, plan)
	require.ErrorContains(t, out.err, "boom")
	assert.Equal(t, []bool{true, false}, recorded, "the first try records, and the second finds it there")

	version, fails = "2.0", false
	out = resumeOn(t, j, plan)

	require.NoError(t, out.err)
	assert.Equal(t, []bool{true, false, false}, recorded, "a later attempt finds it there too")
	assert.Empty(t, kinds(out.events, pipeline.EventValue))
	assert.Equal(t, `"1.0"`, valueOf(t, j, "p/a", "prior"), "what was there before the first attempt changed it")
}

// The check and the record are one step: of many goroutines of a step that
// call it with one key, exactly one records.
func TestRecordOnce_OneOfManyGoroutinesRecords(t *testing.T) {
	t.Parallel()

	const callers = 32

	var recorded atomic.Int32

	out := runPlan(t, t.Context(), mustPlan(t, pipeline.Effect("a", pipeline.StepFunc(
		func(_ context.Context, s *pipeline.StepContext) error {
			var (
				wg   sync.WaitGroup
				errs = make([]error, callers)
			)

			for i := range callers {
				wg.Go(func() {
					var ok bool

					if ok, errs[i] = s.RecordOnce("prior", i); ok {
						recorded.Add(1)
					}
				})
			}

			wg.Wait()

			return errors.Join(errs...)
		}))))

	require.NoError(t, out.err)
	assert.Equal(t, int32(1), recorded.Load())
	assert.Len(t, kinds(out.events, pipeline.EventValue), 1)
}

// A value that is not recorded takes nothing from the step's allowance, and
// one that would go past it is refused, as Record refuses it.
func TestRecordOnce_CountsOnlyWhatItRecords(t *testing.T) {
	t.Parallel()

	half := strings.Repeat("x", pipeline.MaxRecordBytes/2)

	var over error

	out := runPlan(t, t.Context(), mustPlan(t, pipeline.Effect("a", pipeline.StepFunc(
		func(_ context.Context, s *pipeline.StepContext) error {
			if recorded, err := s.RecordOnce("prior", half); err != nil || !recorded {
				return errors.Join(err, errors.New("the first was not recorded"))
			}

			for range 8 {
				if recorded, err := s.RecordOnce("prior", half); err != nil || recorded {
					return errors.Join(err, errors.New("recorded again"))
				}
			}

			if err := s.Record("other", strings.Repeat("x", pipeline.MaxRecordBytes/4)); err != nil {
				return err
			}

			_, over = s.RecordOnce("more", half)

			return nil
		}))))

	require.NoError(t, out.err)
	require.ErrorContains(t, over, "would exceed")
	assert.Len(t, kinds(out.events, pipeline.EventValue), 2)

	_, ok := out.state.Value("p/a", "more")
	assert.False(t, ok, "a value that is refused is not recorded")
}

// A recorded value lasts until its step is undone or its group is reset,
// and through a forced attempt. RecordOnce records again once it is gone.
func TestRecordOnce_RecordsAgainOnceTheValueIsForgotten(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		// fails is whether the first attempt fails, between what comes
		// between the two attempts, and opts what the second is given.
		fails   bool
		recover bool
		between func(t *testing.T, j *pipeline.Journal, plan *pipeline.Plan)
		opts    []pipeline.Option
		again   bool
	}{
		"a recovery step reset its group": {fails: true, recover: true, again: true},
		"a rollback undid the step": {
			again: true,
			between: func(t *testing.T, j *pipeline.Journal, plan *pipeline.Plan) {
				t.Helper()

				require.NoError(t, resumeRollback(t, j, plan).err)
			},
		},
		"a failure left it where it was": {fails: true, again: false},
		"a forced attempt keeps recorded values": {
			opts: []pipeline.Option{pipeline.Force()}, again: false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var recorded []bool

			version, fails := "1.0", tc.fails
			group := pipeline.Group("g", pipeline.Effect("a", prior{version: &version, fails: &fails, recorded: &recorded}))

			if tc.recover {
				group = group.RecoverOnFailure(pipeline.Effect("restore", ok()))
			}

			plan, j := mustPlan(t, group), pipeline.MemoryJournal()

			require.Equal(t, tc.fails, resumeOn(t, j, plan).err != nil)

			if tc.between != nil {
				tc.between(t, j, plan)
			}

			version, fails = "2.0", false

			require.NoError(t, resumeOn(t, j, plan, tc.opts...).err)
			assert.Equal(t, []bool{true, tc.again}, recorded)

			want := `"1.0"`
			if tc.again {
				want = `"2.0"`
			}

			assert.Equal(t, want, valueOf(t, j, "p/g/a", "prior"))
		})
	}
}

// RecordOnce refuses what Record refuses.
func TestRecordOnce_Refusals(t *testing.T) {
	t.Parallel()

	var (
		kept     *pipeline.StepContext
		empty    error
		unstored error
	)

	out := runPlan(t, t.Context(), mustPlan(t, pipeline.Effect("a", pipeline.StepFunc(
		func(_ context.Context, s *pipeline.StepContext) error {
			kept = s
			_, empty = s.RecordOnce("", 1)
			_, unstored = s.RecordOnce("fn", func() {})

			return nil
		}))))

	require.NoError(t, out.err)
	require.ErrorContains(t, empty, "record key is empty")
	require.ErrorContains(t, unstored, `record "fn"`)
	assert.Empty(t, kinds(out.events, pipeline.EventValue))

	_, err := kept.RecordOnce("late", 1)
	require.ErrorIs(t, err, pipeline.ErrStepReturned)

	var zero pipeline.StepContext

	_, err = zero.RecordOnce("k", 1)
	require.ErrorIs(t, err, pipeline.ErrStepReturned)
	require.ErrorIs(t, zero.Record("k", 1), pipeline.ErrStepReturned)
}

// Values is every key a step has recorded, each with its latest value.
func TestState_ValuesAreEveryKeyOfAStep(t *testing.T) {
	t.Parallel()

	out := runPlan(t, t.Context(), mustPlan(t, pipeline.Effect("a", pipeline.StepFunc(
		func(_ context.Context, s *pipeline.StepContext) error {
			return errors.Join(s.Record("count", 1), s.Record("name", "node7"), s.Record("count", 2))
		}))))

	require.NoError(t, out.err)

	values := out.state.Values("p/a")
	require.Len(t, values, 2)
	assert.JSONEq(t, `2`, string(values["count"]))
	assert.JSONEq(t, `"node7"`, string(values["name"]))

	// The map is the caller's own.
	delete(values, "count")
	assert.Len(t, out.state.Values("p/a"), 2)

	assert.NotNil(t, out.state.Values("p/nowhere"))
	assert.Empty(t, out.state.Values("p/nowhere"))
}

// A recovery step's reset forgets what was recorded beneath its group, the
// recovery step's own values of an earlier attempt among them. RecordOnce
// goes by what the journal holds now, so a recovery step that runs again
// records again, where Recorded still reads what the attempt started with.
func TestRecordOnce_SeesAResetMadeInThisAttempt(t *testing.T) {
	t.Parallel()

	type call struct {
		before   string
		found    bool
		recorded []bool
	}

	var calls []call

	attempt := 0
	restore := pipeline.Effect("restore", pipeline.StepFunc(func(_ context.Context, s *pipeline.StepContext) error {
		var c call

		found, err := s.Recorded(s.Path(), "restored", &c.before)
		if err != nil {
			return err
		}

		c.found = found

		for range 2 {
			recorded, err := s.RecordOnce("restored", fmt.Sprintf("attempt %d", attempt))
			if err != nil {
				return err
			}

			c.recorded = append(c.recorded, recorded)
		}

		calls = append(calls, c)

		return nil
	}))
	plan := mustPlan(t, pipeline.Group("node", pipeline.Effect("a", fail("boom"))).RecoverOnFailure(restore))
	j := pipeline.MemoryJournal()

	for attempt = 1; attempt <= 3; attempt++ {
		require.ErrorContains(t, resumeOn(t, j, plan).err, "boom")
		assert.Equal(t, fmt.Sprintf(`"attempt %d"`, attempt), valueOf(t, j, "p/node/restore", "restored"),
			"the journal holds what this attempt's recovery recorded")
	}

	assert.Equal(t, []call{
		{recorded: []bool{true, false}},
		{before: "attempt 1", found: true, recorded: []bool{true, false}},
		{before: "attempt 2", found: true, recorded: []bool{true, false}},
	}, calls)
}
