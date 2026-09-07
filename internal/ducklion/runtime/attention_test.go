package runtime

import (
	"bytes"
	"testing"
)

func TestAttentionDetectorRecognizesAllowlistAcrossChunks(t *testing.T) {
	var detector AttentionDetector
	chunks := [][]byte{
		[]byte("plain\a\x1b]9;turn "),
		[]byte("done\a\x1b]99;notify\x1b"),
		[]byte("\\\x1b]777;command;message\a"),
	}
	total := 0
	for _, chunk := range chunks {
		total += detector.Feed(chunk)
	}
	if total != 4 {
		t.Fatalf("attention count=%d", total)
	}
}

func TestAttentionDetectorExcludesProgressTitlesAndMalformedOSC(t *testing.T) {
	var detector AttentionDetector
	data := []byte("\x1b]9;4;50\a\x1b]9;4\x1b\\\x1b]0;title\a\x1b]2;title\x1b\\\x1b]8;;url\a")
	if count := detector.Feed(data); count != 0 {
		t.Fatalf("unexpected attention count=%d", count)
	}
}

func TestAttentionDetectorDoesNotRetainOversizedPayload(t *testing.T) {
	var detector AttentionDetector
	oversized := append([]byte("\x1b]9;"), bytes.Repeat([]byte{'x'}, maxAttentionSequenceBytes+1)...)
	oversized = append(oversized, '\a')
	if count := detector.Feed(oversized); count != 0 {
		t.Fatalf("attention count=%d", count)
	}
	if len(detector.payload) != 0 || detector.state != attentionText {
		t.Fatalf("detector retained oversized payload: len=%d state=%d", len(detector.payload), detector.state)
	}
}

func TestAttentionDetectorResumesAfterDiscardedOversizedOSC(t *testing.T) {
	var detector AttentionDetector
	first := append([]byte("\x1b]9;"), bytes.Repeat([]byte{'x'}, maxAttentionSequenceBytes+1)...)
	if count := detector.Feed(first); count != 0 {
		t.Fatalf("oversized prefix count=%d", count)
	}
	if count := detector.Feed([]byte("ignored\x1b\\text\a")); count != 1 {
		t.Fatalf("post-terminator count=%d", count)
	}
}

func TestAttentionDetectorReportsExclusiveOffsets(t *testing.T) {
	var detector AttentionDetector
	if offsets := detector.FeedOffsets([]byte("abc\a\x1b]9;")); len(offsets) != 1 || offsets[0] != 4 {
		t.Fatalf("first offsets=%v", offsets)
	}
	if offsets := detector.FeedOffsets([]byte("done\a")); len(offsets) != 1 || offsets[0] != 13 {
		t.Fatalf("second offsets=%v", offsets)
	}
}

func TestAttentionDetectorRequiresCompleteOSC(t *testing.T) {
	var detector AttentionDetector
	if count := detector.Feed([]byte("\x1b]777;incomplete")); count != 0 {
		t.Fatalf("incomplete count=%d", count)
	}
	if count := detector.Feed([]byte("\x1b\\")); count != 1 {
		t.Fatalf("completed count=%d", count)
	}
}
