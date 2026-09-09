package ducklord

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

var (
	ErrOutputPoolClosed = errors.New("raw output subscription pool is closed")
	ErrHandoffBusy      = errors.New("raw output subscription handoff is already in progress")
	ErrStaleOutputLease = errors.New("raw output subscription lease is stale")
)

// OutputKey is the stable identity of one runtime output stream. Handles and
// client display names are deliberately excluded: neither is an authority
// boundary and both may be duplicated or renamed.
type OutputKey struct {
	ClientKey         string
	InstanceID        string
	SessionID         string
	RuntimeGeneration uint64
}

func (k OutputKey) validate() error {
	if k.ClientKey == "" || k.InstanceID == "" || k.SessionID == "" || k.RuntimeGeneration == 0 {
		return fmt.Errorf("raw output key requires client, instance, session, and runtime generation")
	}
	return nil
}

// OutputResource is prepared outside the pool lock. Open must not return until
// its initial authoritative, gap-free snapshot is ready for display.
type OutputResource interface {
	Snapshot(context.Context) error
	Close() error
}

type OutputOpenFunc func(context.Context, OutputKey, uint64) (OutputResource, error)

type OutputActivation struct {
	Key      OutputKey
	Lease    uint64
	Resource OutputResource
	Existing bool
	Evicted  *OutputKey
}

type OutputPoolStatus struct {
	Capacity int
	Count    int
	Active   *OutputKey
	Desired  []OutputKey
	Handoff  bool
	Closed   bool
}

type outputPoolEntry struct {
	lease    uint64
	resource OutputResource
}

// OutputPool owns process-wide desired raw subscriptions and their display LRU.
// Activate is transactional: when full it permits exactly one temporary +1
// resource, and does not alter active/LRU membership until preparation and the
// victim snapshot both succeed.
type OutputPool struct {
	mu            sync.Mutex
	capacity      int
	entries       map[OutputKey]outputPoolEntry
	lru           []OutputKey // oldest first
	active        *OutputKey
	nextLease     uint64
	handoff       bool
	handoffDone   chan struct{}
	handoffCancel context.CancelFunc
	closed        bool
	closeDone     chan struct{}
}

func NewOutputPool(capacity int) (*OutputPool, error) {
	if capacity < 1 || capacity > 100 {
		return nil, fmt.Errorf("raw output subscription limit must be between 1 and 100")
	}
	return &OutputPool{capacity: capacity, entries: make(map[OutputKey]outputPoolEntry), closeDone: make(chan struct{})}, nil
}

func (p *OutputPool) Activate(ctx context.Context, key OutputKey, open OutputOpenFunc) (OutputActivation, error) {
	if err := key.validate(); err != nil {
		return OutputActivation{}, err
	}
	if open == nil {
		return OutputActivation{}, fmt.Errorf("raw output opener is required")
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return OutputActivation{}, ErrOutputPoolClosed
	}
	if p.handoff {
		p.mu.Unlock()
		return OutputActivation{}, ErrHandoffBusy
	}
	if entry, ok := p.entries[key]; ok {
		p.setActiveLocked(key)
		p.mu.Unlock()
		return OutputActivation{Key: key, Lease: entry.lease, Resource: entry.resource, Existing: true}, nil
	}
	p.handoff = true
	openCtx, cancel := context.WithCancel(ctx)
	p.handoffDone = make(chan struct{})
	p.handoffCancel = cancel
	p.nextLease++
	lease := p.nextLease
	p.mu.Unlock()

	resource, err := open(openCtx, key, lease)
	if err != nil {
		p.finishFailedHandoff()
		return OutputActivation{}, err
	}
	if resource == nil {
		p.finishFailedHandoff()
		return OutputActivation{}, fmt.Errorf("raw output opener returned no resource")
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = resource.Close()
		p.finishFailedHandoff()
		return OutputActivation{}, ErrOutputPoolClosed
	}
	var victim OutputKey
	haveVictim := len(p.entries) >= p.capacity
	if haveVictim {
		victim = p.oldestEvictionCandidateLocked()
		if victim == (OutputKey{}) {
			p.finishHandoffLocked()
			p.mu.Unlock()
			_ = resource.Close()
			return OutputActivation{}, fmt.Errorf("raw output pool has no evictable subscription")
		}
	}
	var victimResource OutputResource
	if haveVictim {
		victimResource = p.entries[victim].resource
	}
	p.mu.Unlock()

	// Saving can perform filesystem I/O. Keep the handoff reservation set but do
	// not hold the coordinator lock, so status readers and Close remain live.
	if haveVictim {
		if err := victimResource.Snapshot(openCtx); err != nil {
			_ = resource.Close()
			p.finishFailedHandoff()
			return OutputActivation{}, fmt.Errorf("snapshot raw output eviction candidate: %w", err)
		}
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = resource.Close()
		p.finishFailedHandoff()
		return OutputActivation{}, ErrOutputPoolClosed
	}
	p.entries[key] = outputPoolEntry{lease: lease, resource: resource}
	p.setActiveLocked(key)
	if haveVictim {
		delete(p.entries, victim)
		p.removeLRULocked(victim)
	}
	p.finishHandoffLocked()
	p.mu.Unlock()
	if haveVictim {
		_ = victimResource.Close()
		copy := victim
		return OutputActivation{Key: key, Lease: lease, Resource: resource, Evicted: &copy}, nil
	}
	return OutputActivation{Key: key, Lease: lease, Resource: resource}, nil
}

