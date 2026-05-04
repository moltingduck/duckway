package services

import (
	"encoding/json"
	"sync"
)

// CCEventHub is an in-process pub/sub fan-out used to push Discord events
// to connected `duckway cc watch` daemons (one per client). The gateway
// publishes after writing to discord_inbox; SSE handlers subscribe by
// client_id and stream events as they arrive.
//
// We keep the inbox for cold-start replay and history — the hub is just
// the "live tail" path.
type CCEventHub struct {
	mu   sync.RWMutex
	subs map[string][]chan CCEvent // clientID → list of subscriber channels
}

// CCEvent is the shape pushed to subscribers and serialized as SSE data.
type CCEvent struct {
	Type    string          `json:"type"`              // message_create, message_update, message_delete, channel_delete, channel_update
	CCID    string          `json:"cc_id"`
	Handle  string          `json:"channel_handle"`     // empty if channel not in our cache
	Payload json.RawMessage `json:"payload,omitempty"`  // raw Discord dispatch
	InboxID int64           `json:"inbox_id,omitempty"` // 0 when the event was not appended (e.g. channel_delete)
}

func NewCCEventHub() *CCEventHub {
	return &CCEventHub{subs: map[string][]chan CCEvent{}}
}

// Subscribe registers a buffered channel for one clientID and returns it
// plus an unsubscribe function. The buffer is generous (32) so a slow
// daemon doesn't drop events from a burst — but we still drop on overflow
// rather than block the publisher (gateway must not stall).
func (h *CCEventHub) Subscribe(clientID string) (<-chan CCEvent, func()) {
	ch := make(chan CCEvent, 32)
	h.mu.Lock()
	h.subs[clientID] = append(h.subs[clientID], ch)
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		list := h.subs[clientID]
		for i, c := range list {
			if c == ch {
				h.subs[clientID] = append(list[:i], list[i+1:]...)
				close(ch)
				if len(h.subs[clientID]) == 0 {
					delete(h.subs, clientID)
				}
				return
			}
		}
	}
}

// Publish best-effort fans an event out to every subscriber for clientID.
// Non-blocking: if a subscriber's buffer is full, the event for that
// subscriber is dropped — they can still poll the inbox to recover.
func (h *CCEventHub) Publish(clientID string, ev CCEvent) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, ch := range h.subs[clientID] {
		select {
		case ch <- ev:
		default:
			// Drop. Daemon should fall back to polling the inbox after
			// reconnecting if it suspects gaps.
		}
	}
}

// SubscriberCount is for tests + admin diagnostics.
func (h *CCEventHub) SubscriberCount(clientID string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs[clientID])
}
