package runtime

import (
	"errors"
	"sync"
)

var ErrOffsetAhead = errors.New("output offset is ahead of runtime")
var ErrOutputClosed = errors.New("output hub is closed")

const maxOutputFrameBytes = 64 << 10

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
	start       int
	size        int
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
	return &OutputHub{capacity: capacity, data: make([]byte, capacity), subscribers: make(map[uint64]chan OutputFrame)}
}

func (h *OutputHub) Publish(data []byte) OutputFrame {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return OutputFrame{Offset: h.endOffset}
	}
	startOffset := h.endOffset
	for start := 0; start < len(data); start += maxOutputFrameBytes {
		end := start + maxOutputFrameBytes
		if end > len(data) {
			end = len(data)
		}
		chunk := data[start:end]
		frame := OutputFrame{Offset: h.endOffset, Data: append([]byte(nil), chunk...)}
		h.endOffset += uint64(len(chunk))
		h.appendLocked(chunk)
		for id, subscriber := range h.subscribers {
			select {
			case subscriber <- frame:
			default:
				close(subscriber)
				delete(h.subscribers, id)
			}
		}
	}
	// The return value is an acknowledgement view; its Data aliases the caller
	// and is never retained by the hub. Subscriber frames are owned copies.
	return OutputFrame{Offset: startOffset, Data: data}
}

// PublishRecovered seeds an empty hub with a retained suffix whose original
// byte offset is known after daemon recovery.
func (h *OutputHub) PublishRecovered(offset uint64, data []byte, gap bool) (OutputFrame, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || h.endOffset != 0 || h.size != 0 || offset != 0 && !gap {
		return OutputFrame{}, ErrOffsetAhead
	}
	if len(data) > h.capacity {
		offset += uint64(len(data) - h.capacity)
		data = data[len(data)-h.capacity:]
		gap = true
	}
	h.startOffset = offset
	h.endOffset = offset + uint64(len(data))
	h.appendLocked(data)
	return OutputFrame{Offset: offset, Data: append([]byte(nil), data...), Gap: gap}, nil
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
	replay := OutputFrame{Offset: start, Gap: gap, Data: h.copyRangeLocked(int(start - h.startOffset))}
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

// Snapshot remains available after Close so a supervisor can replay final PTY
// bytes when the daemon was unavailable at process exit.
func (h *OutputHub) Snapshot() OutputFrame {
	h.mu.Lock()
	defer h.mu.Unlock()
	return OutputFrame{Offset: h.startOffset, Gap: h.startOffset != 0, Data: h.copyRangeLocked(0)}
}

// appendLocked retains only the newest capacity bytes without shifting the
// previously retained buffer. Callers must hold h.mu.
func (h *OutputHub) appendLocked(p []byte) {
	if len(p) == 0 {
		return
	}
	if len(p) >= h.capacity {
		dropped := h.size + len(p) - h.capacity
		copy(h.data, p[len(p)-h.capacity:])
		h.start = 0
		h.size = h.capacity
		h.startOffset += uint64(dropped)
		return
	}
	overflow := h.size + len(p) - h.capacity
	if overflow > 0 {
		h.start = (h.start + overflow) % h.capacity
		h.size -= overflow
		h.startOffset += uint64(overflow)
	}
	end := (h.start + h.size) % h.capacity
	first := len(p)
	if available := h.capacity - end; first > available {
		first = available
	}
	copy(h.data[end:], p[:first])
	copy(h.data, p[first:])
	h.size += len(p)
}

// copyRangeLocked returns retained bytes beginning at a logical index. Callers
// must hold h.mu.
func (h *OutputHub) copyRangeLocked(from int) []byte {
	if from < 0 || from > h.size {
		return nil
	}
	n := h.size - from
	out := make([]byte, n)
	physical := (h.start + from) % h.capacity
	first := n
	if available := h.capacity - physical; first > available {
		first = available
	}
	copy(out, h.data[physical:physical+first])
	copy(out[first:], h.data[:n-first])
	return out
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
}
