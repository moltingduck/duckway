package ducklord

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
)

var (
	ErrOutputPoolClosed    = errors.New("raw output subscription pool is closed")
	ErrHandoffBusy         = errors.New("raw output subscription handoff is already in progress")
	ErrRestoreBusy         = errors.New("raw output subscription restore is already in progress")
	ErrOutputNotDesired    = errors.New("raw output subscription is not desired")
	ErrStaleOutputLease    = errors.New("raw output subscription lease is stale")
	ErrOutputResourceOwned = errors.New("raw output resource is already owned by the pool")
)

// Ducklion admits at most eight observer connections for one Ducklord owner
// on a host. Pooled streams use isolated observer connections so canceling one
// replay cannot tear down unrelated subscriptions.
const maxStableOutputObserversPerDaemon = 7

// OutputKey identifies one logical PTY. Runtime and bridge generations are
// stream revisions, not identity, so a restarted session cannot occupy two
// desired pool entries.
type OutputKey struct {
	ClientKey  string
	InstanceID string
	SessionID  string
}

type OutputRevision struct {
	RuntimeGeneration uint64
}

func (r OutputRevision) validate() error {
	if r.RuntimeGeneration == 0 {
		return fmt.Errorf("raw output revision requires runtime generation")
	}
	return nil
}

func (k OutputKey) validate() error {
	if k.ClientKey == "" || k.InstanceID == "" || k.SessionID == "" {
		return fmt.Errorf("raw output key requires client, instance, and session")
	}
	return nil
}

type outputHost struct{ clientKey, instanceID string }

type outputDaemon struct{ instanceID string }

func (k OutputKey) host() outputHost     { return outputHost{k.ClientKey, k.InstanceID} }
func (k OutputKey) daemon() outputDaemon { return outputDaemon{k.InstanceID} }

// OutputResource owns one connection-scoped stream. Snapshot must atomically
// serialize parsed render state. The pool quiesces WithLease callbacks before
// Snapshot and closes the stream before accepting any later callback.
type OutputResource interface {
	Snapshot(context.Context) error
	Close() error
	BindOutputLease(*OutputPool, OutputKey, uint64) error
	ValidateOutputCommit() error
}

type OutputOpenFunc func(context.Context, OutputKey, OutputRevision, uint64) (OutputResource, error)

type OutputActivation struct {
	Key                OutputKey
	Revision           OutputRevision
	Lease              uint64
	Existing           bool
	Evicted            *OutputKey
	EvictionCloseError error
}

type OutputPoolStatus struct {
	Capacity  int
	Count     int
	Connected int
	Active    *OutputKey
	Desired   []OutputKey
	Handoff   bool
	Closed    bool
}

type outputPoolEntry struct {
	key           OutputKey
	lease         uint64
	revision      OutputRevision
	resource      OutputResource
	useMu         sync.RWMutex
	evicting      bool
	restoring     bool
	restoreEpoch  uint64
	restoreCancel context.CancelFunc
}

type outputHandoff struct {
	id          uint64
	key         OutputKey
	lease       uint64
	epoch       uint64
	revision    OutputRevision
	victimHost  *outputHost
	victimKey   *OutputKey
	victimEpoch uint64
	cancel      context.CancelFunc
	done        chan struct{}
}

// OutputPool owns process-wide desired subscriptions and display LRU. Slow
// network/filesystem work happens outside the coordinator lock and is fenced by
// unique reservations plus a per-host reconnect epoch.
type OutputPool struct {
	mu        sync.Mutex
	capacity  int
	entries   map[OutputKey]*outputPoolEntry
	lru       []OutputKey // oldest first
	active    *OutputKey
	nextLease uint64
	nextID    uint64
	hostEpoch map[outputHost]uint64
	retired   map[outputHost]bool
	handoff   *outputHandoff
	closed    bool
	workers   sync.WaitGroup
	closeDone chan struct{}
	closeErr  error
}

