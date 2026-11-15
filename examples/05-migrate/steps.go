package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ruffel/pipeline"
)

// migrations are the schema changes, in the order they apply.
var migrations = []struct{ name, does string }{
	{"001-create-users", "Creates the users table"},
	{"002-add-email-index", "Adds an index on users.email"},
	{"003-drop-legacy-sessions", "Drops the legacy sessions table"},
}

// inputs are what the plan was built from. They are recorded in the journal,
// so a resume or a rollback builds the same plan without being told again.
// The password is the exception: a Secret is never stored, and has to be
// given on every attempt.
type inputs struct {
	Database string          `json:"database"`
	Target   int             `json:"target"`
	Password pipeline.Secret `json:"password"`
}

// faults are ways to make the demo misbehave.
type faults struct {
	// fail names a migration whose Run fails.
	fail string
	// pace is how long each migration takes.
	pace time.Duration
}

// newPlan builds the plan for in. Its shape depends on the target: a
// migration to version 2 has one step fewer than one to version 3, which is
// why the target is an input.
func newPlan(db *database, in inputs, f faults) (*pipeline.Plan, error) {
	return pipeline.NewPlan("migrate",
		preflight(db, in.Target),
		pipeline.Effect("backup", backup{db: db}).
			Description("Copies the database aside"),
		changeSchema(db, in.Target, f),
		pipeline.Observe("verify", verify{db: db, target: in.Target}).
			Description("Checks the schema is at the target version"),
	)
}

// preflight reads the database and changes nothing. A group of Observe steps
// runs on every attempt: what it read last time has gone stale.
func preflight(db *database, target int) pipeline.GroupNode {
	return pipeline.Group("preflight",
		pipeline.Observe("connect", pipeline.StepFunc(connect)).
			Description("Checks the database answers"),
		pipeline.Observe("pending", pending{db: db, target: target}).
			Description("Reports which migrations are still to apply"),
	).Description("Read-only checks")
}

// changeSchema applies the migrations up to target, in maintenance mode. If
// one fails, the group's handler lets the application write again.
func changeSchema(db *database, target int, f faults) pipeline.GroupNode {
	reopen := maintenance{db: db, on: false}

	// The migrations, between the two changes of mode.
	steps := make([]pipeline.Element, 0, target+2)

	// Ensure, not Effect: the mode can drift between attempts. When a
	// migration fails, the handler turns it off, so the attempt that resumes
	// must turn it on again.
	steps = append(steps, pipeline.Ensure("maintenance-on", maintenance{db: db, on: true}).
		Description("Stops the application writing while the schema changes"))

	for _, m := range migrations[:target] {
		steps = append(steps, pipeline.Effect(m.name, migration{
			db: db, name: m.name, fail: f.fail == m.name, pace: f.pace,
		}).Description(m.does))
	}

	steps = append(steps, pipeline.Effect("maintenance-off", reopen).
		Description("Lets the application write again"))

	return pipeline.Group("schema", steps...).
		Description("Changes the schema, in maintenance mode").
		OnFailure(pipeline.Effect("reopen", reopen).
			Description("Lets the application write again while the failure is looked at"))
}

// connect checks the database answers. It prints the password it was given,
// which no real step should: the terminal and the journal both show it
// masked, because the input is a Secret.
func connect(_ context.Context, sc *pipeline.StepContext) error {
	in, ok := sc.Inputs().(inputs)
	if !ok {
		return errors.New("the run was not given its inputs")
	}

	password := in.Password.Reveal()
	if password == "" {
		return errors.New("no password: give -password")
	}

	sc.Info(fmt.Sprintf("connected to %s as admin, password %s", filepath.Base(in.Database), password))

	return nil
}

// pending reports which migrations are still to apply.
type pending struct {
	db     *database
	target int
}

func (p pending) Run(_ context.Context, sc *pipeline.StepContext) error {
	s, err := p.db.load()
	if err != nil {
		return err
	}

	var todo []string

	for _, m := range migrations[:p.target] {
		if !slices.Contains(s.Applied, m.name) {
			todo = append(todo, m.name)
		}
	}

	if len(todo) == 0 {
		sc.Info("nothing to apply")

		return nil
	}

	sc.Info("to apply: " + strings.Join(todo, ", "))

	return nil
}

