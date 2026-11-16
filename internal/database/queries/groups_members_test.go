package queries_test

import (
	"testing"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
)

// Regression: GetMembers must load the whole key row the proxy relies on. It
// used to omit acl and upstream_proxy_url, so a key reached through a legacy
// key group silently lost its own ACL layer and outbound proxy.
func TestGroupGetMembersLoadsACLAndUpstreamProxy(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	exec := func(q string, args ...interface{}) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	exec(`INSERT INTO services (id, name, display_name, upstream_url, host_pattern) VALUES ('svc-gm', 'gm', 'GM', 'https://gm.example', 'gm.example')`)
	exec(`INSERT INTO api_keys (id, service_id, name, key_encrypted, acl, upstream_proxy_url, token_endpoint, subscription_info) VALUES ('k-gm', 'svc-gm', 'gm-key', 'x', '{"rules":[]}', 'enc-proxy', 'https://t.example/token', '{"a":1}')`)
	exec(`INSERT INTO api_keys (id, service_id, name, key_encrypted, is_active) VALUES ('k-off', 'svc-gm', 'off', 'x', 0)`)
	exec(`INSERT INTO api_key_groups (id, service_id, name, strategy) VALUES ('g-gm', 'svc-gm', 'grp', 'failover')`)
	exec(`INSERT INTO api_key_group_members (group_id, api_key_id, priority) VALUES ('g-gm', 'k-gm', 1), ('g-gm', 'k-off', 2)`)

	got, err := queries.NewGroupQueries(db).GetMembers("g-gm")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("members: %d, want 1 active", len(got))
	}
	k := got[0]
	if k.ACL != `{"rules":[]}` || k.UpstreamProxyURL != "enc-proxy" || k.TokenEndpoint != "https://t.example/token" || k.SubscriptionInfo != `{"a":1}` || k.ServiceName != "gm" {
		t.Fatalf("member row incomplete: %+v", k)
	}
}
