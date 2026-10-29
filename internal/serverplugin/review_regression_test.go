package serverplugin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPublishApprovalBindsWholeManifestAcrossABA(t *testing.T) {
	s, m, data, hits, source := newFixture(t)
	ctx := context.Background()
	original := *m
	if _, err := s.Import(ctx, "duckllo", source); err != nil {
		t.Fatal(err)
	}
	firstDigest := currentApproval(s)
	*data = []byte("candidate B")
	*m = fixtureManifest(*data)
	if _, err := s.Check(ctx, "duckllo"); err != nil {
		t.Fatal(err)
	}
	// The publisher reuses A's version while changing its payload and description.
	*data = []byte("changed candidate A")
	*m = fixtureManifest(*data)
	m.Version = original.Version
	m.Description = "changed A"
	if _, err := s.Check(ctx, "duckllo"); err != nil {
		t.Fatal(err)
	}
	before := hits.Load()
	if _, err := s.Publish(ctx, "duckllo", original.Version, firstDigest); err == nil {
		t.Fatal("stale A approval published changed A")
	}
	if hits.Load() != before {
		t.Fatal("stale approval fetched before rejection")
	}
	if currentApproval(s) == firstDigest {
		t.Fatal("whole-manifest digest failed to change")
	}
	if _, err := s.Publish(ctx, "duckllo", original.Version, currentApproval(s)); err != nil {
		t.Fatal(err)
	}
}
func TestPrivateRootRejectsForeignOwnedAncestorAndValidatesDescriptor(t *testing.T) {
	t.Run("foreign ancestor", func(t *testing.T) {
		if os.Geteuid() != 0 {
			t.Skip("changing directory ownership requires root")
		}
		dir := filepath.Join(t.TempDir(), "foreign")
		if err := os.Mkdir(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(dir, 65534, 65534); err != nil {
			t.Skipf("chown unavailable: %v", err)
		}
		t.Cleanup(func() {
			if err := os.Chown(dir, os.Geteuid(), os.Getegid()); err != nil {
				t.Errorf("restore owner: %v", err)
			}
		})
		if root, err := privateRoot(filepath.Join(dir, "plugins")); err == nil {
			root.Close()
			t.Fatal("foreign-owned nonwritable ancestor accepted")
		}
		if _, err := os.Lstat(filepath.Join(dir, "plugins")); !os.IsNotExist(err) {
			t.Fatal("created cache inside untrusted ancestor")
		}
	})
	t.Run("descriptor", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Chmod(dir, 0700); err != nil {
			t.Fatal(err)
		}
		root, err := privateRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = root.Close() })
		expected, err := os.Lstat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if validateRoot(root, expected) != nil {
			t.Fatal("valid descriptor rejected")
		}
		other, err := os.Stat(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if validateRoot(root, other) == nil {
			t.Fatal("mismatched opened descriptor accepted")
		}
		if err := os.Chmod(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if validateRoot(root, expected) == nil {
			t.Fatal("opened root permissions not revalidated")
		}
	})
	t.Run("trusted sticky tmp", func(t *testing.T) {
		root, err := privateRoot(filepath.Join(t.TempDir(), "plugins"))
		if err != nil {
			t.Fatal(err)
		}
		_ = root.Close()
	})
}

