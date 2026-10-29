package serverplugin

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestFleetReportsUseReceiptTimeAndStableAttemptID(t *testing.T) {
	s, _, _, _, source := newFixture(t)
	ctx := context.Background()
	m, err := publishFixture(s, ctx, "duckllo", source)
	if err != nil {
		t.Fatal(err)
	}
	clients := []ClientIdentity{{ID: "client-a", Name: "Alpha"}, {ID: "client-b", Name: "Beta"}}
	states, err := s.ClientStates(ctx, "duckllo", clients, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if states[0].Status != "unknown" || states[1].Status != "unknown" {
		t.Fatal("missing reports assumed current")
	}
	success := Report{APIVersion: 1, Plugins: []PluginStatus{{Name: "duckllo", Version: m.Version, LastUpdate: &UpdateResult{ID: strings.Repeat("a", 32), Operation: "install", Status: "succeeded"}}}}
	if err := s.ReceiveReport(ctx, "client-a", success); err != nil {
		t.Fatal(err)
	}
	states, err = s.ClientStates(ctx, "duckllo", clients, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if states[0].Status != "up_to_date" || states[0].SeenAt == "" || states[0].LastUpdateAt == "" || states[1].Status != "unknown" {
		t.Fatalf("fleet=%+v", states)
	}
	firstAt := states[0].LastUpdateAt
	if err := s.ReceiveReport(ctx, "client-a", success); err != nil {
		t.Fatal(err)
	}
	states, _ = s.ClientStates(ctx, "duckllo", clients, time.Now())
	if states[0].LastUpdateAt != firstAt {
		t.Fatal("repeated snapshot reset attempt timestamp")
	}
	changed := success
	changed.Plugins = append([]PluginStatus(nil), success.Plugins...)
	altered := *changed.Plugins[0].LastUpdate
	altered.Status = "failed"
	altered.ErrorCode = "download_failed"
	changed.Plugins[0].LastUpdate = &altered
	if err := s.ReceiveReport(ctx, "client-a", changed); err == nil {
		t.Fatal("same attempt ID changed content")
	}
	failed := changed
	failed.Plugins[0].LastUpdate.ID = strings.Repeat("b", 32)
	failed.Plugins[0].Version = "sha256-" + strings.Repeat("0", 64)
	if err := s.ReceiveReport(ctx, "client-a", failed); err != nil {
		t.Fatal(err)
	}
	states, _ = s.ClientStates(ctx, "duckllo", clients, time.Now())
	if states[0].Status != "update_failed" {
		t.Fatalf("failure hidden: %+v", states[0])
	}
	states, _ = s.ClientStates(ctx, "duckllo", clients, time.Now().Add(ReportFreshness+time.Minute))
	if states[0].Status != "stale" {
		t.Fatal("old report assumed fresh")
	}
	different := Report{APIVersion: 1, Plugins: []PluginStatus{{Name: "duckllo", Version: "sha256-" + strings.Repeat("0", 64)}}}
	if err := s.ReceiveReport(ctx, "client-a", different); err != nil {
		t.Fatal(err)
	}
	states, _ = s.ClientStates(ctx, "duckllo", clients, time.Now())
	if states[0].Status != "different" {
		t.Fatal("different hash interpreted current")
	}
	if err := s.ReceiveReport(ctx, "client-a", Report{APIVersion: 1, Plugins: []PluginStatus{}}); err != nil {
		t.Fatal(err)
	}
	states, _ = s.ClientStates(ctx, "duckllo", clients, time.Now())
	if states[0].Status != "not_installed" {
		t.Fatal("empty inventory assumed installed")
	}
}
func TestReportValidation(t *testing.T) {
	version := "sha256-" + strings.Repeat("a", 64)
	bad := []Report{
		{APIVersion: 0, Plugins: []PluginStatus{}}, {APIVersion: 1, Plugins: nil},
		{APIVersion: 1, Plugins: []PluginStatus{{Name: "duckllo", Version: version}, {Name: "duckllo", Version: version}}},
		{APIVersion: 1, Plugins: []PluginStatus{{Name: "../escape", Version: version}}},
		{APIVersion: 1, Plugins: []PluginStatus{{Name: "duckllo", Version: "1.2.3"}}},
		{APIVersion: 1, Plugins: []PluginStatus{{Name: "duckllo", Version: ""}}},
		{APIVersion: 1, Plugins: []PluginStatus{{Name: "duckllo", Version: version, LastUpdate: &UpdateResult{ID: "bad", Operation: "update", Status: "succeeded"}}}},
		{APIVersion: 1, Plugins: []PluginStatus{{Name: "duckllo", Version: version, LastUpdate: &UpdateResult{ID: strings.Repeat("a", 32), Operation: "update", Status: "failed", ErrorCode: "https://secret.invalid/?token=x"}}}},
	}
	for i, r := range bad {
		if r.Validate() == nil {
			t.Errorf("invalid report %d accepted", i)
		}
	}
	valid := Report{APIVersion: 1, Plugins: []PluginStatus{{Name: "duckllo", Version: "", LastUpdate: &UpdateResult{ID: strings.Repeat("a", 32), Operation: "install", Status: "failed", ErrorCode: "verification_failed"}}}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("failed initial install rejected: %v", err)
	}
}