func NewOutputPool(capacity int) (*OutputPool, error) {
	if capacity < 1 || capacity > 100 {
		return nil, fmt.Errorf("raw output subscription limit must be between 1 and 100")
	}
	return &OutputPool{capacity: capacity, entries: make(map[OutputKey]*outputPoolEntry), hostEpoch: make(map[outputHost]uint64), retired: make(map[outputHost]bool), closeDone: make(chan struct{})}, nil
}

// Activate changes the displayed PTY only after destination preparation and,
// when full, a quiesced victim snapshot both succeed. At most one preparation
// may temporarily exceed capacity.
func (p *OutputPool) Activate(ctx context.Context, key OutputKey, revision OutputRevision, open OutputOpenFunc) (OutputActivation, error) {
	return p.activate(ctx, key, revision, open, true, false, false)
}

// ReplaceDesired prepares a new stream revision for an existing background
// member without stealing active selection or changing LRU order.
func (p *OutputPool) ReplaceDesired(ctx context.Context, key OutputKey, revision OutputRevision, open OutputOpenFunc) (OutputActivation, error) {
	return p.activate(ctx, key, revision, open, false, true, false)
}

// ReconnectDesired atomically replaces an existing desired stream even when
// its runtime generation is unchanged. The selected session remains active.
func (p *OutputPool) ReconnectDesired(ctx context.Context, key OutputKey, revision OutputRevision, open OutputOpenFunc) (OutputActivation, error) {
	return p.activate(ctx, key, revision, open, true, false, true)
}

