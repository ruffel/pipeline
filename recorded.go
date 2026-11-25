package pipeline

import (
	"encoding/json"
	"maps"
	"strings"
	"sync"
)

// recorded is what an attempt knows of the values steps have recorded: the
// journal as the attempt found it, the values recorded since, and what the
// journal still holds once a recovery step has reset a group.
//
// Two questions have different answers after a reset. [StepContext.Recorded]
// still reads what the reset forgot, so that the recovery step can, and
// [StepContext.RecordOnce] goes by what the journal holds now. Between them,
// reset and standing say what prev and values alone do not.
type recorded struct {
	// prev is the journal as it stood when the attempt started.
	prev *State

	mu sync.Mutex
	// values are those recorded in this attempt, by path and key.
	values map[string]map[string]json.RawMessage
	// reset are the groups a recovery step has reset in this attempt, and
	// standing the values recorded in it that no reset has forgotten since,
	// both by path.
	reset    map[string]bool
	standing map[string]map[string]bool
}

// remember keeps a value recorded in this attempt.
func (r *recorded) remember(path, key string, raw json.RawMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.values == nil {
		r.values = map[string]map[string]json.RawMessage{}
	}

	if r.values[path] == nil {
		r.values[path] = map[string]json.RawMessage{}
	}

	r.values[path][key] = raw

	if r.standing == nil {
		r.standing = map[string]map[string]bool{}
	}

	if r.standing[path] == nil {
		r.standing[path] = map[string]bool{}
	}

	r.standing[path][key] = true
}

// forget notes that a recovery step has reset the group at path: the journal
// no longer holds the values recorded beneath it, in this attempt or before.
// recalled still returns them, so that the recovery step can read them.
func (r *recorded) forget(path string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.reset == nil {
		r.reset = map[string]bool{}
	}

	r.reset[path] = true

	maps.DeleteFunc(r.standing, func(p string, _ map[string]bool) bool { return strings.HasPrefix(p, path+"/") })
}

// holds reports whether the journal, as it is now, holds a value under path
// and key: one recorded in this attempt that no reset has forgotten since,
// or one from before it that no reset of this attempt has reached.
func (r *recorded) holds(path, key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.standing[path][key] {
		return true
	}

	for group := range r.reset {
		if strings.HasPrefix(path, group+"/") {
			return false
		}
	}

	_, ok := r.prev.Value(path, key)

	return ok
}

// recalled returns the latest value recorded under path and key: from this
// attempt if there is one, otherwise from the journal as it was at the start.
func (r *recorded) recalled(path, key string) (json.RawMessage, bool) {
	r.mu.Lock()
	raw, ok := r.values[path][key]
	r.mu.Unlock()

	if ok {
		return raw, true
	}

	return r.prev.Value(path, key)
}
