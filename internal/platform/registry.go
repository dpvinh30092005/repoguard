package platform

import (
	"sort"
	"sync"
)

// Registry holds the active client per platform. Clients can be swapped at
// runtime (e.g. after credentials change in the web UI) while jobs run.
type Registry struct {
	mu sync.RWMutex
	m  map[Kind]Client
}

func NewRegistry(clients ...Client) *Registry {
	r := &Registry{m: map[Kind]Client{}}
	for _, c := range clients {
		r.m[c.Kind()] = c
	}
	return r
}

func (r *Registry) Get(k Kind) (Client, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.m[k]
	return c, ok
}

func (r *Registry) Set(c Client) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[c.Kind()] = c
}

func (r *Registry) Remove(k Kind) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, k)
}

// All returns the clients in a stable order.
func (r *Registry) All() []Client {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Client, 0, len(r.m))
	for _, c := range r.m {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind() < out[j].Kind() })
	return out
}
