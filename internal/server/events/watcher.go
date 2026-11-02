package events

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/hackerduck/duckway/internal/database/queries"
)

// DefaultInterval is the watcher poll period.
const DefaultInterval = 2 * time.Second

// Snapshotter produces change fingerprints (queries.WatchQueries).
type Snapshotter interface {
	Snapshot(ctx context.Context) (queries.WatchSnapshot, error)
}

// Publisher receives invalidation events (Hub).
type Publisher interface{ Publish(Event) }

// Watcher polls a Snapshotter and publishes an event per changed topic.
// The first successful snapshot is the baseline and publishes nothing.
type Watcher struct {
	Src      Snapshotter
	Pub      Publisher
	Interval time.Duration
	// Tick, if set, replaces the real ticker (tests). It returns the tick
	// channel and a stop func.
	Tick func(d time.Duration) (<-chan time.Time, func())
}

func (w *Watcher) ticker() (<-chan time.Time, func()) {
	d := w.Interval
	if d <= 0 {
		d = DefaultInterval
	}
	if w.Tick != nil {
		return w.Tick(d)
	}
	t := time.NewTicker(d)
	return t.C, t.Stop
}

// Run blocks until ctx is done.
func (w *Watcher) Run(ctx context.Context) {
	ch, stop := w.ticker()
	defer stop()
	var prev queries.WatchSnapshot
	have := false
	lastLog := time.Time{}
	for {
		snap, err := w.Src.Snapshot(ctx)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return
			}
			if time.Since(lastLog) > time.Minute {
				log.Printf("[events] watcher snapshot failed: %v", err)
				lastLog = time.Now()
			}
		case !have:
			prev, have = snap, true
		default:
			w.diff(prev, snap)
			prev = snap
		}
		select {
		case <-ctx.Done():
			return
		case <-ch:
		}
	}
}

func jsonOf(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func shortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:4])
}

func (w *Watcher) diff(a, b queries.WatchSnapshot) {
	if a.LogMaxID != b.LogMaxID {
		w.Pub.Publish(Event{Topic: TopicLogs, Data: jsonOf(map[string]int64{"max_id": b.LogMaxID})})
	}
	if a.ApprovalsSig != b.ApprovalsSig {
		w.Pub.Publish(Event{Topic: TopicApprovals, Data: jsonOf(map[string]int64{"pending": b.ApprovalsPend, "total": b.ApprovalsTotal})})
	}
	if a.ApprovalsPend != b.ApprovalsPend {
		w.Pub.Publish(Event{Topic: TopicBadge, Data: jsonOf(map[string]int64{"pending": b.ApprovalsPend})})
	}
	if a.RolloutsSig != b.RolloutsSig {
		w.Pub.Publish(Event{Topic: TopicRollouts, Data: jsonOf(map[string]string{"rev": shortHash(b.RolloutsSig)})})
	}
}

// Service couples a Hub with a Watcher that runs only while the hub has
// subscribers.
type Service struct {
	Hub *Hub

	mu      sync.Mutex
	src     Snapshotter
	interv  time.Duration
	tick    func(time.Duration) (<-chan time.Time, func())
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	stopped bool
}

// ServiceOptions configures NewService.
type ServiceOptions struct {
	Limits   Limits
	Interval time.Duration
	Tick     func(time.Duration) (<-chan time.Time, func())
}

// NewService builds the hub and lazy watcher.
func NewService(src Snapshotter, o ServiceOptions) *Service {
	s := &Service{src: src, interv: o.Interval, tick: o.Tick}
	s.Hub = NewHub(o.Limits, func(bool) { s.reconcile() })
	return s
}

func (s *Service) reconcile() {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := !s.stopped && s.Hub.Subscribers() > 0
	switch {
	case want && s.cancel == nil:
		ctx, cancel := context.WithCancel(context.Background())
		s.cancel = cancel
		w := &Watcher{Src: s.src, Pub: s.Hub, Interval: s.interv, Tick: s.tick}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			w.Run(ctx)
		}()
	case !want && s.cancel != nil:
		s.cancel()
		s.cancel = nil
	}
}

// Running reports whether a watcher goroutine is active.
func (s *Service) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cancel != nil
}

// Stop closes the hub, stops the watcher and waits for it.
func (s *Service) Stop() {
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
	s.Hub.Close()
	s.reconcile()
	s.wg.Wait()
}