func (p *OutputPool) finishFailedHandoff() {
	p.mu.Lock()
	p.finishHandoffLocked()
	p.mu.Unlock()
}

func (p *OutputPool) finishHandoffLocked() {
	if !p.handoff {
		return
	}
	p.handoff = false
	if p.handoffCancel != nil {
		p.handoffCancel()
		p.handoffCancel = nil
	}
	if p.handoffDone != nil {
		close(p.handoffDone)
		p.handoffDone = nil
	}
}

// WithLease applies a reader update only while the exact stream incarnation is
// still installed. Delayed frames/completions from a pre-reconnect reader are
// rejected instead of mutating its replacement.
func (p *OutputPool) WithLease(key OutputKey, lease uint64, apply func(OutputResource)) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok := p.entries[key]
	if !ok || entry.lease != lease {
		return ErrStaleOutputLease
	}
	if apply != nil {
		apply(entry.resource)
	}
	return nil
}

func (p *OutputPool) Status() OutputPoolStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	status := OutputPoolStatus{Capacity: p.capacity, Count: len(p.entries), Handoff: p.handoff, Closed: p.closed}
	if p.active != nil {
		copy := *p.active
		status.Active = &copy
	}
	status.Desired = append([]OutputKey(nil), p.lru...)
	return status
}

func (p *OutputPool) Close() error {
	p.mu.Lock()
	if p.closed {
		done := p.closeDone
		p.mu.Unlock()
		<-done
		return nil
	}
	p.closed = true
	done := p.handoffDone
	cancel := p.handoffCancel
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	p.mu.Lock()
	resources := make([]OutputResource, 0, len(p.entries))
	for _, entry := range p.entries {
		resources = append(resources, entry.resource)
	}
	p.entries = make(map[OutputKey]outputPoolEntry)
	p.lru = nil
	p.active = nil
	p.mu.Unlock()
	var joined error
	for _, resource := range resources {
		if err := resource.Close(); err != nil {
			joined = errors.Join(joined, err)
		}
	}
	close(p.closeDone)
	return joined
}

func (p *OutputPool) setActiveLocked(key OutputKey) {
	p.removeLRULocked(key)
	p.lru = append(p.lru, key)
	copy := key
	p.active = &copy
}

func (p *OutputPool) removeLRULocked(key OutputKey) {
	for index := range p.lru {
		if p.lru[index] == key {
			p.lru = append(p.lru[:index], p.lru[index+1:]...)
			return
		}
	}
}

func (p *OutputPool) oldestEvictionCandidateLocked() OutputKey {
	if len(p.lru) > 0 {
		// The current active remains pinned throughout preparation. Once the
		// destination commits it becomes active and the former active is a valid
		// victim (notably when capacity is one).
		return p.lru[0]
	}
	return OutputKey{}
}