func TestUpstreamCheckLeavesCacheReportsAndDisableResponsive(t *testing.T) {
	data := []byte("published bytes")
	manifest := fixtureManifest(data)
	var block atomic.Bool
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/manifest" {
			if block.Load() {
				select {
				case entered <- struct{}{}:
				default:
				}
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
			_ = json.NewEncoder(w).Encode(manifest)
			return
		}
		_, _ = w.Write(data)
	}))
	t.Cleanup(server.Close)
	s, err := New(t.TempDir(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	published, err := publishFixture(s, context.Background(), "duckllo", server.URL+"/manifest")
	if err != nil {
		t.Fatal(err)
	}
	block.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := s.Check(ctx, "duckllo"); done <- err }()
	t.Cleanup(func() {
		cancel()
		unblock()
		select {
		case err := <-done:
			if err != nil {
				t.Logf("worker exit after teardown: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("check worker did not exit")
		}
	})
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("source check did not begin")
	}
	short, cancelShort := context.WithTimeout(context.Background(), time.Second)
	defer cancelShort()
	if list, err := s.Catalog(short); err != nil || len(list) != 1 {
		t.Fatalf("hung source blocked catalog: %v %v", list, err)
	}
	if _, err := s.Manifest(short, "duckllo"); err != nil {
		t.Fatalf("hung source blocked manifest: %v", err)
	}
	f, _, err := s.Artifact(short, "duckllo", published.Version, "linux", "amd64")
	if err != nil {
		t.Fatalf("hung source blocked artifact: %v", err)
	}
	got, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil || string(got) != string(data) {
		t.Fatal("cached bytes changed")
	}
	if err := s.ReceiveReport(short, "client-a", Report{APIVersion: 1, Plugins: []PluginStatus{}}); err != nil {
		t.Fatalf("hung source blocked report: %v", err)
	}
	if err := s.SetEnabled(short, "duckllo", false); err != nil {
		t.Fatalf("hung source blocked disable: %v", err)
	}
	unblock()
	select {
	case err := <-done:
		done <- err
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("check did not finish")
	}
	entry, err := s.snapshot(context.Background(), "duckllo")
	if err != nil || entry.Enabled {
		t.Fatalf("check overwrote concurrent disable: %+v %v", entry, err)
	}
}
func TestDisableWinsOverSlowPublish(t *testing.T) {
	oldData := []byte("old release")
	oldManifest := fixtureManifest(oldData)
	nextData := []byte("new release")
	nextManifest := fixtureManifest(nextData)
	var next atomic.Bool
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/manifest" {
			if next.Load() {
				_ = json.NewEncoder(w).Encode(nextManifest)
			} else {
				_ = json.NewEncoder(w).Encode(oldManifest)
			}
			return
		}
		if next.Load() {
			entered <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			_, _ = w.Write(nextData)
		} else {
			_, _ = w.Write(oldData)
		}
	}))
	t.Cleanup(server.Close)
	s, err := New(t.TempDir(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publishFixture(s, context.Background(), "duckllo", server.URL+"/manifest"); err != nil {
		t.Fatal(err)
	}
	next.Store(true)
	if _, err := s.Check(context.Background(), "duckllo"); err != nil {
		t.Fatal(err)
	}
	digest := currentApproval(s)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := s.Publish(ctx, "duckllo", nextManifest.Version, digest); done <- err }()
	t.Cleanup(func() {
		cancel()
		unblock()
		select {
		case err := <-done:
			if err != nil {
				t.Logf("worker exit after teardown: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("publish worker did not exit")
		}
	})
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("artifact fetch did not begin")
	}
	short, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if catalog, err := s.Catalog(short); err != nil || len(catalog) != 1 {
		t.Fatalf("slow publish blocked cached catalog: %v %v", catalog, err)
	}
	if err := s.ReceiveReport(short, "client-a", Report{APIVersion: 1, Plugins: []PluginStatus{}}); err != nil {
		t.Fatalf("slow publish blocked report: %v", err)
	}
	if err := s.SetEnabled(short, "duckllo", false); err != nil {
		t.Fatalf("slow publish blocked disable: %v", err)
	}
	unblock()
	select {
	case err := <-done:
		done <- err
		if err == nil {
			t.Fatal("publish ignored concurrent disable")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("publish did not finish")
	}
	e, err := s.snapshot(context.Background(), "duckllo")
	if err != nil || e.Enabled || e.Current != oldManifest.Version {
		t.Fatalf("publish overrode disabled old release: %+v %v", e, err)
	}
}
func TestSchedulerPollingHonorsRegistrationDueTime(t *testing.T) {
	s, _, _, hits, source := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if _, err := s.Import(ctx, "duckllo", source); err != nil {
		t.Fatal(err)
	}
	e, err := s.snapshot(ctx, "duckllo")
	if err != nil {
		t.Fatal(err)
	}
	due, err := time.Parse(time.RFC3339Nano, e.NextCheckAt)
	if err != nil {
		t.Fatal(err)
	}
	if poll := checkPollInterval(DefaultCheckInterval); poll > time.Minute {
		t.Fatalf("poll=%v allows multi-hour delay", poll)
	}
	// Inject timer ticks as a fake clock. A tick just before due must skip, while
	// the next polling tick must check even though no second six-hour tick elapsed.
	ticks := make(chan time.Time)
	finished := make(chan struct{})
	go func() { defer close(finished); runChecker(ctx, s, ticks) }()
	t.Cleanup(func() { cancel(); <-finished })
	ticks <- due.Add(-time.Second)
	ticks <- due.Add(checkPollInterval(s.interval))
	close(ticks)
	<-finished
	if hits.Load() != 2 {
		t.Fatalf("scheduler requests=%d, wanted registration plus first due poll", hits.Load())
	}
	if summary := e.AdminSummary(); summary["available_manifest_digest"] != currentApproval(s) {
		t.Fatal("admin digest absent")
	}
}
