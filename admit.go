package pipeline

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// applyPlanChange decides whether an attempt may be made with plan against a
// journal that stands at prev, and applies the change to prev if the plan
// has a different shape and how allows it. It returns the change, for the
// attempt to record, or nil if the shape is the same.
func applyPlanChange(prev *State, plan *Plan, how PlanChange) (*planDiff, error) {
	if len(prev.Attempts) == 0 || prev.Fingerprint == plan.Fingerprint() {
		return nil, nil //nolint:nilnil // No change is not an error.
	}

	if how != MatchByPath || prev.Plan == nil {
		return nil, ErrPlanChanged
	}

	d := diffPlans(*prev.Plan, plan.Spec())

	// A path that an earlier change removed still has its record, which a
	// plan that puts it back carries on from: its kind must not differ
	// either.
	rekinded := d.rekinded

	for _, n := range d.added {
		if slices.ContainsFunc(prev.Removed, func(was NodeSpec) bool { return was.Path == n.Path && was.Kind != n.Kind }) {
			rekinded = append(rekinded, n.Path)
		}
	}

	if len(rekinded) > 0 {
		return nil, fmt.Errorf("%w: the kind of %s differs, so its record can't be carried over",
			ErrPlanChanged, strings.Join(rekinded, ", "))
	}

	// What replay does with the attempt's run.started, done now: resume and
	// rollback decide from prev before that event is written.
	prev.changePlan(d)

	return &d, nil
}

// changedInputsAccepted decides whether an attempt may be made with inputs
// in against a journal that stands at prev. If they differ from the latest
// attempt's and the change is accepted, it returns the pointers of the
// fields that changed, for the attempt to record.
func changedInputsAccepted(prev *State, plan *Plan, in *inputs, cfg config) ([]string, error) {
	if err := checkConsumers(plan, cfg.inputConsumers); err != nil {
		return nil, err
	}

	if len(prev.Attempts) == 0 || prev.InputsDigest == in.digest {
		return nil, nil
	}

	decide := cfg.inputChange
	if cfg.inputConsumers != nil {
		decide = append([]func(InputChange) error{consumersHook(cfg.inputConsumers)}, decide...)
	}

	if len(decide) == 0 && !cfg.newInputs {
		return nil, ErrInputsChanged
	}

	changed, err := changedInputs(prev.Inputs, in.encoded)
	if err != nil {
		return nil, err
	}

	if cfg.newInputs {
		return changed, nil
	}

	change := InputChange{Old: prev.Inputs, New: in.encoded, Changed: changed, State: prev}

	for _, fn := range decide {
		if err := fn(change); err != nil {
			if errors.Is(err, ErrInputsChanged) {
				return nil, err
			}

			return nil, fmt.Errorf("%w: %w", ErrInputsChanged, err)
		}
	}

	return changed, nil
}

// admit checks that an attempt may be made against a journal that stands at
// prev, and returns what it decides which work is already done from: prev,
// or, for a forced run, nothing, though earlier values stay readable. The
// children of a group that has settled its order must still be those the
// order names.
func admit(prev *State, plan *Plan, cfg config, rollback bool) (*State, error) {
	prior, err := admitted(prev, plan, cfg, rollback)
	if err != nil {
		return nil, err
	}

	if err := misordered(prior, plan); err != nil {
		return nil, err
	}

	return prior, nil
}

// admitted is admit, before the plan is held against the orders that the
// journal's groups have settled.
func admitted(prev *State, plan *Plan, cfg config, rollback bool) (*State, error) {
	n := len(prev.Attempts)
	if n == 0 {
		return prev, nil
	}

	last := prev.Attempts[n-1]

	switch {
	case rollback:
		// A rollback undoes what the journal says ran, however the latest
		// attempt ended.
		return prev, canUndo(prev, plan, cfg.strictUndo)
	case last.Rollback && last.Outcome != OutcomePassed:
		return nil, ErrRollbackUnfinished
	case cfg.force:
		return newState(), nil
	case !last.Rollback && last.Outcome == OutcomePassed:
		return nil, ErrAlreadyComplete
	default:
		return prev, nil
	}
}

// misordered returns why plan can't carry on from the orders that the
// journal's groups have settled, or nil: the first group whose children are
// not those its order names, or that would now run them at once. An order
// stands until its group goes back to never started, whether or not the plan
// still asks for one. Only a later build can bring either about: the order
// named every child of a sequential group when it was settled.
func misordered(prior *State, plan *Plan) error {
	var err error

	plan.root.walk(func(n *node) {
		names, settled := prior.orders[n.path]
		if err != nil || !settled {
			return
		}

		switch _, bad := n.arranged(names); {
		case bad != nil:
			err = fmt.Errorf("%w: the children of %s are no longer those its order names", ErrPlanChanged, n.path)
		case n.parallel:
			err = fmt.Errorf("%w: %s has settled the order of its children, and may not now be parallel", ErrPlanChanged, n.path)
		}
	})

	return err
}