func (p *OutputPool) activate(ctx context.Context, key OutputKey, revision OutputRevision, open OutputOpenFunc, makeActive, requireDesired, forceReplace bool) (OutputActivation, error) {
	if err := key.validate(); err != nil {
		return OutputActivation{}, err
	}
	if err := revision.validate(); err != nil {
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
	if p.retired[key.host()] {
		p.mu.Unlock()
		return OutputActivation{}, ErrStaleOutputLease
	}
	if p.handoff != nil {
		p.mu.Unlock()
		return OutputActivation{}, ErrHandoffBusy
	}
	if requireDesired && p.entries[key] == nil {
		p.mu.Unlock()
		return OutputActivation{}, ErrOutputNotDesired
	}
	if entry := p.entries[key]; entry != nil && entry.restoring {
		p.mu.Unlock()
		return OutputActivation{}, ErrRestoreBusy
	}
	if entry := p.entries[key]; entry != nil && entry.resource != nil && entry.revision == revision && !entry.evicting && !forceReplace {
		if makeActive {
			p.setActiveLocked(key)
		}
		result := OutputActivation{Key: key, Revision: revision, Lease: entry.lease, Existing: true}
		p.mu.Unlock()
		return result, nil
	}
	p.nextLease++
	p.nextID++
	openCtx, cancel := context.WithCancel(ctx)
	handoff := &outputHandoff{id: p.nextID, key: key, revision: revision, lease: p.nextLease, epoch: p.hostEpoch[key.host()], cancel: cancel, done: make(chan struct{})}
	p.handoff = handoff
	p.workers.Add(1)
	p.mu.Unlock()
	defer p.workers.Done()

	resource, err := open(openCtx, key, revision, handoff.lease)
	if err != nil {
		p.finishHandoff(handoff)
		return OutputActivation{}, err
	}
	if resource == nil {
		p.finishHandoff(handoff)
		return OutputActivation{}, fmt.Errorf("raw output opener returned no resource")
	}

	p.mu.Lock()
	if openCtx.Err() != nil || !p.handoffCurrentLocked(handoff) {
		p.clearHandoffLocked(handoff)
		p.mu.Unlock()
		return OutputActivation{}, closeOutputAbort(ctx, resource, ErrStaleOutputLease)
	}
	if err := resource.ValidateOutputCommit(); err != nil {
		p.clearHandoffLocked(handoff)
		p.mu.Unlock()
		return OutputActivation{}, errors.Join(err, resource.Close())
	}
	existing := p.entries[key]
	newMembership := existing == nil
	var victim *outputPoolEntry
	var replacement *outputPoolEntry
	if existing != nil && existing.resource != nil {
		replacement = existing
		replacement.evicting = true
	}
	if newMembership {
		if p.membersForDaemonLocked(key.daemon()) >= maxStableOutputObserversPerDaemon {
			victim = p.oldestEvictionCandidateForDaemonLocked(key.daemon())
		} else if len(p.entries) >= p.capacity {
			victim = p.oldestEvictionCandidateLocked()
		}
	}
	if newMembership && (len(p.entries) >= p.capacity || p.membersForDaemonLocked(key.daemon()) >= maxStableOutputObserversPerDaemon) {
		if victim == nil {
			p.clearHandoffLocked(handoff)
			p.mu.Unlock()
			_ = resource.Close()
			return OutputActivation{}, fmt.Errorf("raw output pool has no evictable subscription")
		}
		victim.evicting = true
		victimHost := victim.key.host()
		handoff.victimHost = &victimHost
		victimKey := victim.key
		handoff.victimKey = &victimKey
		handoff.victimEpoch = p.hostEpoch[victimHost]
	}
	p.mu.Unlock()

	if victim != nil {
		victim.useMu.Lock()
		if err := victim.resource.Snapshot(openCtx); err != nil {
			victim.useMu.Unlock()
			p.mu.Lock()
			if p.entries[victim.key] == victim {
				victim.evicting = false
			}
			p.clearHandoffLocked(handoff)
			p.mu.Unlock()
			_ = resource.Close()
			return OutputActivation{}, fmt.Errorf("snapshot raw output eviction candidate: %w", err)
		}
		victim.useMu.Unlock()
	}
	var replacementResource OutputResource
	if replacement != nil {
		replacement.useMu.Lock()
		replacementResource = replacement.resource
		replacement.useMu.Unlock()
	}

	p.mu.Lock()
	if openCtx.Err() != nil || !p.handoffCurrentLocked(handoff) {
		if victim != nil && p.entries[victim.key] == victim {
			victim.evicting = false
		}
		if replacement != nil && p.entries[replacement.key] == replacement {
			replacement.evicting = false
		}
		p.clearHandoffLocked(handoff)
		p.mu.Unlock()
		return OutputActivation{}, closeOutputAbort(ctx, resource, ErrStaleOutputLease)
	}
	oldResource := replacementResource
	if oldResource == nil && existing != nil {
		oldResource = existing.resource
	}
	if p.resourceOwnedLocked(resource) {
		if victim != nil && p.entries[victim.key] == victim {
			victim.evicting = false
		}
		if replacement != nil && p.entries[replacement.key] == replacement {
			replacement.evicting = false
		}
		p.clearHandoffLocked(handoff)
		p.mu.Unlock()
		return OutputActivation{}, ErrOutputResourceOwned
	}
	if err := resource.BindOutputLease(p, key, handoff.lease); err != nil {
		if victim != nil && p.entries[victim.key] == victim {
			victim.evicting = false
		}
		if replacement != nil && p.entries[replacement.key] == replacement {
			replacement.evicting = false
		}
		p.clearHandoffLocked(handoff)
		p.mu.Unlock()
		return OutputActivation{}, errors.Join(err, resource.Close())
	}
	if err := resource.ValidateOutputCommit(); err != nil {
		if victim != nil && p.entries[victim.key] == victim {
			victim.evicting = false
		}
		if replacement != nil && p.entries[replacement.key] == replacement {
			replacement.evicting = false
		}
		p.clearHandoffLocked(handoff)
		p.mu.Unlock()
		return OutputActivation{}, errors.Join(err, resource.Close())
	}
	entry := existing
	if entry == nil {
		entry = &outputPoolEntry{key: key}
		p.entries[key] = entry
	}
	entry.lease = handoff.lease
	entry.revision = revision
	entry.resource = resource
	entry.restoring = false
	entry.evicting = false
	if makeActive {
		p.setActiveLocked(key)
	}
	var evicted *OutputKey
	var victimResource OutputResource
	if victim != nil {
		delete(p.entries, victim.key)
		p.removeLRULocked(victim.key)
		copy := victim.key
		evicted = &copy
		victimResource = victim.resource
	}
	p.clearHandoffLocked(handoff)
	committedLease := entry.lease
	p.mu.Unlock()
	var closeErr error
	if victimResource != nil {
		closeErr = victimResource.Close()
	}
	if oldResource != nil && oldResource != victimResource {
		closeErr = errors.Join(closeErr, oldResource.Close())
	}
	return OutputActivation{Key: key, Revision: revision, Lease: committedLease, Evicted: evicted, EvictionCloseError: closeErr}, nil
}

func (p *OutputPool) handoffCurrentLocked(h *outputHandoff) bool {
	return !p.closed && p.handoff == h && p.hostEpoch[h.key.host()] == h.epoch &&
		(h.victimHost == nil || p.hostEpoch[*h.victimHost] == h.victimEpoch)
}

func (p *OutputPool) clearHandoffLocked(h *outputHandoff) {
	if p.handoff == h {
		p.handoff = nil
		h.cancel()
		close(h.done)
	}
}

func (p *OutputPool) finishHandoff(h *outputHandoff) {
	p.mu.Lock()
	p.clearHandoffLocked(h)
	p.mu.Unlock()
}

// WithLease runs one reader-state mutation outside the coordinator lock.
// Eviction takes the entry write lock first, so every accepted update is either
// included in the saved snapshot or rejected before removal.
func (p *OutputPool) WithLease(key OutputKey, lease uint64, apply func(OutputResource)) error {
	p.mu.Lock()
	entry := p.entries[key]
	if entry == nil || entry.lease != lease || entry.resource == nil || entry.evicting || entry.restoring {
		p.mu.Unlock()
		return ErrStaleOutputLease
	}
	p.mu.Unlock()
	entry.useMu.Lock()
	defer entry.useMu.Unlock()
	p.mu.Lock()
	if p.entries[key] != entry || entry.lease != lease || entry.resource == nil || entry.evicting || entry.restoring {
		p.mu.Unlock()
		return ErrStaleOutputLease
	}
	resource := entry.resource
	p.mu.Unlock()
	if apply != nil {
		apply(resource)
	}
	return nil
}

// TerminalView copies a framebuffer only while the requested lease is still
// current. Callers must not retain a resource pointer across an eviction or a
// runtime-generation replacement.
func (p *OutputPool) TerminalView(key OutputKey, lease uint64) (PooledTerminalView, error) {
	var view PooledTerminalView
	var viewErr error
	leaseErr := p.WithLease(key, lease, func(resource OutputResource) {
		terminal, ok := resource.(*PooledTerminal)
		if !ok {
			viewErr = fmt.Errorf("raw output resource is not a pooled terminal")
			return
		}
		view = terminal.View()
	})
	return view, errors.Join(leaseErr, viewErr)
}

// TerminalSignals returns read-only lifecycle signals for the exact current
// lease. Receiving a stale signal is harmless because View fences the lease.
func (p *OutputPool) TerminalSignals(key OutputKey, lease uint64) (<-chan struct{}, <-chan PooledTerminalView, <-chan struct{}, error) {
	var updates, done <-chan struct{}
	var final <-chan PooledTerminalView
	var viewErr error
	leaseErr := p.WithLease(key, lease, func(resource OutputResource) {
		terminal, ok := resource.(*PooledTerminal)
		if !ok {
			viewErr = fmt.Errorf("raw output resource is not a pooled terminal")
			return
		}
		updates, final, done = terminal.Updates(), terminal.finalViews, terminal.done
	})
	return updates, final, done, errors.Join(leaseErr, viewErr)
}

// ResizeTerminalAt invokes the authoritative remote resize while output frame
// application is quiesced, then schedules the framebuffer dimension change at
// the returned byte barrier before readers may continue.
func (p *OutputPool) ResizeTerminalAt(key OutputKey, lease uint64, rows, cols uint16, resize func(uint16, uint16) (uint64, error)) (uint64, error) {
	if resize == nil {
		return 0, fmt.Errorf("PTY resize function is required")
	}
	var barrier uint64
	var resizeErr error
	leaseErr := p.WithLease(key, lease, func(resource OutputResource) {
		terminal, ok := resource.(*PooledTerminal)
		if !ok {
			resizeErr = fmt.Errorf("raw output resource is not a pooled terminal")
			return
		}
		barrier, resizeErr = resize(rows, cols)
		if resizeErr == nil {
			resizeErr = terminal.scheduleResize(barrier, int(rows), int(cols))
		}
	})
	return barrier, errors.Join(leaseErr, resizeErr)
}

// DisconnectLease conditionally detaches a naturally ended background
// resource while preserving desired membership, active selection, and LRU for
// reconnect. Stale readers and resources already being evicted are ignored.
func (p *OutputPool) DisconnectLease(key OutputKey, lease uint64) bool {
	p.mu.Lock()
	entry := p.entries[key]
	if entry == nil || entry.lease != lease || entry.resource == nil || entry.evicting || entry.restoring {
		p.mu.Unlock()
		return false
	}
	entry.evicting = true
	p.mu.Unlock()
	entry.useMu.Lock()
	defer entry.useMu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.entries[key] != entry || entry.lease != lease || entry.resource == nil || !entry.evicting || entry.restoring {
		return false
	}
	p.nextLease++
	entry.lease = p.nextLease
	entry.resource = nil
	entry.evicting = false
	return true
}

// DisconnectHost preserves desired membership, active selection and LRU while
// invalidating all connection-scoped resources and reader leases.
func (p *OutputPool) DisconnectHost(clientKey, instanceID string) error {
	host := outputHost{clientKey, instanceID}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return ErrOutputPoolClosed
	}
	p.hostEpoch[host]++
	var handoffDone chan struct{}
	if p.handoff != nil && (p.handoff.key.host() == host || p.handoff.victimHost != nil && *p.handoff.victimHost == host) {
		p.handoff.cancel()
		handoffDone = p.handoff.done
	}
	if handoffDone != nil {
		p.mu.Unlock()
		<-handoffDone
		return p.DisconnectHost(clientKey, instanceID)
	}
	var detached []*outputPoolEntry
	for _, entry := range p.entries {
		if entry.key.host() != host {
			continue
		}
		if entry.evicting {
			continue
		}
		if entry.restoreCancel != nil {
			entry.restoreCancel()
			entry.restoreCancel = nil
		}
		entry.restoring = false
		entry.evicting = true
		p.nextLease++
		entry.lease = p.nextLease
		if entry.resource != nil {
			detached = append(detached, entry)
		}
	}
	p.mu.Unlock()
	var result error
	for _, entry := range detached {
		entry.useMu.Lock()
		p.mu.Lock()
		resource := entry.resource
		entry.resource = nil
		p.mu.Unlock()
		entry.useMu.Unlock()
		if err := resource.Close(); err != nil {
			result = errors.Join(result, err)
		}
	}
	p.mu.Lock()
	for _, entry := range p.entries {
		if entry.key.host() == host {
			entry.evicting = false
		}
	}
	p.mu.Unlock()
	return result
}

