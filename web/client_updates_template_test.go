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
	for _, want := range []string{"ce-managed-updates", "update_policy", "Manual updates", "Managed updates"} {
		if !strings.Contains(page, want) {
			t.Errorf("clients template missing %q", want)
		}
	}
}
