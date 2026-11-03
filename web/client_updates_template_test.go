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
		`class="cl-subtabs"`, `href="/admin/clients"`, `href="/admin/clients/rollouts"`, "Rollouts",
		`role="progressbar"`, "askCancel", "ro-drawer", "DW.live.on('rollouts'",
		`href="/admin/clients?updates=legacy"`, `href="/admin/clients?updates=eligible"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("client updates template missing %q", want)
		}
	}
}

func TestClientsTemplatesContainManagedUpdatePolicy(t *testing.T) {
	for file, wants := range map[string][]string{
		"templates/clients.html": {
			`href="/admin/clients/rollouts"`, `href="/admin/clients/new"`, `class="cl-subtabs"`, "Needs update", "Install instructions", "managed",
		},
		"templates/client_detail.html": {
			"ce-managed", "update_policy", "Manual updates", "Managed updates",
		},
	} {
		body, err := Content.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range wants {
			if !strings.Contains(string(body), want) {
				t.Errorf("%s missing %q", file, want)
			}
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
