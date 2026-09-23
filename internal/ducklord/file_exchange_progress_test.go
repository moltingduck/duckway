package ducklord

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCopyLocalCleanupFailurePreservesCommittedOutcome(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	if err := os.Mkdir(src, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dst, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "item"), []byte("committed bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	cleanupErr := errors.New("injected cleanup failure")
	cleanupAttempts := 0
	var events []FileCopyProgress
	req := FileCopyRequest{Source: FileEndpoint{Path: src}, Destination: FileEndpoint{Path: dst}, Names: []string{"item"}}
	req.Progress = func(p FileCopyProgress) {
		if p.State == "copied" && cleanupAttempts == 0 {
			t.Error("copied event preceded staging cleanup attempt")
		}
		events = append(events, p)
	}
	results, err := copyLocalWithStageRemover(context.Background(), req, func(_ *os.Root, _ string) error {
		cleanupAttempts++
		return cleanupErr
	})
	if !errors.Is(err, cleanupErr) {
		t.Fatalf("cleanup error not returned: %v", err)
	}
	if cleanupAttempts == 0 {
		t.Fatal("staging cleanup was not attempted")
	}
	if len(results) != 1 || results[0].Name != "item" || results[0].Destination != filepath.Join(dst, "item") {
		t.Fatalf("committed result missing: %#v", results)
	}
	if got, readErr := os.ReadFile(results[0].Destination); readErr != nil || string(got) != "committed bytes" {
		t.Fatalf("destination commit missing: %q, %v", got, readErr)
	}
	if len(events) != 2 || events[0].State != "copying" || events[1].State != "copied" || events[1].Completed != 1 {
		t.Fatalf("unexpected progress outcomes: %#v", events)
	}
}

func TestCopyFilesProgressCommitOrderAndConflicts(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	if err := os.Mkdir(src, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dst, 0700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"a": "A", "b": "B", "skip": "new"} {
		if err := os.WriteFile(filepath.Join(src, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dst, "skip"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	var events []FileCopyProgress
	req := FileCopyRequest{Source: FileEndpoint{Path: src}, Destination: FileEndpoint{Path: dst}, Names: []string{"a", "skip", "b"}, Conflict: "rename"}
	req.Progress = func(p FileCopyProgress) {
		events = append(events, p)
		if p.State == "copied" {
			if got, err := os.ReadFile(p.Destination); err != nil || string(got) == "" {
				t.Errorf("copied event preceded destination commit: %q, %v", p.Destination, err)
			}
		}
	}
	results, err := CopyFiles(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 || results[1].Destination != filepath.Join(dst, "skip (1)") {
		t.Fatalf("results %#v", results)
	}
	wantStates := []string{"copying", "copied", "copying", "copied", "copying", "copied"}
	gotStates := make([]string, len(events))
	for i, event := range events {
		gotStates[i] = event.State
		if event.Total != 3 {
			t.Fatalf("event total %#v", event)
		}
	}
	if !reflect.DeepEqual(gotStates, wantStates) {
		t.Fatalf("states %v", gotStates)
	}
	if events[1].Completed != 1 || events[3].Completed != 2 || events[5].Completed != 3 || events[3].Destination != results[1].Destination {
		t.Fatalf("completion/destination events %#v", events)
	}
	// A skip reports the original occupied path, while rename commits to the
	// selected alternate path.
	if got, _ := os.ReadFile(filepath.Join(dst, "skip")); string(got) != "old" {
		t.Fatalf("skip changed destination to %q", got)
	}
	var skipEvents []FileCopyProgress
	_, err = CopyFiles(context.Background(), FileCopyRequest{Source: FileEndpoint{Path: src}, Destination: FileEndpoint{Path: dst}, Names: []string{"skip"}, Conflict: "skip", Progress: func(p FileCopyProgress) { skipEvents = append(skipEvents, p) }})
	if err != nil || len(skipEvents) != 2 || skipEvents[0].State != "copying" || skipEvents[1].State != "skipped" || skipEvents[1].Completed != 1 || skipEvents[1].Destination != filepath.Join(dst, "skip") {
		t.Fatalf("skip progress %#v err %v", skipEvents, err)
	}
}

func TestCopyFilesProgressPartialFailureAndValidation(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	if err := os.Mkdir(src, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dst, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "ok"), []byte("ok"), 0600); err != nil {
		t.Fatal(err)
	}
	var events []FileCopyProgress
	base := FileCopyRequest{Source: FileEndpoint{Path: src}, Destination: FileEndpoint{Path: dst}, Names: []string{"ok", "missing"}, Progress: func(p FileCopyProgress) { events = append(events, p) }}
	results, err := CopyFiles(context.Background(), base)
	if err == nil || len(results) != 1 || len(events) != 3 {
		t.Fatalf("partial result %#v events %#v err %v", results, events, err)
	}
	if events[0].State != "copying" || events[1].State != "copied" || events[2].State != "copying" || events[2].Name != "missing" || events[2].Completed != 1 {
		t.Fatalf("partial events %#v", events)
	}
	var validationEvents []FileCopyProgress
	invalid := base
	invalid.Names = []string{"../bad"}
	invalid.Progress = func(p FileCopyProgress) { validationEvents = append(validationEvents, p) }
	if _, err := CopyFiles(context.Background(), invalid); err == nil || len(validationEvents) != 0 {
		t.Fatalf("validation events %#v err %v", validationEvents, err)
	}
}

func TestCopyFilesProgressCancellationBetweenItems(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	if err := os.Mkdir(src, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dst, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(src, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var events []FileCopyProgress
	req := FileCopyRequest{Source: FileEndpoint{Path: src}, Destination: FileEndpoint{Path: dst}, Names: []string{"a", "b"}}
	req.Progress = func(p FileCopyProgress) {
		events = append(events, p)
		if p.State == "copied" {
			cancel()
		}
	}
	results, err := CopyFiles(ctx, req)
	if err == nil || len(results) != 1 {
		t.Fatalf("cancel result %#v err %v", results, err)
	}
	if len(events) != 2 || events[0].Name != "a" || events[0].State != "copying" || events[1].State != "copied" {
		t.Fatalf("cancellation started another item: %#v", events)
	}
}
