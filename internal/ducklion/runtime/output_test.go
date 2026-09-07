package runtime

import (
	"bytes"
	"errors"
	"math/rand"
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

func TestOutputHubCircularRetentionMatchesReference(t *testing.T) {
	const capacity = 257
	hub := NewOutputHub(capacity)
	rng := rand.New(rand.NewSource(42))
	var all []byte
	for i := 0; i < 10_000; i++ {
		chunk := make([]byte, 1+rng.Intn(31))
		if _, err := rng.Read(chunk); err != nil {
			t.Fatal(err)
		}
		hub.Publish(chunk)
		all = append(all, chunk...)
		want := all
		if len(want) > capacity {
			want = want[len(want)-capacity:]
		}
		if i%97 == 0 {
			snapshot := hub.Snapshot()
			if !bytes.Equal(snapshot.Data, want) {
				t.Fatalf("iteration %d retained bytes differ", i)
			}
			if snapshot.Offset != uint64(len(all)-len(want)) {
				t.Fatalf("iteration %d offset=%d want=%d", i, snapshot.Offset, len(all)-len(want))
			}
		}
	}
}

func TestOutputHubRecoveredSnapshotIsBounded(t *testing.T) {
	hub := NewOutputHub(5)
	frame, err := hub.PublishRecovered(10, []byte("abcdefgh"), true)
	if err != nil {
		t.Fatal(err)
	}
	if frame.Offset != 13 || string(frame.Data) != "defgh" || !frame.Gap {
		t.Fatalf("recovered frame=%+v", frame)
	}
	snapshot := hub.Snapshot()
	if snapshot.Offset != 13 || string(snapshot.Data) != "defgh" || !snapshot.Gap {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	start, end := hub.Bounds()
	if start != 13 || end != 18 {
		t.Fatalf("bounds=(%d,%d)", start, end)
	}
}

func TestOutputHubChunksOversizedLivePublish(t *testing.T) {
	hub := NewOutputHub(128 << 10)
	_, live, cancel, err := hub.Subscribe(0, 32)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	input := bytes.Repeat([]byte("x"), (1<<20)+17)
	ack := hub.Publish(input)
	if ack.Offset != 0 || len(ack.Data) != len(input) {
		t.Fatalf("ack offset=%d length=%d", ack.Offset, len(ack.Data))
	}
	var offset uint64
	for offset < uint64(len(input)) {
		frame, ok := <-live
		if !ok {
			t.Fatal("subscriber dropped despite sufficient bounded queue")
		}
		if frame.Offset != offset || len(frame.Data) == 0 || len(frame.Data) > maxOutputFrameBytes {
			t.Fatalf("frame offset=%d length=%d want offset=%d", frame.Offset, len(frame.Data), offset)
		}
		offset += uint64(len(frame.Data))
	}
	snapshot := hub.Snapshot()
	if len(snapshot.Data) != 128<<10 || snapshot.Offset != uint64(len(input)-(128<<10)) {
		t.Fatalf("snapshot offset=%d length=%d", snapshot.Offset, len(snapshot.Data))
	}
}
