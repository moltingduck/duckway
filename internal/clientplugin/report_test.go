package clientplugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func lastReport(t *testing.T, f *fixture) Report {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reportBodies) == 0 {
		t.Fatal("no plugin report received")
	}
	data := f.reportBodies[len(f.reportBodies)-1]
	if bytes.Contains(data, []byte("fixture-client-token")) || bytes.Contains(data, []byte("top-secret-error")) {
		t.Fatal("secret entered report body")
	}
	var report Report
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if err := report.Validate(); err != nil {
		t.Fatal(err)
	}
	return report
}

func TestInventoryReportsDurableManualOutcomeAndRetries(t *testing.T) {
	f := newFixture(t)
	installed := f.install(t)
	if err := f.host.Report(context.Background()); err != nil {
		t.Fatal(err)
	}
	initial := lastReport(t, f)
	if len(initial.Plugins) != 1 || initial.Plugins[0].Version != installed.Manifest.Version || initial.Plugins[0].LastUpdate != nil {
		t.Fatalf("initial report fabricated update success: %+v", initial)
	}
	if err := f.host.RecordOperation(context.Background(), "duckllo", "install", nil); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.reportStatus = 503
	f.mu.Unlock()
	if err := f.host.Report(context.Background()); err == nil {
		t.Fatal("report delivery failure was hidden")
	}
	before, err := os.ReadFile(filepath.Join(f.host.root(), "duckllo", "current.json"))
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.reportStatus = 0
	f.mu.Unlock()
	// Recreate the host to prove retry reads durable state across invocations.
	restarted := Host{ConfigDir: f.host.ConfigDir, ServerURL: f.host.ServerURL, Token: f.host.Token}
	if err := restarted.Report(context.Background()); err != nil {
		t.Fatal(err)
	}
	succeeded := lastReport(t, f).Plugins[0].LastUpdate
	if succeeded == nil || succeeded.Status != "succeeded" || succeeded.Operation != "install" || succeeded.ErrorCode != "" {
		t.Fatalf("install result: %+v", succeeded)
	}
	if err := restarted.Report(context.Background()); err != nil {
		t.Fatal(err)
	}
	if lastReport(t, f).Plugins[0].LastUpdate.ID != succeeded.ID {
		t.Fatal("periodic replay generated a new operation identity")
	}
	if err := f.host.RecordOperation(context.Background(), "duckllo", "update", fmt.Errorf("%w: top-secret-error", ErrVerificationFailed)); err != nil {
		t.Fatal(err)
	}
	if err := f.host.Report(context.Background()); err != nil {
		t.Fatal(err)
	}
	failed := lastReport(t, f).Plugins[0]
	if failed.Version != installed.Manifest.Version || failed.LastUpdate.Status != "failed" || failed.LastUpdate.ErrorCode != "verification_failed" || failed.LastUpdate.ID == succeeded.ID {
		t.Fatalf("failed result: %+v", failed)
	}
	after, _ := os.ReadFile(filepath.Join(f.host.root(), "duckllo", "current.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("report failure or result recording changed activation")
	}
}

func TestFailedFirstInstallAndOriginFiltering(t *testing.T) {
	f := newFixture(t)
	f.mu.Lock()
	f.status = 502
	f.mu.Unlock()
	_, operationErr := f.host.Install(context.Background(), "duckllo")
	if operationErr == nil {
		t.Fatal("fixture install unexpectedly succeeded")
	}
	if err := f.host.RecordOperation(context.Background(), "duckllo", "install", operationErr); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.status = 200
	f.mu.Unlock()
	if err := f.host.Report(context.Background()); err != nil {
		t.Fatal(err)
	}
	failed := lastReport(t, f).Plugins[0]
	if failed.Version != "" || failed.LastUpdate.Status != "failed" || failed.LastUpdate.ErrorCode != "download_failed" {
		t.Fatalf("failed first install: %+v", failed)
	}
	f.install(t)
	other := newFixture(t)
	changed := f.host
	changed.ServerURL = other.server.URL
	if err := changed.RecordOperation(context.Background(), "duckllo", "update", ErrServerChanged); !errors.Is(err, ErrServerChanged) {
		t.Fatalf("origin mismatch was recorded: %v", err)
	}
	if err := changed.Report(context.Background()); err != nil {
		t.Fatal(err)
	}
	other.mu.Lock()
	otherReports := len(other.reportBodies)
	other.mu.Unlock()
	if otherReports != 0 {
		t.Fatal("contacted another server with an empty inventory")
	}
}

func TestEmptySnapshotStillClearsPriorManagedInventory(t *testing.T) {
	f := newFixture(t)
	f.install(t)
	if err := f.host.RecordOperation(context.Background(), "duckllo", "install", nil); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(f.host.root(), "duckllo")); err != nil {
		t.Fatal(err)
	}
	if err := f.host.Report(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := lastReport(t, f); got.Plugins == nil || len(got.Plugins) != 0 {
		t.Fatalf("managed history did not send an empty inventory snapshot: %+v", got)
	}
}

type reportTransport func(*http.Request) (*http.Response, error)

func (f reportTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPlainHTTPPublicServerRejectedBeforeTokenSend(t *testing.T) {
	var calls atomic.Int32
	host := Host{ConfigDir: filepath.Join(t.TempDir(), "config"), ServerURL: "http://public.example.invalid", Token: "top-secret-token", HTTPClient: &http.Client{Transport: reportTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, fmt.Errorf("unexpected request")
	})}}
	if _, err := host.Catalog(context.Background()); err == nil {
		t.Fatal("plaintext public catalog allowed")
	}
	if _, err := host.Install(context.Background(), "duckllo"); err == nil {
		t.Fatal("plaintext public install allowed")
	}
	if err := host.Report(context.Background()); err == nil {
		t.Fatal("plaintext public report allowed")
	}
	if calls.Load() != 0 {
		t.Fatal("token sent before rejecting plaintext public server")
	}
	for _, raw := range []string{"http://localhost:18080", "http://127.0.0.1:18080", "http://[::1]:18080", "https://public.example.invalid"} {
		if _, err := ServerOrigin(raw); err != nil {
			t.Errorf("supported origin rejected: %s: %v", raw, err)
		}
	}
}

