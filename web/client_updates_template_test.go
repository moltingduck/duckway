package web

import (
	"strings"
	"testing"
)

func TestClientUpdatesTemplateContainsRolloutControls(t *testing.T) {
	body, err := Content.ReadFile("templates/client_updates.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(body)
	for _, want := range []string{
		"/api/client-updates", "rollout-concurrency", "rollout-interval", "rollout-failure-threshold",
		"retry-failed", "skipped_manual", "manual_required", "managed_update_v1", "runtimeFresh",
		`class="client-tab active"`, `href="/admin/clients"`, "Updates",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("client updates template missing %q", want)
		}
	}
}

func TestClientsTemplateContainsManagedUpdatePolicy(t *testing.T) {
	body, err := Content.ReadFile("templates/clients.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(body)
	for _, want := range []string{
		"ce-managed-updates", "update_policy", "Manual updates", "Managed updates",
		`class="client-tab active"`, `href="/admin/client-updates"`, "Details",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("clients template missing %q", want)
		}
	}
}

func TestClientUpdatesRemovedFromSidebar(t *testing.T) {
	body, err := Content.ReadFile("templates/layout.html")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), `href="/admin/client-updates"`) {
		t.Fatal("client updates should be reachable through the Clients tabs, not the sidebar")
	}
}
