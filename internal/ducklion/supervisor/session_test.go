package supervisor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	duckruntime "github.com/hackerduck/duckway/internal/ducklion/runtime"
)

func TestAgentPrepareCommitWritesExactlyOnceOnReplay(t *testing.T) {
	session, err := Start(Options{SessionID: "ABC123", RuntimeGeneration: 2, OwnershipEpoch: 3, CWD: t.TempDir(),
		Command: []string{"sh", "-c", `IFS= read -r value; printf 'got:%s\n' "$value"; sleep 30`}, OutputCapacity: 1024})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Terminate(true); _ = session.Wait() }()
	replay, stream, cancel, err := session.Output().Subscribe(0, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if len(replay.Data) != 0 {
		t.Fatalf("replay=%q", replay.Data)
	}
	prompt := []byte("hello")
	digest := sha256.Sum256(prompt)
	owner := model.Owner{Kind: model.OwnerCC, ID: "dwch_task"}
	if err := session.PrepareAgentTask("inbox/42", digest, prompt, owner, 3, 2); err != nil {
		t.Fatal(err)
	}
	if err := session.CommitAgentTask(context.Background(), "inbox/42", digest, owner, 3, 2); err != nil {
		t.Fatal(err)
	}
	if err := session.CommitAgentTask(context.Background(), "inbox/42", digest, owner, 3, 2); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	deadline := time.After(2 * time.Second)
	for !bytes.Contains(output.Bytes(), []byte("got:")) {
		select {
		case frame := <-stream:
			output.Write(frame.Data)
		case <-deadline:
			t.Fatalf("output timeout: %q", output.String())
		}
	}
	if count := bytes.Count(output.Bytes(), []byte("got:")); count != 1 {
		t.Fatalf("prompt executions=%d output=%q", count, output.String())
	}
}

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

func TestSessionResizeRejectsUnsafeBounds(t *testing.T) {
	session, err := Start(Options{SessionID: "ABC123", RuntimeGeneration: 2, OwnershipEpoch: 3, CWD: t.TempDir(), Command: []string{"sh", "-c", "exit 0"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range [][2]uint16{{4, 80}, {201, 80}, {20, 19}, {20, 501}} {
		if err := session.Resize(size[0], size[1], 3, 2); !errors.Is(err, ErrInvalidPTYSize) {
			t.Fatalf("size=%v err=%v", size, err)
		}
	}
	_ = session.Wait()
}

func TestSessionStartRejectsUnsafeBounds(t *testing.T) {
	for _, size := range [][2]uint16{{4, 80}, {201, 80}, {20, 19}, {20, 501}} {
		if _, err := Start(Options{SessionID: "ABC123", RuntimeGeneration: 2, OwnershipEpoch: 3, Rows: size[0], Cols: size[1],
			CWD: t.TempDir(), Command: []string{"sh"}}); !errors.Is(err, ErrInvalidPTYSize) {
			t.Fatalf("size=%v err=%v", size, err)
		}
	}
}
