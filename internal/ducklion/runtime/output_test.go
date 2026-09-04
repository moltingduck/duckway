package runtime

import (
	"bytes"
	"errors"
	"testing"
)

func TestOutputHubReplayGapAndLiveBoundary(t *testing.T) {
	hub := NewOutputHub(5)
	hub.Publish([]byte("abc"))
	hub.Publish([]byte("def"))
	replay, live, cancel, err := hub.Subscribe(0, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if !replay.Gap || replay.Offset != 1 || string(replay.Data) != "bcdef" {
		t.Fatalf("replay=%+v", replay)
	}
	hub.Publish([]byte("gh"))
	frame := <-live
	if frame.Offset != 6 || string(frame.Data) != "gh" {
		t.Fatalf("live=%+v", frame)
	}
	if _, _, _, err := hub.Subscribe(99, 1); !errors.Is(err, ErrOffsetAhead) {
		t.Fatalf("ahead error=%v", err)
	}
}

func TestOutputHubDropsSlowSubscriberWithoutBlockingFastSubscriber(t *testing.T) {
	hub := NewOutputHub(8)
	_, slow, _, err := hub.Subscribe(0, 1)
	if err != nil {
		t.Fatal(err)
	}
	_, fast, cancelFast, err := hub.Subscribe(0, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer cancelFast()
	for _, chunk := range []string{"a", "b", "c"} {
		hub.Publish([]byte(chunk))
	}
	var got bytes.Buffer
	for i := 0; i < 3; i++ {
		got.Write((<-fast).Data)
	}
	if got.String() != "abc" {
		t.Fatalf("fast output=%q", got.String())
	}
	if _, ok := <-slow; !ok { // first queued frame is delivered before closure
		t.Fatal("slow subscriber lost its already queued frame")
	}
	if _, ok := <-slow; ok {
		t.Fatal("slow subscriber was not closed after overflow")
	}
}

func TestOutputHubCloseZeroesAndCloses(t *testing.T) {
	hub := NewOutputHub(8)
	hub.Publish([]byte("secret"))
	_, stream, _, err := hub.Subscribe(0, 1)
	if err != nil {
		t.Fatal(err)
	}
	hub.Close()
	if _, ok := <-stream; ok {
		t.Fatal("stream remains open")
	}
	start, end := hub.Bounds()
	if start != 0 || end != 6 {
		t.Fatalf("bounds changed unexpectedly: %d %d", start, end)
	}
	hub.Publish([]byte("resurrect"))
	if _, _, _, err := hub.Subscribe(end, 1); !errors.Is(err, ErrOutputClosed) {
		t.Fatalf("subscribe after close error=%v", err)
	}
	if _, after := hub.Bounds(); after != end {
		t.Fatalf("publish after close changed end to %d", after)
	}
}
