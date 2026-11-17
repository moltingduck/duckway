package services

import (
	"sync"
	"time"
)

// chatRefTTL bounds how long a message reference is kept. A decision made by
// another process never reaches this one, so an undecided entry would
// otherwise live forever.
const chatRefTTL = 48 * time.Hour

// ttlMap is a mutex-guarded map whose entries expire after ttl. Expired entries
// are dropped lazily by every operation (at most one sweep per minute), so it
// needs no janitor goroutine to stop.
type ttlMap[V any] struct {
	mu        sync.Mutex
	m         map[string]ttlEntry[V]
	ttl       time.Duration
	now       func() time.Time
	lastSweep time.Time
}

type ttlEntry[V any] struct {
	v  V
	at time.Time
}

func (t *ttlMap[V]) init() {
	if t.m == nil {
		t.m = map[string]ttlEntry[V]{}
		if t.ttl == 0 {
			t.ttl = chatRefTTL
		}
		if t.now == nil {
			t.now = time.Now
		}
	}
}

// sweep must be called with mu held.
func (t *ttlMap[V]) sweep() {
	now := t.now()
	if now.Sub(t.lastSweep) < time.Minute {
		return
	}
	t.lastSweep = now
	for k, e := range t.m {
		if now.Sub(e.at) > t.ttl {
			delete(t.m, k)
		}
	}
}

func (t *ttlMap[V]) Store(k string, v V) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.init()
	t.sweep()
	t.m[k] = ttlEntry[V]{v, t.now()}
}

// Update replaces the value of k with fn(old, found), keeping the entry fresh.
func (t *ttlMap[V]) Update(k string, fn func(old V, ok bool) V) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.init()
	t.sweep()
	e, ok := t.m[k]
	if ok && t.now().Sub(e.at) > t.ttl {
		ok = false
	}
	t.m[k] = ttlEntry[V]{fn(e.v, ok), t.now()}
}

func (t *ttlMap[V]) Load(k string) (V, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.init()
	t.sweep()
	e, ok := t.m[k]
	if ok && t.now().Sub(e.at) > t.ttl {
		delete(t.m, k)
		ok = false
	}
	return e.v, ok
}

func (t *ttlMap[V]) Delete(k string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.init()
	delete(t.m, k)
}

func (t *ttlMap[V]) LoadAndDelete(k string) (V, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.init()
	e, ok := t.m[k]
	delete(t.m, k)
	if ok && t.now().Sub(e.at) > t.ttl {
		ok = false
	}
	return e.v, ok
}

func (t *ttlMap[V]) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.init()
	return len(t.m)
}
