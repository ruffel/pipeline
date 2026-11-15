package main

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ruffel/pipeline"
	"github.com/ruffel/pipeline/pipelinetest"
)

func newDatabase(t *testing.T) *database {
	t.Helper()

	return &database{file: filepath.Join(t.TempDir(), "database.json")}
}

func applied(t *testing.T, db *database) []string {
	t.Helper()

	s, err := db.load()
	if err != nil {
		t.Fatal(err)
	}

	return s.Applied
}

func backups(t *testing.T, db *database) []string {
	t.Helper()

	found, err := filepath.Glob(filepath.Join(filepath.Dir(db.file), "backup-*.json"))
	if err != nil {
		t.Fatal(err)
	}

	return found
}

// A real process can die after Run changed the database and before the
// journal said so. CheckIdempotent crashes the step after every event it
// writes, resumes it, and expects it to pass; the cleanup then checks the
// migration was applied exactly once.
func TestMigrationSurvivesACrash(t *testing.T) {
	t.Parallel()

	const name = "001-create-users"

	pipelinetest.CheckIdempotent(t, func(t *testing.T) *database {
		t.Helper()

		db := newDatabase(t)

		t.Cleanup(func() {
			if got := applied(t, db); !slices.Equal(got, []string{name}) {
				t.Errorf("applied %v, want only %s", got, name)
			}
		})

		return db
	}, func(db *database) pipeline.StepNode {
		return pipeline.Effect(name, migration{db: db, name: name})
	})
}

// The backup records its archive's name before writing it, so a crash
// between the two must not leave a second archive behind.
func TestBackupSurvivesACrash(t *testing.T) {
	t.Parallel()

	pipelinetest.CheckIdempotent(t, func(t *testing.T) *database {
		t.Helper()

		db := newDatabase(t)

		t.Cleanup(func() {
			if got := backups(t, db); len(got) != 1 {
				t.Errorf("found archives %v, want one", got)
			}
		})

		return db
	}, func(db *database) pipeline.StepNode {
		return pipeline.Effect("backup", backup{db: db})
	})
}

// A migration that fails leaves the run to be rolled back. The rollback
// needs no plan of its own: it undoes what the journal says was done.
func TestFailedMigrationRollsBack(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := newDatabase(t)
	in := inputs{Database: db.file, Target: 3, Password: pipeline.NewSecret("for-tests")}
	journal := pipeline.MemoryJournal()

	failing, err := newPlan(db, in, faults{fail: "002-add-email-index"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := pipeline.Run(ctx, failing, journal, pipeline.WithInputs(in)); err == nil {
		t.Fatal("the run passed, and 002 should have failed")
	}

	if got := applied(t, db); !slices.Equal(got, []string{"001-create-users"}) {
		t.Fatalf("after the failure, applied %v", got)
	}

	// The failure handler has let the application write again.
	if s, _ := db.load(); s.Maintenance {
		t.Fatal("after the failure, the database is still in maintenance mode")
	}

	// Rebuilt as a new process would build it: same inputs, same shape.
	plan, err := newPlan(db, in, faults{})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := pipeline.Rollback(ctx, plan, journal, pipeline.WithInputs(in)); err != nil {
		t.Fatal(err)
	}

	if got := applied(t, db); len(got) != 0 {
		t.Errorf("after the rollback, applied %v", got)
	}

	if got := backups(t, db); len(got) != 0 {
		t.Errorf("after the rollback, archives %v are left", got)
	}

	if s, _ := db.load(); s.Maintenance {
		t.Error("after the rollback, the database is in maintenance mode")
	}
}

// A crash while a migration runs: the next attempt carries on, and applies
// nothing twice.
func TestCrashMidMigrationResumes(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := newDatabase(t)
	in := inputs{Database: db.file, Target: 3, Password: pipeline.NewSecret("for-tests")}

	plan, err := newPlan(db, in, faults{})
	if err != nil {
		t.Fatal(err)
	}

	crash := pipelinetest.CrashAfter("migrate/schema/002-add-email-index", pipelinetest.Running)

	_, err = pipeline.Run(ctx, plan, crash.Journal, pipeline.WithInputs(in))
	if !errors.Is(err, pipelinetest.ErrCrashed) {
		t.Fatalf("the run did not crash: %v", err)
	}

	crash.Restart()

	if _, err := pipeline.Run(ctx, plan, crash.Journal, pipeline.WithInputs(in)); err != nil {
		t.Fatal(err)
	}

	want := []string{"001-create-users", "002-add-email-index", "003-drop-legacy-sessions"}
	if got := applied(t, db); !slices.Equal(got, want) {
		t.Errorf("applied %v, want %v", got, want)
	}
}
