package handlers

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// receivePackMaxCommandBytes bounds the pkt-line command section the proxy
	// reads before it starts streaming the pack upstream.
	receivePackMaxCommandBytes = 1 << 20
	receivePackMaxCommands     = 1000
	// receivePackMaxDrain bounds how much of a refused push's pack is read and
	// discarded so the git client sees the rejection instead of a reset.
	receivePackMaxDrain = 256 << 20
	// receivePackMaxPktPayload is the largest pkt-line payload (65520 - 4).
	receivePackMaxPktPayload = 65516
)

// receivePackDrainTimeout bounds, by wall clock, how long a refused push's
// remaining body is read. A variable so tests can shorten it.
var receivePackDrainTimeout = 30 * time.Second

type receivePackCommand struct {
	Old, New, Ref string
}

// IsCreate reports whether the command creates its ref.
func (c receivePackCommand) isCreate() bool { return isZeroOID(c.Old) && !isZeroOID(c.New) }

type receivePackRequest struct {
	Commands []receivePackCommand
	Caps     map[string]bool
	// Prefix is every byte consumed from the body (through the flush packet),
	// to be replayed ahead of the unread remainder.
	Prefix []byte
}

func isZeroOID(s string) bool { return strings.Trim(s, "0") == "" }

func isHexDigit(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func isHexOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isHexDigit(c) {
			return false
		}
	}
	return true
}

var errReceivePackTruncated = errors.New("truncated git-receive-pack command section")

// parseReceivePackCommands reads only the pkt-line command section of a
// git-receive-pack request (up to and including the first flush packet) and
// never reads further, so the pack that follows stays unread in r.
func parseReceivePackCommands(r io.Reader, maxBytes, maxCommands int) (*receivePackRequest, error) {
	out := &receivePackRequest{Caps: map[string]bool{}}
	var consumed bytes.Buffer
	for {
		var hdr [4]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return nil, errReceivePackTruncated
		}
		consumed.Write(hdr[:])
		for _, c := range hdr {
			if !isHexDigit(c) {
				return nil, errors.New("malformed pkt-line length in git-receive-pack request")
			}
		}
		n64, _ := strconv.ParseUint(string(hdr[:]), 16, 32)
		n := int(n64)
		if n == 0 {
			out.Prefix = consumed.Bytes()
			return out, nil
		}
		if n < 4 {
			return nil, errors.New("unsupported pkt-line in git-receive-pack command section")
		}
		if n > 65520 {
			return nil, errors.New("oversize pkt-line in git-receive-pack request")
		}
		if consumed.Len()+n-4 > maxBytes {
			return nil, fmt.Errorf("git-receive-pack command section exceeds %d bytes", maxBytes)
		}
		payload := make([]byte, n-4)
		if _, err := io.ReadFull(r, payload); err != nil {
			return nil, errReceivePackTruncated
		}
		consumed.Write(payload)
		line := string(payload)
		line = strings.TrimSuffix(line, "\n")
		head, capsStr, hasCaps := strings.Cut(line, "\x00")
		if hasCaps {
			for _, c := range strings.Fields(capsStr) {
				out.Caps[c] = true
			}
		}
		switch {
		case strings.HasPrefix(head, "shallow "):
			continue
		case head == "push-cert" || strings.HasPrefix(head, "push-cert"):
			return nil, errors.New("signed pushes (push-cert) are not supported through this proxy")
		}
		parts := strings.SplitN(head, " ", 3)
		if len(parts) != 3 || !isHexOID(parts[0]) || !isHexOID(parts[1]) || len(parts[0]) != len(parts[1]) || parts[2] == "" {
			return nil, errors.New("malformed command in git-receive-pack request")
		}
		if len(out.Commands) >= maxCommands {
			return nil, fmt.Errorf("git-receive-pack request has more than %d commands", maxCommands)
		}
		out.Commands = append(out.Commands, receivePackCommand{Old: parts[0], New: parts[1], Ref: parts[2]})
	}
}

