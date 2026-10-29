package actor

import (
	"sort"
	"sync"
)

// Registry is a node's leases in flight, by lease id: the one place the
// node keeps a lease while it runs (ADR-044: actors own everything, and
// the node holds no in-flight map of its own). Each entry is the lease's
// own record — under --actors it carries the lease's Actor — added when
// the lease is taken and removed by its actor's end, so nothing of a
// lease outlives it. Its lock is a leaf: no method calls out while
// holding it.
type Registry[T any] struct {
	mu sync.Mutex
	m  map[string]T
}

// Put adds the lease's entry.
func (r *Registry[T]) Put(id string, v T) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = map[string]T{}
	}
	r.m[id] = v
}

// Delete removes the lease's entry.
func (r *Registry[T]) Delete(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, id)
}

// Get is the lease's entry, and whether there is one.
func (r *Registry[T]) Get(id string) (T, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.m[id]
	return v, ok
}

// Len is how many leases are in flight.
func (r *Registry[T]) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.m)
}

// Entry is one lease in flight.
type Entry[T any] struct {
	ID    string
	Value T
}

// All is every lease in flight, by id.
func (r *Registry[T]) All() []Entry[T] {
	r.mu.Lock()
	out := make([]Entry[T], 0, len(r.m))
	for id, v := range r.m {
		out = append(out, Entry[T]{ID: id, Value: v})
	}
	r.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
