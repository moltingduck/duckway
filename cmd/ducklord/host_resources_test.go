package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/protocol"
	"github.com/hackerduck/duckway/internal/ducklord"
)

func TestPublishHostResourceEventDoesNotBlockWhenCompletionIsStale(t *testing.T) {
	done := make(chan hostResourceEvent, 1)
	done <- hostResourceEvent{id: 1}

	finished := make(chan struct{})
	go func() {
		publishHostResourceEvent(context.Background(), done, hostResourceEvent{id: 2})
		close(finished)
	}()

	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("stale resource completion blocked on a full channel")
	}
	if got := (<-done).id; got != 1 {
		t.Fatalf("queued completion changed: got id %d", got)
	}
}

func TestHostResourcesRouteRefreshAndFocusRestoration(t *testing.T) {
	s := testHostSkillsState()
	s.hostMenuIndex = 8
	if action := s.handleHostMenuInput([]byte("\r")); action != "host-resources-read" || s.hostMenuStep != "resources-loading" {
		t.Fatalf("open action=%q step=%q", action, s.hostMenuStep)
	}
	s.hostResourceStatus = protocol.HostResourceStatus{GOOS: "linux", GOARCH: "amd64", CPUCount: 4, HeapAllocBytes: 123, UptimeSeconds: 61, ManagedSessionCount: 2, ActivePTYCount: 1}
	s.hostMenuStep = "resources-view"
	var out bytes.Buffer
	s.renderHostModal(&out, 100, 24)
	for _, want := range []string{"linux / amd64", "CPU: 4", "123 bytes", "Sessions: 2 · PTYs: 1", "r refresh"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in %q", want, out.String())
		}
	}
	if action := s.handleHostMenuInput([]byte("r")); action != "host-resources-read" || s.hostMenuStep != "resources-loading" {
		t.Fatalf("refresh action=%q step=%q", action, s.hostMenuStep)
	}
	s.hostMenuStep = "resources-view"
	s.handleHostMenuInput([]byte("\x1b"))
	if s.hostMenuStep != "actions" || s.hostMenuIndex != 8 {
		t.Fatalf("restore step=%q index=%d", s.hostMenuStep, s.hostMenuIndex)
	}
}

func TestHostResourcesRejectsStaleResponseAfterHostReplacement(t *testing.T) {
	s := &tuiState{
		hostMenuMode:      true,
		hostMenuTarget:    "host-a",
		hostMenuRequestID: 7,
		hostMenuStep:      "resources-loading",
		hostSync:          map[string]ducklord.SessionUpdate{"host-a": {InstanceID: "new-instance"}},
		disconnectedHosts: map[string]bool{},
	}
	event := hostResourceEvent{id: 7, host: "host-a", epoch: 3, instanceID: "old-instance"}
	if s.acceptHostResourceEvent(event, 4) {
		t.Fatal("accepted resource response from the old host epoch")
	}
	event.epoch, event.instanceID = 4, "new-instance"
	if !s.acceptHostResourceEvent(event, 4) {
		t.Fatal("rejected resource response from the current host instance")
	}
}

func TestHostResourcesDelayedHostSwitchCannotOverwriteSelectedHost(t *testing.T) {
	const (
		hostA = "host-a"
		hostB = "host-b"
	)
	s := &tuiState{
		hostMenuMode:      true,
		hostMenuTarget:    hostA,
		hostMenuRequestID: 11,
		hostMenuStep:      "resources-loading",
		hostSync: map[string]ducklord.SessionUpdate{
			hostA: {InstanceID: "a-instance"},
			hostB: {InstanceID: "b-instance"},
		},
		disconnectedHosts: map[string]bool{},
	}
	done := make(chan hostResourceEvent, 2)
	// The worker for A finishes after the user has selected B. The explicit
	// gates make the completion order deterministic while retaining the real
	// non-blocking dispatcher path.
	aReleased := make(chan struct{})
	go func() {
		<-aReleased
		publishHostResourceEvent(context.Background(), done, hostResourceEvent{
			id: 11, epoch: 1, instanceID: "a-instance", host: hostA,
			status: protocol.HostResourceStatus{GOOS: "linux", CPUCount: 1},
		})
	}()
	bReleased := make(chan struct{})
	go func() {
		<-bReleased
		publishHostResourceEvent(context.Background(), done, hostResourceEvent{
			id: 12, epoch: 1, instanceID: "b-instance", host: hostB,
			status: protocol.HostResourceStatus{GOOS: "freebsd", CPUCount: 2},
		})
	}()

	// Switching hosts invalidates A's request before either completion is read.
	s.hostMenuTarget, s.hostMenuRequestID = hostB, 12
	s.hostMenuStep = "resources-loading"
	close(aReleased)
	event := <-done
	if s.acceptHostResourceEvent(event, 1) {
		s.hostResourceStatus = event.status
		t.Fatal("stale Host A completion was accepted after switching to Host B")
	}
	close(bReleased)
	event = <-done
	if !s.acceptHostResourceEvent(event, 1) {
		t.Fatal("current Host B completion was rejected")
	}
	s.hostResourceStatus = event.status
	if s.hostResourceStatus.GOOS != "freebsd" || s.hostResourceStatus.CPUCount != 2 {
		t.Fatalf("selected Host B status was not applied: %+v", s.hostResourceStatus)
	}
}