// receivePackNgLine builds one "ng" line that always fits a single pkt-line:
// an over-long ref is echoed truncated and the reason is cut to what is left.
func receivePackNgLine(ref, reason string) string {
	const maxRef = 1024
	if len(ref) > maxRef {
		ref = ref[:maxRef] + "..."
	}
	reason = strings.ReplaceAll(reason, "\n", " ")
	prefix := "ng " + ref + " duckway: "
	if room := receivePackMaxPktPayload - len(prefix) - 1; len(reason) > room {
		cut := room
		for cut > 0 && !utf8.RuneStart(reason[cut]) {
			cut--
		}
		reason = reason[:cut]
	}
	return prefix + reason + "\n"
}

// drainRefusedBody reads and discards the rest of a refused push, bounded by
// size and by a wall-clock deadline. A real server connection gets a read
// deadline; a body that blocks regardless (tests, wrapped readers) is read in
// a goroutine that is abandoned when the deadline passes.
func drainRefusedBody(w http.ResponseWriter, body io.Reader) {
	if body == nil {
		return
	}
	rc := http.NewResponseController(w)
	deadline := time.Now().Add(receivePackDrainTimeout)
	if err := rc.SetReadDeadline(deadline); err == nil {
		defer func() { _ = rc.SetReadDeadline(time.Time{}) }()
		_, _ = io.CopyN(io.Discard, body, receivePackMaxDrain)
		return
	}
	// Fallback only for writers without read-deadline support (a recorder in
	// tests, a wrapped ResponseWriter): a real net/http server connection takes
	// the branch above. The goroutine only reads body and discards, touches no
	// shared state, and ends when the body returns, errors or hits the size cap;
	// we stop waiting after the timeout. For a server request the body is
	// closed by net/http when the handler returns, which unblocks the read.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.CopyN(io.Discard, body, receivePackMaxDrain)
	}()
	t := time.NewTimer(receivePackDrainTimeout)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
	}
}

func pktLine(b *bytes.Buffer, s string) {
	fmt.Fprintf(b, "%04x%s", len(s)+4, s)
}

// receivePackRejectionBody builds the report-status payload: "unpack ok" and
// one "ng" per command, wrapped in side-band band 1 when requested.
func receivePackRejectionBody(req *receivePackRequest, reasons []string) []byte {
	var report bytes.Buffer
	pktLine(&report, "unpack ok\n")
	for i, c := range req.Commands {
		reason := reasons[i]
		if reason == "" {
			reason = "another ref in this push was refused"
		}
		pktLine(&report, receivePackNgLine(c.Ref, reason))
	}
	report.WriteString("0000")
	if !req.Caps["side-band-64k"] && !req.Caps["side-band"] {
		return report.Bytes()
	}
	maxData := 65515
	if !req.Caps["side-band-64k"] {
		maxData = 995
	}
	var out bytes.Buffer
	data := report.Bytes()
	for len(data) > 0 {
		chunk := data
		if len(chunk) > maxData {
			chunk = chunk[:maxData]
		}
		data = data[len(chunk):]
		fmt.Fprintf(&out, "%04x\x01", len(chunk)+5)
		out.Write(chunk)
	}
	out.WriteString("0000")
	return out.Bytes()
}

// writeReceivePackRefusal answers a refused push. A client that asked for
// report-status gets a protocol-level rejection that `git push` prints as
// "! [remote rejected]"; anything else gets a plain 403. The rest of the
// request body is drained (bounded) so the client is not cut off mid-upload.
func writeReceivePackRefusal(w http.ResponseWriter, body io.Reader, req *receivePackRequest, reasons []string, fallback string) {
	drainRefusedBody(w, body)
	if req == nil || (!req.Caps["report-status"] && !req.Caps["report-status-v2"]) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "duckway: "+fallback+"\n")
		return
	}
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(receivePackRejectionBody(req, reasons))
}
