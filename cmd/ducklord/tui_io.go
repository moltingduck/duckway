package main

import (
	"context"
	"io"
	"os"
	"time"
	"unicode/utf8"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func readInput(ctx context.Context, ch chan<- []byte, ready chan<- struct{}) {
	raw := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 64)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				close(raw)
				return
			}
			select {
			case raw <- append([]byte(nil), buf[:n]...):
			case <-ctx.Done():
				return
			}
		}
	}()
	var pending []byte
	var escapeTimer *time.Timer
	var escapeTimeout <-chan time.Time
	stopEscapeTimer := func() {
		if escapeTimer != nil && !escapeTimer.Stop() {
			select {
			case <-escapeTimer.C:
			default:
			}
		}
		escapeTimeout = nil
	}
	emit := func(event []byte) bool {
		select {
		case ready <- struct{}{}:
		default:
		}
		select {
		case ch <- event:
			return true
		case <-ctx.Done():
			return false
		}
	}
	for {
		select {
		case <-ctx.Done():
			stopEscapeTimer()
			return
		case data, open := <-raw:
			if !open {
				return
			}
			stopEscapeTimer()
			pending = append(pending, data...)
		case <-escapeTimeout:
			escapeTimeout = nil
			if len(pending) > 0 && pending[0] == 0x1b {
				if !emit([]byte{0x1b}) {
					return
				}
				pending = pending[1:]
			}
		}
		for len(pending) > 0 {
			event, rest, ok := nextInputEvent(pending)
			if !ok {
				pending = rest
				if len(pending) <= 2 && pending[0] == 0x1b {
					escapeTimer = time.NewTimer(inputEscapeAmbiguityTimeout)
					escapeTimeout = escapeTimer.C
				}
				break
			}
			pending = rest
			if !emit(event) {
				return
			}
		}
	}
}

func nextInputEvent(pending []byte) (event, rest []byte, ok bool) {
	if len(pending) == 0 {
		return nil, nil, false
	}
	if pending[0] != 0x1b {
		if pending[0] < utf8.RuneSelf {
			return append([]byte(nil), pending[:1]...), pending[1:], true
		}
		if !utf8.FullRune(pending) {
			return nil, pending, false
		}
		_, size := utf8.DecodeRune(pending)
		return append([]byte(nil), pending[:size]...), pending[size:], true
	}
	if len(pending) == 1 || len(pending) == 2 && pending[1] == '[' {
		return nil, pending, false
	}
	if len(pending) >= 3 && pending[1] == '[' && (pending[2] == 'A' || pending[2] == 'B' || pending[2] == 'C' || pending[2] == 'D') {
		return append([]byte(nil), pending[:3]...), pending[3:], true
	}
	if len(pending) >= 3 && pending[1] == '[' && (pending[2] == '5' || pending[2] == '6') {
		if len(pending) == 3 {
			return nil, pending, false
		}
		if pending[3] == '~' {
			return append([]byte(nil), pending[:4]...), pending[4:], true
		}
	}
	if len(pending) >= 4 && pending[1] == '[' && pending[2] == '<' {
		for i := 3; i < len(pending); i++ {
			if pending[i] == 'M' || pending[i] == 'm' {
				return append([]byte(nil), pending[:i+1]...), pending[i+1:], true
			}
		}
		if len(pending) > 64 {
			return append([]byte(nil), pending...), nil, true
		}
		return nil, pending, false
	}
	// Preserve Alt/Meta key chords as one event. In particular, this prevents
	// the Esc prefix from closing copy mode and its suffix from becoming a
	// normal-mode shortcut such as q or c.
	if len(pending) >= 2 && pending[1] != '[' {
		if pending[1] < utf8.RuneSelf {
			// C0 controls are complete input events in their own right. In
			// particular, do not merge Esc + Ctrl-] into an Alt-style chord:
			// after a modal consumes Esc, Ctrl-] must still unfocus the PTY.
			if pending[1] < 0x20 || pending[1] == 0x7f {
				return append([]byte(nil), pending[:1]...), pending[1:], true
			}
			return append([]byte(nil), pending[:2]...), pending[2:], true
		}
		if !utf8.FullRune(pending[1:]) {
			return nil, pending, false
		}
		_, size := utf8.DecodeRune(pending[1:])
		return append([]byte(nil), pending[:1+size]...), pending[1+size:], true
	}
	return append([]byte(nil), pending[:1]...), pending[1:], true
}

func superviseAttach(ctx context.Context, id int, session *ducklord.AttachSession, out chan<- attachOutputEvent) {
	stdoutDone := make(chan error, 1)
	go readAttachOutput(ctx, id, session, out, stdoutDone)
	var stdoutErr error
	var commandErr error
	stdoutOpen := true
	commandOpen := session.Done != nil
	for stdoutOpen || commandOpen {
		select {
		case err := <-stdoutDone:
			stdoutErr = err
			stdoutOpen = false
			_ = session.Stdin.Close()
		case err := <-session.Done:
			commandErr = err
			commandOpen = false
		case <-ctx.Done():
			return
		}
	}
	if commandErr != nil {
		select {
		case out <- attachOutputEvent{id: id, done: true, err: commandErr}:
		case <-ctx.Done():
		}
		return
	}
	select {
	case out <- attachOutputEvent{id: id, done: true, err: stdoutErr}:
	case <-ctx.Done():
	}
}

func runResizeWorker(ctx context.Context, requests <-chan resizeRequest, results chan<- resizeDoneEvent) {
	for {
		select {
		case <-ctx.Done():
			return
		case request := <-requests:
			// Collapse queued SIGWINCH bursts before starting the single RPC.
			for {
				select {
				case newer := <-requests:
					request = newer
				default:
					goto resize
				}
			}
		resize:
			barrier, err := request.resize(request.rows, request.cols)
			select {
			case results <- resizeDoneEvent{id: request.id, rows: request.rows, cols: request.cols, barrier: barrier, err: err}:
			case <-ctx.Done():
				return
			}
		}
	}
}

func readAttachOutput(ctx context.Context, id int, session *ducklord.AttachSession, out chan<- attachOutputEvent, done chan<- error) {
	buf := make([]byte, 4096)
	for {
		n, err := session.Stdout.Read(buf)
		if n > 0 {
			offset := uint64(0)
			if session.OutputOffset != nil {
				offset = session.OutputOffset()
			}
			startOffset := offset
			if offset >= uint64(n) {
				startOffset = offset - uint64(n)
			}
			select {
			case out <- attachOutputEvent{id: id, text: string(buf[:n]), runtimeGeneration: session.RuntimeGeneration, startOffset: startOffset, outputOffset: offset}:
			case <-ctx.Done():
				return
			}
		}
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			select {
			case done <- err:
			case <-ctx.Done():
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}
