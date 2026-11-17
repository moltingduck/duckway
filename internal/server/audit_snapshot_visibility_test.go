package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/audit"
	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/server/handlers"
)

// Identifier fields (api_key_id, service_id, placeholder_id ...) stay visible in
// every snapshot type while real secrets are masked.
func TestAuditSnapshotsKeepIDsVisibleAndMaskSecrets(t *testing.T) {
	e := newNtEnv(t)
	svc := e.must(e.do("POST", "/api/services", map[string]any{"name": "vis", "upstream_url": "https://api.vis.example"}), 200, 201)
	key := e.must(e.do("POST", "/api/keys", map[string]any{"service_id": svc["id"], "name": "k", "key": "sk-visible-SECRETVALUE-123456"}), 200, 201)
	cl := e.must(e.do("POST", "/api/clients", map[string]any{"name": "c"}), 200, 201)
	ph := e.must(e.do("POST", "/api/placeholders", map[string]any{"env_name": "VIS_TOKEN", "service_id": svc["id"], "client_id": cl["id"], "api_key_id": key["id"]}), 200, 201)
	grp := e.must(e.do("POST", "/api/groups", map[string]any{"name": "g", "service_id": svc["id"], "strategy": "round_robin"}), 200, 201)
	nt := e.must(e.do("POST", "/api/notifications", map[string]any{"channel_type": "discord_bot", "name": "n",
		"config": `{"bot_token":"BOT-SECRET-TOKEN-999","channel_id":"42","approver_user_ids":"7"}`}), 200, 201)
	e.s.db.Exec(`INSERT INTO approvals (id, placeholder_id, status) VALUES ('ap1', ?, 'pending')`, ph["id"])
	aq := queries.NewApproverTokenQueries(e.s.db)
	if err := aq.Create("at1", "tui", "HASH-OF-APPROVER-TOKEN"); err != nil {
		t.Fatal(err)
	}
	if err := queries.NewSettingsQueries(e.s.db).Set("webhook_secret_x", "SETTING-SECRET-777"); err != nil {
		t.Fatal(err)
	}
	snaps := &handlers.AuditSnapshots{
		Services: queries.NewServiceQueries(e.s.db), APIKeys: queries.NewAPIKeyQueries(e.s.db), Placeholders: queries.NewPlaceholderQueries(e.s.db),
		Groups: queries.NewGroupQueries(e.s.db), Clients: queries.NewClientQueries(e.s.db), Notifications: queries.NewNotificationQueries(e.s.db),
		Approvals: queries.NewApprovalQueries(e.s.db), Approvers: aq, Settings: queries.NewSettingsQueries(e.s.db),
	}
	cases := []struct {
		typ, id    string
		visible    []string
		secretText []string
	}{
		{"service", svc["id"].(string), []string{`"id"`}, nil},
		{"api_key", key["id"].(string), []string{`"service_id":"` + svc["id"].(string) + `"`}, []string{"SECRETVALUE"}},
		{"placeholder", ph["id"].(string), []string{`"api_key_id":"` + key["id"].(string) + `"`, `"service_id"`, `"client_id":"` + cl["id"].(string) + `"`}, nil},
		{"group", grp["id"].(string), []string{`"service_id":"` + svc["id"].(string) + `"`}, nil},
		{"client", cl["id"].(string), []string{`"id"`}, []string{"token_hash\":\"$", "$2a$"}},
		{"notification", nt["id"].(string), []string{`"channel_id":"42"`, `"approver_user_ids":"7"`}, []string{"BOT-SECRET-TOKEN-999"}},
		{"approval", "ap1", []string{`"placeholder_id":"` + ph["id"].(string) + `"`}, nil},
		{"approver_token", "at1", []string{`"id":"at1"`}, []string{"HASH-OF-APPROVER-TOKEN"}},
		{"setting", "", nil, []string{"SETTING-SECRET-777"}},
	}
	for _, c := range cases {
		_, v, ok := snaps.Get(c.typ, c.id)
		if !ok {
			t.Fatalf("%s: no snapshot", c.typ)
		}
		_, after := audit.RedactPair(nil, v)
		var m map[string]any
		if err := json.Unmarshal([]byte(after), &m); err != nil {
			t.Fatalf("%s: %v", c.typ, err)
		}
		for _, want := range c.visible {
			if !strings.Contains(after, want) {
				t.Errorf("%s: %s not visible in %s", c.typ, want, after)
			}
		}
		for _, bad := range c.secretText {
			if strings.Contains(after, bad) {
				t.Errorf("%s: secret %q leaked in %s", c.typ, bad, after)
			}
		}
	}
	// key_encrypted and token_hash are masked, not dropped.
	_, kv, _ := snaps.Get("api_key", key["id"].(string))
	_, ka := audit.RedactPair(nil, kv)
	if !strings.Contains(ka, `"key_encrypted":"(set)"`) && !strings.Contains(ka, `"key_encrypted":"(changed)"`) {
		t.Errorf("key_encrypted should be masked: %s", ka)
	}
}