// RestoreDesired rebuilds a disconnected member without changing LRU or active
// selection. Late open results are closed unless host epoch and lease match.
func (p *OutputPool) RestoreDesired(ctx context.Context, key OutputKey, revision OutputRevision, open OutputOpenFunc) (OutputActivation, error) {
	if err := key.validate(); err != nil {
		return OutputActivation{}, err
	}
	if err := revision.validate(); err != nil {
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
	entry := p.entries[key]
	if entry == nil {
		p.mu.Unlock()
		return OutputActivation{}, ErrOutputNotDesired
	}
	if entry.resource != nil && entry.revision == revision {
		result := OutputActivation{Key: key, Revision: revision, Lease: entry.lease, Existing: true}
		p.mu.Unlock()
		return result, nil
	}
	if entry.resource != nil {
		p.mu.Unlock()
		return OutputActivation{}, fmt.Errorf("desired output runtime generation changed; activate replacement first")
	}
	if entry.restoring {
		p.mu.Unlock()
		return OutputActivation{}, ErrRestoreBusy
	}
	p.nextLease++
	lease := p.nextLease
	epoch := p.hostEpoch[key.host()]
	openCtx, cancel := context.WithCancel(ctx)
	entry.lease = lease
	entry.revision = revision
	entry.restoring = true
	entry.restoreEpoch = epoch
	entry.restoreCancel = cancel
	p.workers.Add(1)
	p.mu.Unlock()
	defer p.workers.Done()

	resource, err := open(openCtx, key, revision, lease)
	if err != nil {
		p.finishRestore(entry, lease)
		return OutputActivation{}, err
	}
	if resource == nil {
		p.finishRestore(entry, lease)
		return OutputActivation{}, fmt.Errorf("raw output opener returned no resource")
	}
	p.mu.Lock()
	current := p.entries[key]
	if openCtx.Err() != nil || p.closed || current != entry || !entry.restoring || entry.lease != lease || entry.restoreEpoch != epoch || p.hostEpoch[key.host()] != epoch {
		p.mu.Unlock()
		p.finishRestore(entry, lease)
		return OutputActivation{}, closeOutputAbort(ctx, resource, ErrStaleOutputLease)
	}
	if err := resource.ValidateOutputCommit(); err != nil {
		p.mu.Unlock()
		p.finishRestore(entry, lease)
		return OutputActivation{}, errors.Join(err, resource.Close())
	}
	if p.resourceOwnedLocked(resource) {
		p.mu.Unlock()
		p.finishRestore(entry, lease)
		return OutputActivation{}, ErrOutputResourceOwned
	}
	if err := resource.BindOutputLease(p, key, lease); err != nil {
		p.mu.Unlock()
		p.finishRestore(entry, lease)
		return OutputActivation{}, errors.Join(err, resource.Close())
	}
	if err := resource.ValidateOutputCommit(); err != nil {
		p.mu.Unlock()
		p.finishRestore(entry, lease)
		return OutputActivation{}, errors.Join(err, resource.Close())
	}
	entry.resource = resource
	entry.restoring = false
	entry.restoreCancel = nil
	cancel()
	p.mu.Unlock()
	return OutputActivation{Key: key, Revision: revision, Lease: lease}, nil
}

func (p *OutputPool) resourceOwnedLocked(resource OutputResource) bool {
	typeOf := reflect.TypeOf(resource)
	if typeOf == nil || !typeOf.Comparable() {
		return false
	}
	for _, entry := range p.entries {
		if entry.resource == resource {
			return true
		}
	}
	return false
}

func closeOutputAbort(ctx context.Context, resource OutputResource, fallback error) error {
	primary := fallback
	if err := ctx.Err(); err != nil {
		primary = err
	}
	return errors.Join(primary, resource.Close())
}

func (p *OutputPool) finishRestore(entry *outputPoolEntry, lease uint64) {
	p.mu.Lock()
	var cancel context.CancelFunc
	if current := p.entries[entry.key]; current == entry && entry.restoring && entry.lease == lease {
		entry.restoring = false
		cancel = entry.restoreCancel
		entry.restoreCancel = nil
	}
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (p *OutputPool) DesiredForHost(clientKey, instanceID string) []OutputKey {
	host := outputHost{clientKey, instanceID}
	p.mu.Lock()
	defer p.mu.Unlock()
	result := make([]OutputKey, 0)
	if p.active != nil && p.active.host() == host {
		result = append(result, *p.active)
	}
	for index := len(p.lru) - 1; index >= 0; index-- {
		key := p.lru[index]
		if key.host() == host && (p.active == nil || key != *p.active) {
			result = append(result, key)
		}
	}
	return result
}

// RemoveHostDesired forgets every subscription belonging to an authoritative
// daemon instance replacement. A new Ducklion must not revive old leases.
func (p *OutputPool) ForgetHost(clientKey, instanceID string) error {
	host := outputHost{clientKey, instanceID}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return ErrOutputPoolClosed
	}
	p.retired[host] = true
	p.mu.Unlock()
	// This advances the pool host epoch and cancels an opener that captured the
	// old epoch before the instance was retired.
	if err := p.DisconnectHost(clientKey, instanceID); err != nil {
		return err
	}
	for {
		keys := p.DesiredForHost(clientKey, instanceID)
		if len(keys) == 0 {
			return nil
		}
		for _, key := range keys {
			if err := p.RemoveDesired(key); err != nil {
				return err
			}
		}
	}
}

// RemoveDesired applies an authoritative session removal. A matching in-flight
// handoff is canceled first; the installed reader is fenced before it is
// quiesced and closed.
func (p *OutputPool) RemoveDesired(key OutputKey) error {
	if err := key.validate(); err != nil {
		return err
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return ErrOutputPoolClosed
	}
	if p.handoff != nil && (p.handoff.key == key || p.handoff.victimKey != nil && *p.handoff.victimKey == key) {
		done := p.handoff.done
		p.handoff.cancel()
		p.mu.Unlock()
		<-done
		return p.RemoveDesired(key)
	}
	entry := p.entries[key]
	if entry == nil {
		p.mu.Unlock()
		return nil
	}
	entry.evicting = true
	if entry.restoreCancel != nil {
		entry.restoreCancel()
	}
	p.nextLease++
	entry.lease = p.nextLease
	delete(p.entries, key)
	p.removeLRULocked(key)
	if p.active != nil && *p.active == key {
		p.active = nil
	}
	p.mu.Unlock()
	entry.useMu.Lock()
	resource := entry.resource
	entry.resource = nil
	entry.useMu.Unlock()
	if resource != nil {
		return resource.Close()
	}
	return nil
}

func (p *OutputPool) Status() OutputPoolStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	status := OutputPoolStatus{Capacity: p.capacity, Count: len(p.entries), Handoff: p.handoff != nil, Closed: p.closed}
	if p.active != nil {
		copy := *p.active
		status.Active = &copy
	}
	status.Desired = append([]OutputKey(nil), p.lru...)
	for _, entry := range p.entries {
		if entry.resource != nil {
			status.Connected++
		}
	}
	return status
}

