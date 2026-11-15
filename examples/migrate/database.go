package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// database is the mock database, kept in a file so it outlives the process:
// what a step's Check reads is the system, never the step's own memory.
type database struct{ file string }

type schema struct {
	// Applied names the migrations applied, in order.
	Applied []string `json:"applied,omitempty"`
	// Maintenance stops the application writing while the schema changes.
	Maintenance bool `json:"maintenance,omitempty"`
}

func (d *database) load() (schema, error) {
	b, err := os.ReadFile(d.file)
	if errors.Is(err, os.ErrNotExist) {
		return schema{}, nil
	}

	if err != nil {
		return schema{}, err
	}

	var s schema

	return s, json.Unmarshal(b, &s)
}

// change loads the schema, applies fn and saves the result.
func (d *database) change(fn func(*schema)) error {
	s, err := d.load()
	if err != nil {
		return err
	}

	fn(&s)

	b, err := json.Marshal(s)
	if err != nil {
		return err
	}

	return writeFile(d.file, b)
}

// writeFile replaces a file whole: it writes a temporary file beside it and
// renames it over the old one, so a crash leaves the old contents or the new,
// never a torn file that a Check can't read.
func writeFile(file string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(file), ".tmp-*")
	if err != nil {
		return err
	}

	defer func() { _ = os.Remove(tmp.Name()) }()

	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()

		return err
	}

	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()

		return err
	}

	if err := tmp.Close(); err != nil {
		return err
	}

	return os.Rename(tmp.Name(), file)
}
