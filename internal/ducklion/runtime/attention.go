package runtime

import "bytes"

const maxAttentionSequenceBytes = 4096

// AttentionDetector recognizes only terminal sequences that explicitly ask for
// user attention. It keeps framing state across PTY output chunks, but never
// exposes or persists terminal-provided payloads.
type AttentionDetector struct {
	state   attentionParseState
	payload []byte
	offset  uint64
}

type attentionParseState uint8

const (
	attentionText attentionParseState = iota
	attentionEscape
	attentionOSC
	attentionOSCEscape
	attentionDiscardOSC
	attentionDiscardOSCEscape
)

// Feed returns the number of complete allowlisted notifications in data.
func (d *AttentionDetector) Feed(data []byte) int {
	return len(d.FeedOffsets(data))
}

// FeedOffsets returns exclusive output offsets for complete notifications.
// Offsets make a notification report idempotent across daemon reconnects.
func (d *AttentionDetector) FeedOffsets(data []byte) []uint64 {
	var offsets []uint64
	for _, b := range data {
		d.offset++
		switch d.state {
		case attentionText:
			switch b {
			case '\a':
				offsets = append(offsets, d.offset)
			case 0x1b:
				d.state = attentionEscape
			}
		case attentionEscape:
			switch b {
			case ']':
				d.payload = d.payload[:0]
				d.state = attentionOSC
			case 0x1b:
				d.state = attentionEscape
			default:
				d.state = attentionText
				if b == '\a' {
					offsets = append(offsets, d.offset)
				}
			}
		case attentionOSC:
			switch b {
			case '\a':
				if allowlistedAttentionOSC(d.payload) {
					offsets = append(offsets, d.offset)
				}
				d.reset()
			case 0x1b:
				d.state = attentionOSCEscape
			default:
				d.appendPayload(b)
			}
		case attentionOSCEscape:
			switch b {
			case '\\', '\a':
				if allowlistedAttentionOSC(d.payload) {
					offsets = append(offsets, d.offset)
				}
				d.reset()
			default:
				d.appendPayload(0x1b)
				d.appendPayload(b)
				if d.state != attentionText {
					d.state = attentionOSC
				}
			}
		case attentionDiscardOSC:
			switch b {
			case '\a':
				d.reset()
			case 0x1b:
				d.state = attentionDiscardOSCEscape
			}
		case attentionDiscardOSCEscape:
			if b == '\\' || b == '\a' {
				d.reset()
			} else if b != 0x1b {
				d.state = attentionDiscardOSC
			}
		}
	}
	return offsets
}

func (d *AttentionDetector) appendPayload(b byte) {
	if len(d.payload) >= maxAttentionSequenceBytes {
		d.payload = d.payload[:0]
		d.state = attentionDiscardOSC
		return
	}
	d.payload = append(d.payload, b)
}

func (d *AttentionDetector) reset() {
	d.state = attentionText
	d.payload = d.payload[:0]
}

func allowlistedAttentionOSC(payload []byte) bool {
	command, body, found := bytes.Cut(payload, []byte{';'})
	if !found {
		return false
	}
	switch string(command) {
	case "9":
		// OSC 9;4 is taskbar progress, not a user-attention notification.
		return !bytes.Equal(body, []byte("4")) && !bytes.HasPrefix(body, []byte("4;"))
	case "99", "777":
		return true
	default:
		return false
	}
}
