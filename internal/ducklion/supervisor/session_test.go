package supervisor

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	duckruntime "github.com/hackerduck/duckway/internal/ducklion/runtime"
)

func TestSessionInputIsFencedAndOutputIsMemoryOnly(t *testing.T) {
	session, err := Start(Options{SessionID: "ABC123", RuntimeGeneration: 2, OwnershipEpoch: 3, CWD: t.TempDir(),
		Command: []string{"sh", "-c", `IFS= read -r value; printf 'got:%s\n' "$value"`}, OutputCapacity: 1024})
	if err != nil {
		t.Fatal(err)
	}
	replay, stream, cancel, err := session.Output().Subscribe(0, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if len(replay.Data) != 0 {
		t.Fatalf("unexpected replay=%q", replay.Data)
	}
	stale := duckruntime.InputFrame{Sequence: 1, OwnershipEpoch: 2, RuntimeGeneration: 2, Data: []byte("forbidden\r")}
	if err := session.SubmitInput(context.Background(), stale); !errors.Is(err, model.ErrStaleEpoch) {
		t.Fatalf("stale input error=%v", err)
	}
	valid := duckruntime.InputFrame{Sequence: 1, OwnershipEpoch: 3, RuntimeGeneration: 2, Data: []byte("allowed\r")}
	if err := session.SubmitInput(context.Background(), valid); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	for !bytes.Contains(output.Bytes(), []byte("got:allowed")) {
		frame, ok := <-stream
		if !ok {
			break
		}
		output.Write(frame.Data)
	}
	if !bytes.Contains(output.Bytes(), []byte("got:allowed")) || bytes.Contains(output.Bytes(), []byte("forbidden")) {
		t.Fatalf("output=%q", output.String())
	}
	if err := session.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionResizeIsGenerationAndEpochFenced(t *testing.T) {
	session, err := Start(Options{SessionID: "ABC123", RuntimeGeneration: 2, OwnershipEpoch: 3, CWD: t.TempDir(), Command: []string{"sh", "-c", "exit 0"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Resize(20, 80, 2, 2); !errors.Is(err, model.ErrStaleEpoch) {
		t.Fatalf("stale epoch error=%v", err)
	}
	if err := session.Resize(20, 80, 3, 1); !errors.Is(err, model.ErrStaleGeneration) {
		t.Fatalf("stale generation error=%v", err)
	}
	_ = session.Wait()
}
