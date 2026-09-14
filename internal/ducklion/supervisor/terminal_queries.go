package supervisor

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// serializedPTYWriter keeps terminal-generated replies from interleaving with
// user input frames. Terminal replies deliberately bypass the ownership gate.
type serializedPTYWriter struct {
	pty *os.File
	mu  *sync.Mutex
}

func (w *serializedPTYWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pty.Write(data)
}

const (
	maxTerminalQueryBytes    = 64
	maxTerminalQueryReplies  = 32 // bound reply amplification from one PTY read
	terminalReplyBurst       = 64
	terminalRepliesPerSecond = 128
)

// terminalReplyLimiter is owned by one session's reply worker. Dropping excess
// replies bounds a child that issues queries continuously across PTY reads.
type terminalReplyLimiter struct {
	last   time.Time
	tokens float64
}

func (l *terminalReplyLimiter) Allow(now time.Time) bool {
	if l.last.IsZero() {
		l.last, l.tokens = now, terminalReplyBurst
	} else {
		elapsed := now.Sub(l.last).Seconds()
		if elapsed > 0 {
			l.tokens += elapsed * terminalRepliesPerSecond
			if l.tokens > terminalReplyBurst {
				l.tokens = terminalReplyBurst
			}
			l.last = now
		}
	}
	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
}

type terminalQueryParser struct {
	state byte // 0 ground, 1 ESC, 2 CSI, 3 OSC, 4 OSC ESC
	data  []byte
}

// Feed observes (but does not remove) the child PTY's output. State survives
// arbitrary read boundaries; malformed/oversized escapes are discarded.
func (p *terminalQueryParser) Feed(chunk []byte, size uint32) [][]byte {
	var replies [][]byte
	for _, b := range chunk {
		switch p.state {
		case 0:
			if b == 0x1b {
				p.state = 1
			}
		case 1:
			switch b {
			case '[':
				p.state, p.data = 2, p.data[:0]
			case ']':
				p.state, p.data = 3, p.data[:0]
			case 0x1b:
				// A new ESC can start another sequence.
			default:
				p.state = 0
			}
		case 2:
			if b >= 0x40 && b <= 0x7e {
				if reply := terminalCSIReply(string(p.data), b, size); len(reply) != 0 && len(replies) < maxTerminalQueryReplies {
					replies = append(replies, reply)
				}
				p.state, p.data = 0, p.data[:0]
			} else if b >= 0x20 && b <= 0x3f && len(p.data) < maxTerminalQueryBytes {
				p.data = append(p.data, b)
			} else {
				p.state, p.data = 0, p.data[:0]
			}
		case 3:
			switch b {
			case 0x07:
				if reply := terminalOSCReply(string(p.data)); len(reply) != 0 && len(replies) < maxTerminalQueryReplies {
					replies = append(replies, reply)
				}
				p.state, p.data = 0, p.data[:0]
			case 0x1b:
				p.state = 4
			default:
				if b < 0x20 || len(p.data) >= maxTerminalQueryBytes {
					p.state, p.data = 0, p.data[:0]
				} else {
					p.data = append(p.data, b)
				}
			}
		case 4:
			switch b {
			case '\\':
				if reply := terminalOSCReply(string(p.data)); len(reply) != 0 && len(replies) < maxTerminalQueryReplies {
					replies = append(replies, reply)
				}
				p.state, p.data = 0, p.data[:0]
			case 0x1b:
				p.state, p.data = 1, p.data[:0]
			default:
				p.state, p.data = 0, p.data[:0]
			}
		}
	}
	return replies
}

func terminalCSIReply(params string, final byte, size uint32) []byte {
	switch {
	case final == 'c' && (params == "" || params == "0"):
		return []byte("\x1b[?6c")
	case final == 'c' && (params == ">" || params == ">0"):
		return []byte("\x1b[>0;0;0c")
	case final == 'n' && params == "5":
		return []byte("\x1b[0n")
	case final == 'n' && params == "6":
		// Headless fallback: the supervisor cannot infer the canonical cursor
		// position from raw output without a full terminal state machine.
		return []byte("\x1b[1;1R")
	case final == 't' && params == "18":
		return []byte(fmt.Sprintf("\x1b[8;%d;%dt", size>>16, size&0xffff))
	case final == 'q' && params == ">":
		return []byte("\x1bP>|duckway 0\x1b\\")
	case final == 'u' && params == "?":
		// No Kitty keyboard enhancement is enabled by default.
		return []byte("\x1b[?0u")
	}
	return nil
}

func terminalOSCReply(params string) []byte {
	// Headless fallback palette: deterministic high-contrast light foreground
	// and dark background, independent of any Ducklord viewer's own theme.
	switch params {
	case "10;?":
		return []byte("\x1b]10;rgb:ffff/ffff/ffff\x1b\\")
	case "11;?":
		return []byte("\x1b]11;rgb:0000/0000/0000\x1b\\")
	}
	return nil
}
