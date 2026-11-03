package handlers_test

import (
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/database/queries"
)

// Held and ACL-denied rows (the gateway's own 403s) and client cancels are not
// handled traffic: they stay out of the request total, the error count and the
// hourly trend. A 502 (upstream unreachable) is an error.
func TestOverviewExcludesHeldDeniedAndCancelledRows(t *testing.T) {
	db := overviewDB(t)
	ovExec(t, db, `INSERT INTO services (id, name, display_name, upstream_url, host_pattern) VALUES ('s1', 'svc', 'S', 'https://e.com', 'e.com')`)
	ins := func(code int, decision string, ago time.Duration) {
		ovExec(t, db, `INSERT INTO request_log (service_name, method, path, status_code, decision, created_at) VALUES ('s', 'GET', '/', ?, ?, ?)`, code, decision, ts(-ago))
	}
	ins(200, "allowed", time.Minute)
	ins(200, "approved_forward", time.Minute)
	ins(500, "allowed", time.Minute)
	ins(502, "allowed", time.Minute) // upstream unreachable
	ins(403, "held", time.Minute)
	ins(403, "held", time.Minute)
	ins(403, "acl_denied", time.Minute)
	ins(0, "allowed", time.Minute) // client cancelled
	r := getOverview(t, db)
	if r.Stats.Requests24h != 4 || r.Stats.Errors24h != 2 {
		t.Fatalf("requests=%d errors=%d, want 4 handled requests and 2 errors", r.Stats.Requests24h, r.Stats.Errors24h)
	}
	sum := 0
	for _, n := range r.RequestsPerHour {
		sum += n
	}
	if sum != 4 {
		t.Fatalf("trend sum=%d, want 4: %v", sum, r.RequestsPerHour)
	}
}

// The "require approval" count parses the JSON, so spacing and key order do not matter.
func TestOverviewApprovalTokenCountIsJSONRobust(t *testing.T) {
	db := overviewDB(t)
	ovExec(t, db, `INSERT INTO services (id, name, display_name, upstream_url, host_pattern) VALUES ('s1', 'svc', 'S', 'https://e.com', 'e.com')`)
	ovExec(t, db, `INSERT INTO clients (id, name, token_hash) VALUES ('c1', 'c', 'h')`)
	ovExec(t, db, `INSERT INTO api_keys (id, service_id, name, key_encrypted) VALUES ('k1', 's1', 'plain', 'x')`)
	cfgs := map[string]string{
		"a": `{"rules":[{"endpoints":[{"method":"POST","path":"/x","approve":true}]}]}`,
		"b": `{ "rules" : [ { "endpoints" : [ { "approve"   :   true , "method":"POST","path":"/x" } ] } ] }`,
		"c": "{\n  \"rules\": [\n    {\n      \"endpoints\": [\n        {\"path\": \"/x\", \"method\": \"POST\", \"approve\":\ttrue}\n      ]\n    }\n  ]\n}",
		"d": `{"rules":[{"endpoints":[{"method":"GET","path":"/x","allow":true}]}]}`,
		"e": `{"rules":[{"endpoints":[{"method":"GET","path":"/x","approve":false}]}]}`,
		"f": `not json`,
	}
	n := 0
	for id, cfg := range cfgs {
		n++
		ovExec(t, db, `INSERT INTO placeholder_keys (id, env_name, placeholder, service_id, api_key_id, client_id, requires_approval, permission_config) VALUES (?, ?, ?, 's1', 'k1', 'c1', 0, ?)`,
			"p"+id, "E"+id, "ph"+id, cfg)
	}
	ovExec(t, db, `INSERT INTO placeholder_keys (id, env_name, placeholder, service_id, api_key_id, client_id, requires_approval) VALUES ('pw', 'EW', 'phw', 's1', 'k1', 'c1', 1)`)
	r := getOverview(t, db)
	if r.Stats.PhantomTokens != n+1 || r.Stats.PhantomRequireApproval != 4 {
		t.Fatalf("tokens=%d require approval=%d, want %d / 4 (a, b, c and the token-wide one)", r.Stats.PhantomTokens, r.Stats.PhantomRequireApproval, n+1)
	}
}

func TestRequestLogPhIndexExists(t *testing.T) {
	db := overviewDB(t)
	var name string
	if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'index' AND name = 'idx_request_log_ph_time'`).Scan(&name); err != nil {
		t.Fatalf("idx_request_log_ph_time missing: %v", err)
	}
	_ = queries.DefaultRequestLogRetentionDays
}
