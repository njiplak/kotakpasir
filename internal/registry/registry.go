package registry

import (
	"iter"
	"sync"
)

type Registry[T any] struct {
	mu    sync.RWMutex
	items map[string]T
}

func New[T any]() *Registry[T] {
	return &Registry[T]{items: make(map[string]T)}
}

func (r *Registry[T]) Put(id string, v T) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items[id] = v
}

func (r *Registry[T]) Get(id string) (T, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.items[id]
	return v, ok
}

func (r *Registry[T]) Delete(id string) (T, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.items[id]
	if ok {
		delete(r.items, id)
	}
	return v, ok
}

func (r *Registry[T]) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.items)
}

func (r *Registry[T]) All() iter.Seq2[string, T] {
	return func(yield func(string, T) bool) {
		r.mu.RLock()
		defer r.mu.RUnlock()
		for id, v := range r.items {
			if !yield(id, v) {
				return
			}
		}
	}
}