// backup copies the database aside before the schema changes.
//
// Its archive is named after the time, so only its Run knows the name. Run
// records it before writing anything: if the process dies part-way through
// the copy, the next attempt writes the same archive again, and Undo knows
// what to delete.
type backup struct{ db *database }

// Check reports whether the archive is already written.
func (b backup) Check(_ context.Context, sc *pipeline.StepContext) (bool, error) {
	archive, err := b.archive(sc)
	if err != nil || archive == "" {
		return false, err
	}

	_, err = os.Stat(archive)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}

	return err == nil, err
}

func (b backup) Run(_ context.Context, sc *pipeline.StepContext) error {
	archive, err := b.archive(sc)
	if err != nil {
		return err
	}

	if archive == "" {
		stamp := time.Now().UTC().Format("20060102T150405.000")
		archive = filepath.Join(filepath.Dir(b.db.file), "backup-"+stamp+".json")

		if err := sc.Record("archive", archive); err != nil {
			return err
		}
	}

	data, err := os.ReadFile(b.db.file)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	if err := writeFile(archive, data); err != nil {
		return err
	}

	sc.Info("database copied to " + filepath.Base(archive))

	return nil
}

// Undo deletes the archive. It copes with a Run that was interrupted: there
// may be a name recorded and no file, or nothing recorded at all.
func (b backup) Undo(_ context.Context, sc *pipeline.StepContext) error {
	archive, err := b.archive(sc)
	if err != nil || archive == "" {
		return err
	}

	if err := os.Remove(archive); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	sc.Info("deleted " + filepath.Base(archive))

	return nil
}

// archive returns the archive this step recorded, if it has.
func (backup) archive(sc *pipeline.StepContext) (string, error) {
	var archive string

	_, err := sc.Recorded(sc.Path(), "archive", &archive)

	return archive, err
}

// maintenance turns the database's maintenance mode on or off.
type maintenance struct {
	db *database
	on bool
}

func (m maintenance) Check(context.Context, *pipeline.StepContext) (bool, error) {
	s, err := m.db.load()

	return s.Maintenance == m.on, err
}

func (m maintenance) Run(context.Context, *pipeline.StepContext) error {
	return m.db.change(func(s *schema) { s.Maintenance = m.on })
}

// Undo puts the mode back as Run found it. A rollback undoes steps in
// reverse plan order, so it undoes maintenance-off first, which turns the
// mode on, then the migrations, then maintenance-on, which turns it off:
// the schema is rolled back in maintenance mode, with no reverse plan written.
func (m maintenance) Undo(context.Context, *pipeline.StepContext) error {
	return m.db.change(func(s *schema) { s.Maintenance = !m.on })
}

// migration applies one schema change.
type migration struct {
	db   *database
	name string
	fail bool
	pace time.Duration
}

// Check asks the database whether the migration is applied. It runs before
// every Run, so a migration applied by an attempt that then crashed, before
// the journal could say so, is not applied twice.
func (m migration) Check(context.Context, *pipeline.StepContext) (bool, error) {
	s, err := m.db.load()

	return slices.Contains(s.Applied, m.name), err
}

func (m migration) Run(ctx context.Context, sc *pipeline.StepContext) error {
	sc.Info("applying " + m.name)

	if err := sleep(ctx, m.pace); err != nil {
		return err
	}

	if m.fail {
		return errors.New("the database refused the change (-fail)")
	}

	return m.db.change(func(s *schema) {
		if !slices.Contains(s.Applied, m.name) {
			s.Applied = append(s.Applied, m.name)
		}
	})
}

// Undo reverts the migration, if it is applied.
func (m migration) Undo(_ context.Context, sc *pipeline.StepContext) error {
	sc.Info("reverting " + m.name)

	return m.db.change(func(s *schema) {
		s.Applied = slices.DeleteFunc(s.Applied, func(name string) bool { return name == m.name })
	})
}

// verify checks the schema is at the target version, and the database open.
type verify struct {
	db     *database
	target int
}

func (v verify) Run(_ context.Context, sc *pipeline.StepContext) error {
	s, err := v.db.load()
	if err != nil {
		return err
	}

	if len(s.Applied) != v.target {
		return fmt.Errorf("schema is at version %d, not %d", len(s.Applied), v.target)
	}

	if s.Maintenance {
		return errors.New("the database is still in maintenance mode")
	}

	sc.Info(fmt.Sprintf("schema at version %d", v.target))

	return nil
}

// sleep waits for d, or until ctx is done: a cancelled run cuts steps short.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
