package runtime

import (
	"errors"
	"sync"
)

var ErrOffsetAhead = errors.New("output offset is ahead of runtime")
var ErrOutputClosed = errors.New("output hub is closed")

type OutputFrame struct {
	Offset uint64 `json:"offset"`
	Data   []byte `json:"data,omitempty"`
	Gap    bool   `json:"gap,omitempty"`
}

// OutputHub is a bounded in-memory replay ring with non-blocking subscribers.
// A slow subscriber is closed instead of blocking PTY capture.
type OutputHub struct {
	mu          sync.Mutex
	capacity    int
	data        []byte
	startOffset uint64
	endOffset   uint64
	nextID      uint64
	closed      bool
	subscribers map[uint64]chan OutputFrame
}

func NewOutputHub(capacity int) *OutputHub {
	if capacity <= 0 {
		capacity = 4 << 20
	}
	return &OutputHub{capacity: capacity, subscribers: make(map[uint64]chan OutputFrame)}
}

func (h *OutputHub) Publish(data []byte) OutputFrame {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return OutputFrame{Offset: h.endOffset}
	}
	frame := OutputFrame{Offset: h.endOffset, Data: append([]byte(nil), data...)}
	h.endOffset += uint64(len(data))
	h.data = append(h.data, data...)
	if len(h.data) > h.capacity {
		drop := len(h.data) - h.capacity
		copy(h.data, h.data[drop:])
		h.data = h.data[:h.capacity]
		h.startOffset += uint64(drop)
	}
	for id, subscriber := range h.subscribers {
		select {
		case subscriber <- frame:
		default:
			close(subscriber)
			delete(h.subscribers, id)
		}
	}
	return frame
}

// Subscribe atomically captures replay and registers for later live frames.
func (h *OutputHub) Subscribe(offset uint64, queue int) (OutputFrame, <-chan OutputFrame, func(), error) {
	if queue <= 0 {
		queue = 16
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return OutputFrame{}, nil, nil, ErrOutputClosed
	}
	if offset > h.endOffset {
		return OutputFrame{}, nil, nil, ErrOffsetAhead
	}
	start := offset
	gap := false
	if start < h.startOffset {
		start, gap = h.startOffset, true
	}
	replay := OutputFrame{Offset: start, Gap: gap, Data: append([]byte(nil), h.data[int(start-h.startOffset):]...)}
	h.nextID++
	id := h.nextID
	stream := make(chan OutputFrame, queue)
	h.subscribers[id] = stream
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			h.mu.Lock()
			if current, ok := h.subscribers[id]; ok {
				delete(h.subscribers, id)
				close(current)
			}
			h.mu.Unlock()
		})
	}
	return replay, stream, cancel, nil
}

func (h *OutputHub) Bounds() (start, end uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.startOffset, h.endOffset
}

func (h *OutputHub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for id, subscriber := range h.subscribers {
		close(subscriber)
		delete(h.subscribers, id)
	}
	for i := range h.data {
		h.data[i] = 0
	}
	h.data = nil
}