func (p *OutputPool) Close() error {
	p.mu.Lock()
	if p.closed {
		done := p.closeDone
		p.mu.Unlock()
		<-done
		p.mu.Lock()
		err := p.closeErr
		p.mu.Unlock()
		return err
	}
	p.closed = true
	if p.handoff != nil {
		p.handoff.cancel()
	}
	entries := make([]*outputPoolEntry, 0, len(p.entries))
	for _, entry := range p.entries {
		if entry.restoreCancel != nil {
			entry.restoreCancel()
		}
		entry.evicting = true
		entries = append(entries, entry)
	}
	p.mu.Unlock()
	var result error
	for _, entry := range entries {
		entry.useMu.Lock()
		p.mu.Lock()
		resource := entry.resource
		entry.resource = nil
		p.mu.Unlock()
		entry.useMu.Unlock()
		if resource != nil {
			if err := resource.Close(); err != nil {
				result = errors.Join(result, err)
			}
		}
	}
	p.workers.Wait()
	p.mu.Lock()
	p.closeErr = result
	p.mu.Unlock()
	close(p.closeDone)
	return result
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

func (p *OutputPool) oldestEvictionCandidateLocked() *outputPoolEntry {
	if len(p.lru) == 0 {
		return nil
	}
	return p.entries[p.lru[0]]
}

func (p *OutputPool) membersForDaemonLocked(daemon outputDaemon) int {
	count := 0
	for key := range p.entries {
		if key.daemon() == daemon {
			count++
		}
	}
	return count
}

func (p *OutputPool) oldestEvictionCandidateForDaemonLocked(daemon outputDaemon) *outputPoolEntry {
	for _, key := range p.lru {
		if key.daemon() != daemon {
			continue
		}
		if entry := p.entries[key]; entry != nil && !entry.evicting && !entry.restoring {
			return entry
		}
	}
	return nil
}
