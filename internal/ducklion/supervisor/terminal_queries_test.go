package supervisor

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTerminalQueryParserAcrossChunksAndBounds(t *testing.T) {
	var parser terminalQueryParser
	size := uint32(47)<<16 | 132
	parts := []string{"plain\x1b]10;", "?\x1b", "\\\x1b[?", "u\x1b[18", "t\x1b[", "6n"}
	var replies [][]byte
	for _, part := range parts {
		replies = append(replies, parser.Feed([]byte(part), size)...)
	}
	want := [][]byte{[]byte("\x1b]10;rgb:ffff/ffff/ffff\x1b\\"), []byte("\x1b[?0u"), []byte("\x1b[8;47;132t"), []byte("\x1b[1;1R")}
	if len(replies) != len(want) {
		t.Fatalf("replies=%q", replies)
	}
	for i := range want {
		if !bytes.Equal(replies[i], want[i]) {
			t.Fatalf("reply[%d]=%q, want %q", i, replies[i], want[i])
		}
	}
	if got := parser.Feed([]byte("\x1b]11;?\a\x1b[c\x1b[>c\x1b[5n\x1b[>q"), size); len(got) != 5 {
		t.Fatalf("BEL/CSI replies=%q", got)
	}
	if got := parser.Feed([]byte("\x1b]10;"+strings.Repeat("x", maxTerminalQueryBytes+1)+"\a\x1b[?25h"), size); len(got) != 0 || len(parser.data) > maxTerminalQueryBytes {
		t.Fatalf("oversized/unrecognized query replies=%q state=%+v", got, parser)
	}
	if got := parser.Feed([]byte(strings.Repeat("\x1b[c", maxTerminalQueryReplies+50)), size); len(got) != maxTerminalQueryReplies {
		t.Fatalf("query flood produced %d replies, max %d", len(got), maxTerminalQueryReplies)
	}
}

func TestTerminalReplyRateLimitAcrossReads(t *testing.T) {
	var parser terminalQueryParser
	var limiter terminalReplyLimiter
	now := time.Unix(100, 0)
	allowed := 0
	for i := 0; i < 20; i++ {
		for range parser.Feed([]byte(strings.Repeat("\x1b[c", 20)), uint32(40)<<16|120) {
			if limiter.Allow(now) {
				allowed++
			}
		}
	}
	if allowed != terminalReplyBurst {
		t.Fatalf("rapid cross-read query flood allowed %d replies, want burst %d", allowed, terminalReplyBurst)
	}
	for range parser.Feed([]byte(strings.Repeat("\x1b[c", 20)), uint32(40)<<16|120) {
		if limiter.Allow(now.Add(100 * time.Millisecond)) {
			allowed++
		}
	}
	if allowed != terminalReplyBurst+12 {
		t.Fatalf("100ms refill allowed %d total replies, want %d", allowed, terminalReplyBurst+12)
	}
	for range parser.Feed([]byte(strings.Repeat("\x1b[c", 100)), uint32(40)<<16|120) {
		if limiter.Allow(now.Add(10 * time.Second)) {
			allowed++
		}
	}
	if allowed != terminalReplyBurst+12+maxTerminalQueryReplies {
		t.Fatalf("per-read cap or burst refill failed: %d", allowed)
	}
}

func TestSupervisorAnswersTerminalQueryWithoutAttachedWriter(t *testing.T) {
	dir := t.TempDir()
	probe := filepath.Join(dir, "reply")
	query := "\x1b]11;?\x1b\\"
	want := "\x1b]11;rgb:0000/0000/0000\x1b\\"
	command := fmt.Sprintf("stty raw -echo; printf %%s %s; sleep .02; printf %%s %s; dd bs=1 count=%d of=%s 2>/dev/null; printf query-done", shellQuote(query[:5]), shellQuote(query[5:]), len(want), shellQuote(probe))
	session, err := Start(Options{SessionID: "ABC123", RuntimeGeneration: 1, OwnershipEpoch: 1, CWD: dir,
		Command: []string{"sh", "-c", command}, Rows: 31, Cols: 91, OutputCapacity: 4096, RetainedOutputDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- session.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		_ = session.Terminate(true)
		<-done
		t.Fatal("terminal query was not answered without an attached Ducklord writer")
	}
	reply, err := os.ReadFile(probe)
	if err != nil {
		t.Fatal(err)
	}
	if string(reply) != want {
		t.Fatalf("PTY query reply=%q, want %q", reply, want)
	}
	replay, err := ReadRetainedOutput(dir, "ABC123", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(replay.Data, []byte("query-done")) || bytes.Contains(replay.Data, []byte(want)) {
		t.Fatalf("output contained internal reply or lost completion: %q", replay.Data)
	}
}
