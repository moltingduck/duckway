// Package events implements the admin live-update stream: an in-process
// pub/sub hub, a database change watcher and the GET /api/events SSE handler.
// Events are invalidation hints only (topic plus small counters); pages
// refetch through the normal authenticated APIs.
package events

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
)

// Topics carried by the stream.
const (
	TopicApprovals = "approvals"
	TopicLogs      = "logs"
	TopicRollouts  = "rollouts"
	TopicBadge     = "badge"
)

// Topics lists every topic in a stable order.
var Topics = []string{TopicApprovals, TopicLogs, TopicRollouts, TopicBadge}

var (
	ErrTooManySubscribers = errors.New("events: too many subscribers")
	ErrSessionLimit       = errors.New("events: too many streams for this session")
	ErrUserLimit          = errors.New("events: too many streams for this user")
	ErrNoSession          = errors.New("events: missing session key")
	ErrClosed             = errors.New("events: hub closed")
)

// Event is one invalidation hint. Data is a small JSON object.
type Event struct {
	Topic string
	Data  json.RawMessage
}

// Limits bounds hub resources.
type Limits struct {
	MaxSubscribers int // global
	MaxPerSession  int
	MaxPerUser     int // per authenticated username; sessions of one user share it
}

// DefaultLimits are used when a Hub is created with zero Limits.
var DefaultLimits = Limits{MaxSubscribers: 64, MaxPerSession: 4, MaxPerUser: 8}

// Hub fans events out to subscribers. Publishing never blocks: each
// subscriber keeps at most one pending event per topic (latest wins), so a
// slow consumer coalesces instead of growing a queue.
type Hub struct {
	mu        sync.Mutex
	limits    Limits
	subs      map[*Subscription]struct{}
	bySession map[string]int
	byUser    map[string]int
	closed    bool
	onActive  func(active bool)
}

// NewHub creates a hub. onActive (optional) is invoked with true when the
// subscriber count goes 0->1 and false when it returns to 0. Calls are made
// outside the hub lock and may interleave, so callbacks must reconcile
// against Subscribers() rather than trust the argument.
func NewHub(l Limits, onActive func(bool)) *Hub {
	if l.MaxSubscribers <= 0 {
		l.MaxSubscribers = DefaultLimits.MaxSubscribers
	}
	if l.MaxPerSession <= 0 {
		l.MaxPerSession = DefaultLimits.MaxPerSession
	}
	if l.MaxPerUser <= 0 {
		l.MaxPerUser = DefaultLimits.MaxPerUser
	}
	return &Hub{limits: l, subs: map[*Subscription]struct{}{}, bySession: map[string]int{}, byUser: map[string]int{}, onActive: onActive}
}

// Subscription receives coalesced events. Use Next to read.
type Subscription struct {
	hub     *Hub
	session string
	user    string
	notify  chan struct{} // cap 1
	done    chan struct{}
	once    sync.Once

	mu      sync.Mutex
	pending map[string]Event // at most one per topic
	order   []string
}

// Subscribe registers a subscriber for session (an opaque non-empty key)
// with no user identity.
func (h *Hub) Subscribe(session string) (*Subscription, error) {
	return h.SubscribeUser(session, "")
}

// SubscribeUser registers a subscriber for session and, when user is not
// empty, also counts it against the per-user limit.
func (h *Hub) SubscribeUser(session, user string) (*Subscription, error) {
	if session == "" {
		return nil, ErrNoSession
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, ErrClosed
	}
	if len(h.subs) >= h.limits.MaxSubscribers {
		h.mu.Unlock()
		return nil, ErrTooManySubscribers
	}
	if h.bySession[session] >= h.limits.MaxPerSession {
		h.mu.Unlock()
		return nil, ErrSessionLimit
	}
	if user != "" && h.byUser[user] >= h.limits.MaxPerUser {
		h.mu.Unlock()
		return nil, ErrUserLimit
	}
	s := &Subscription{hub: h, session: session, user: user, notify: make(chan struct{}, 1), done: make(chan struct{}), pending: map[string]Event{}}
	h.subs[s] = struct{}{}
	h.bySession[session]++
	if user != "" {
		h.byUser[user]++
	}
	first := len(h.subs) == 1
	cb := h.onActive
	h.mu.Unlock()
	if first && cb != nil {
		cb(true)
	}
	return s, nil
}

// Publish delivers ev to every subscriber without blocking.
func (h *Hub) Publish(ev Event) {
	h.mu.Lock()
	subs := make([]*Subscription, 0, len(h.subs))
	for s := range h.subs {
		subs = append(subs, s)
	}
	h.mu.Unlock()
	for _, s := range subs {
		s.push(ev)
	}
}

// Subscribers reports the current subscriber count.
func (h *Hub) Subscribers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// Close ends every subscription and rejects new ones.
func (h *Hub) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	subs := make([]*Subscription, 0, len(h.subs))
	for s := range h.subs {
		subs = append(subs, s)
	}
	h.mu.Unlock()
	for _, s := range subs {
		s.Close()
	}
}

// Stop implements the server's stopper interface.
func (h *Hub) Stop() { h.Close() }

func (s *Subscription) push(ev Event) {
	s.mu.Lock()
	if _, ok := s.pending[ev.Topic]; !ok {
		s.order = append(s.order, ev.Topic)
	}
	s.pending[ev.Topic] = ev
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// Next blocks until events are pending (returned coalesced, in first-seen
// topic order), the subscription is closed (ok=false) or ctx is done.
func (s *Subscription) Next(ctx context.Context) ([]Event, bool) {
	for {
		if evs := s.take(); len(evs) > 0 {
			return evs, true
		}
		select {
		case <-s.notify:
		case <-s.done:
			return s.take(), false
		case <-ctx.Done():
			return nil, false
		}
	}
}

func (s *Subscription) take() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.order) == 0 {
		return nil
	}
	out := make([]Event, 0, len(s.order))
	for _, t := range s.order {
		out = append(out, s.pending[t])
	}
	s.order = s.order[:0]
	s.pending = map[string]Event{}
	return out
}

// Done is closed when the subscription ends.
func (s *Subscription) Done() <-chan struct{} { return s.done }

// Close unsubscribes; safe to call more than once.
func (s *Subscription) Close() {
	s.once.Do(func() {
		close(s.done)
		h := s.hub
		h.mu.Lock()
		delete(h.subs, s)
		if h.bySession[s.session] <= 1 {
			delete(h.bySession, s.session)
		} else {
			h.bySession[s.session]--
		}
		if s.user != "" {
			if h.byUser[s.user] <= 1 {
				delete(h.byUser, s.user)
			} else {
				h.byUser[s.user]--
			}
		}
		last := len(h.subs) == 0 && !h.closed
		cb := h.onActive
		h.mu.Unlock()
		if last && cb != nil {
			cb(false)
		}
	})
}