func TestReportNetworkDoesNotHoldRegistryLock(t *testing.T) {
	f := newFixture(t)
	f.install(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	f.host.HTTPClient = &http.Client{Transport: reportTransport(func(r *http.Request) (*http.Response, error) {
		close(entered)
		select {
		case <-release:
			return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader(""))}, nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- f.host.Report(ctx) }()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("report did not enter transport: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("report did not reach transport")
	}
	recordCtx, recordCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	err := f.host.RecordOperation(recordCtx, "duckllo", "update", nil)
	recordCancel()
	close(release)
	if reportErr := <-done; reportErr != nil {
		t.Fatal(reportErr)
	}
	if err != nil {
		t.Fatalf("network reporting held registry activation lock: %v", err)
	}
	lock, err := f.host.lockNamed(context.Background(), ".report-send.lock", true)
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	err = f.host.Report(waitCtx)
	waitCancel()
	unlock(lock)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("send lock not cancellable: %v", err)
	}
}

func TestResultPersistenceFailurePreservesActivation(t *testing.T) {
	f := newFixture(t)
	p := f.install(t)
	outside := filepath.Join(t.TempDir(), "outside")
	mustWrite(t, outside, []byte("unchanged"), 0600)
	if err := os.Symlink(outside, filepath.Join(f.host.root(), ".report-state.json")); err != nil {
		t.Fatal(err)
	}
	if err := f.host.RecordOperation(context.Background(), "duckllo", "update", nil); err == nil {
		t.Fatal("unsafe local status path accepted")
	}
	installed, err := f.host.List()
	if err != nil || len(installed) != 1 || installed[0].Release != p.Release {
		t.Fatalf("status failure changed activation: %v %v", installed, err)
	}
	data, _ := os.ReadFile(outside)
	if string(data) != "unchanged" {
		t.Fatal("status persistence followed symlink")
	}
}
