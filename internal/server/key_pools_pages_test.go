package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func poolsAPI(t *testing.T, s *Server, shared *SharedServices, method, path, body string) (*httptest.ResponseRecorder, map[string]interface{}) {
	t.Helper()
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(shared.AdminAuth.CreateSession("duckway", req))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	out := map[string]interface{}{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func TestKeyPoolAndBundlePagesRequireAdminAndRenderShell(t *testing.T) {
	s, shared := newShellTestServer(t)
	for _, p := range []string{"/admin/key-pools", "/admin/key-bundles", "/admin/key-pools?pool=rotation:nope"} {
		if rec := adminGet(s, shared, p, false); rec.Code == http.StatusOK {
			t.Errorf("%s: anonymous request rendered the page", p)
		}
		rec := adminGet(s, shared, p, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", p, rec.Code)
		}
		body := rec.Body.String()
		for _, want := range []string{`class="sidebar"`, `src="/static/shell.js"`, "</html>"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: missing %q", p, want)
			}
		}
		checkPage(t, rec)
	}
	body := adminGet(s, shared, "/admin/key-pools", true).Body.String()
	if !strings.Contains(body, `href="/admin/key-pools" class="active" aria-current="page"`) {
		t.Error("key pools nav item not marked active")
	}
	body = adminGet(s, shared, "/admin/key-bundles", true).Body.String()
	if !strings.Contains(body, `href="/admin/key-bundles" class="active" aria-current="page"`) {
		t.Error("key bundles nav item not marked active")
	}
}

func TestMovedKeyGroupDetailLinksToLeasePool(t *testing.T) {
	s, shared := newShellTestServer(t)
	rec := adminGet(s, shared, "/admin/key-groups/abc-123", true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`href="/admin/key-pools?pool=lease%3aabc-123"`, "Key pools", "/admin/key-groups/abc-123", "Update your bookmark"} {
		if !strings.Contains(strings.ToLower(body), strings.ToLower(want)) {
			t.Errorf("moved detail page missing %q", want)
		}
	}
	if anon := adminGet(s, shared, "/admin/key-groups/abc-123", false); anon.Code == http.StatusNotFound {
		t.Error("anonymous request got the moved page")
	}
	for _, old := range []string{"/admin/groups", "/admin/key-groups", "/admin/key-suites"} {
		if r := adminGet(s, shared, old, true); r.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404 moved notice", old, r.Code)
		}
	}
}

// Both pool mechanisms can be created, edited, populated, reordered and
// deleted through the admin APIs the Key pools page drives.
func TestKeyPoolsBothModesCreateEditMembersOrder(t *testing.T) {
	s, shared := newShellTestServer(t)
	exec := func(q string, args ...interface{}) {
		t.Helper()
		if _, err := s.db.Exec(q, args...); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	exec(`INSERT INTO services (id, name, display_name, upstream_url, host_pattern) VALUES ('svc-p', 'pool-svc', 'Pool Svc', 'https://p.example', 'p.example')`)
	for _, id := range []string{"kp1", "kp2", "kp3"} {
		exec(`INSERT INTO api_keys (id, service_id, name, key_encrypted) VALUES (?, 'svc-p', ?, 'x')`, id, "key "+id)
	}

	// Per-request rotation (api_key_groups)
	rec, rot := poolsAPI(t, s, shared, http.MethodPost, "/api/groups", `{"name":"<b>rot</b>","service_id":"svc-p","strategy":"failover"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create rotation: %d %s", rec.Code, rec.Body.String())
	}
	rid := rot["id"].(string)
	if rec, _ := poolsAPI(t, s, shared, http.MethodPut, "/api/groups/"+rid, `{"name":"rot2","strategy":"least-used"}`); rec.Code != http.StatusOK {
		t.Fatalf("update rotation: %d", rec.Code)
	}
	for i, k := range []string{"kp1", "kp2", "kp3"} {
		body := `{"api_key_id":"` + k + `","priority":` + string(rune('0'+i)) + `}`
		if rec, _ := poolsAPI(t, s, shared, http.MethodPost, "/api/groups/"+rid+"/members", body); rec.Code != http.StatusOK {
			t.Fatalf("add rotation member %s: %d", k, rec.Code)
		}
	}
	// Reorder: kp3 first (upsert rewrites priority).
	for i, k := range []string{"kp3", "kp1", "kp2"} {
		body := `{"api_key_id":"` + k + `","priority":` + string(rune('0'+i)) + `}`
		if rec, _ := poolsAPI(t, s, shared, http.MethodPost, "/api/groups/"+rid+"/members", body); rec.Code != http.StatusOK {
			t.Fatalf("reorder rotation member %s: %d", k, rec.Code)
		}
	}
	rec, got := poolsAPI(t, s, shared, http.MethodGet, "/api/groups/"+rid, "")
	members, _ := got["members"].([]interface{})
	if rec.Code != http.StatusOK || len(members) != 3 || members[0].(map[string]interface{})["id"] != "kp3" || got["strategy"] != "least-used" {
		t.Fatalf("rotation pool after edit/reorder: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "key_encrypted\":\"x") {
		t.Error("stored key material leaked in group response")
	}

	// Quota-aware lease (key_groups)
	rec, lease := poolsAPI(t, s, shared, http.MethodPost, "/api/key-groups", `{"name":"lease1","description":"d","service_name":"pool-svc","rotation_strategy":"random"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create lease: %d %s", rec.Code, rec.Body.String())
	}
	lid := lease["id"].(string)
	if rec, _ := poolsAPI(t, s, shared, http.MethodPatch, "/api/key-groups/"+lid, `{"name":"lease2","description":"e","rotation_strategy":"failover"}`); rec.Code != http.StatusOK {
		t.Fatalf("update lease: %d", rec.Code)
	}
	if rec, _ := poolsAPI(t, s, shared, http.MethodPatch, "/api/key-groups/"+lid, `{"name":"lease2","rotation_strategy":"bogus"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid lease strategy: %d, want 400", rec.Code)
	}
	for i, k := range []string{"kp1", "kp2"} {
		body := `{"api_key_id":"` + k + `","position":` + string(rune('0'+i)) + `}`
		if rec, _ := poolsAPI(t, s, shared, http.MethodPost, "/api/key-groups/"+lid+"/members", body); rec.Code != http.StatusOK {
			t.Fatalf("add lease member: %d", rec.Code)
		}
	}
	if rec, _ := poolsAPI(t, s, shared, http.MethodPatch, "/api/key-groups/"+lid+"/members/kp2", `{"position":0}`); rec.Code != http.StatusOK {
		t.Fatalf("reorder lease: %d", rec.Code)
	}
	if rec, _ := poolsAPI(t, s, shared, http.MethodPatch, "/api/key-groups/"+lid+"/members/kp1", `{"position":1}`); rec.Code != http.StatusOK {
		t.Fatalf("reorder lease: %d", rec.Code)
	}
	rec, got = poolsAPI(t, s, shared, http.MethodGet, "/api/key-groups/"+lid, "")
	members, _ = got["members"].([]interface{})
	if len(members) != 2 || members[0].(map[string]interface{})["api_key_id"] != "kp2" || got["rotation_strategy"] != "failover" {
		t.Fatalf("lease pool after edit/reorder: %s", rec.Body.String())
	}

	// The page shell deep-links either kind and both lists feed the page.
	for _, p := range []string{"/admin/key-pools?pool=rotation:" + rid, "/admin/key-pools?pool=lease:" + lid} {
		if rec := adminGet(s, shared, p, true); rec.Code != http.StatusOK {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
	for _, c := range []struct{ path string }{{"/api/groups/" + rid + "/members/kp1"}, {"/api/key-groups/" + lid + "/members/kp1"}, {"/api/groups/" + rid}, {"/api/key-groups/" + lid}} {
		if rec, _ := poolsAPI(t, s, shared, http.MethodDelete, c.path, ""); rec.Code != http.StatusOK {
			t.Errorf("DELETE %s: %d", c.path, rec.Code)
		}
	}
}

// Bundle entries are editable in place (key <-> pool) and assignment conflicts
// still answer 409 until forced.
func TestKeyBundleEntryEditAndAssignConflict(t *testing.T) {
	s, shared := newShellTestServer(t)
	exec := func(q string, args ...interface{}) {
		t.Helper()
		if _, err := s.db.Exec(q, args...); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	exec(`INSERT INTO services (id, name, display_name, upstream_url, host_pattern) VALUES ('svc-b', 'bundle-svc', 'Bundle', 'https://b.example', 'b.example')`)
	exec(`INSERT INTO api_keys (id, service_id, name, key_encrypted) VALUES ('kb1', 'svc-b', 'k1', 'x'), ('kb2', 'svc-b', 'k2', 'x')`)
	exec(`INSERT INTO clients (id, short_id, name, token_hash) VALUES ('cb1', 'cb1', 'client b', 'h')`)
	_, grp := poolsAPI(t, s, shared, http.MethodPost, "/api/groups", `{"name":"gb","service_id":"svc-b"}`)
	gid := grp["id"].(string)
	rec, suite := poolsAPI(t, s, shared, http.MethodPost, "/api/key-suites", `{"name":"b1","description":"d"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create bundle: %d", rec.Code)
	}
	bid := suite["id"].(string)
	rec, ent := poolsAPI(t, s, shared, http.MethodPost, "/api/key-suites/"+bid+"/entries", `{"service_id":"svc-b","api_key_id":"kb1","env_name":"B_KEY"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("add entry: %d %s", rec.Code, rec.Body.String())
	}
	entry, _ := ent["entry"].(map[string]interface{})
	eid, _ := entry["id"].(string)
	if eid == "" {
		eid, _ = ent["id"].(string)
	}
	if eid == "" {
		t.Fatalf("no entry id in %s", rec.Body.String())
	}
	if rec, _ := poolsAPI(t, s, shared, http.MethodPatch, "/api/key-suites/"+bid+"/entries/"+eid, `{"group_id":"`+gid+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("switch entry to pool: %d %s", rec.Code, rec.Body.String())
	}
	if rec, _ := poolsAPI(t, s, shared, http.MethodPatch, "/api/key-suites/"+bid+"/entries/"+eid, `{"api_key_id":"kb2","env_name":"B2"}`); rec.Code != http.StatusOK {
		t.Fatalf("switch entry to key: %d", rec.Code)
	}
	_, got := poolsAPI(t, s, shared, http.MethodGet, "/api/key-suites/"+bid, "")
	es, _ := got["entries"].([]interface{})
	if len(es) != 1 || es[0].(map[string]interface{})["api_key_id"] != "kb2" || es[0].(map[string]interface{})["env_name"] != "B2" {
		t.Fatalf("entry not updated in place: %v", got)
	}
	// A hand-made placeholder for the same service conflicts until forced.
	exec(`INSERT INTO placeholder_keys (id, env_name, placeholder, service_id, api_key_id, client_id) VALUES ('ph-b', 'MANUAL', 'dw-manual-b', 'svc-b', 'kb1', 'cb1')`)
	rec, _ = poolsAPI(t, s, shared, http.MethodPost, "/api/key-suites/"+bid+"/assign", `{"client_id":"cb1"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("assign with conflict: %d %s", rec.Code, rec.Body.String())
	}
	rec, _ = poolsAPI(t, s, shared, http.MethodPost, "/api/key-suites/"+bid+"/assign", `{"client_id":"cb1","force":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("forced assign: %d %s", rec.Code, rec.Body.String())
	}
}

func TestPoolAddMemberRejectsOtherServiceKey(t *testing.T) {
	s, shared := newShellTestServer(t)
	exec := func(q string, a ...interface{}) {
		t.Helper()
		if _, err := s.db.Exec(q, a...); err != nil {
			t.Fatalf("%q: %v", q, err)
		}
	}
	exec(`INSERT INTO services (id, name, display_name, upstream_url, host_pattern) VALUES ('svc-a', 'svc-a', 'A', 'https://a.example', 'a.example')`)
	exec(`INSERT INTO services (id, name, display_name, upstream_url, host_pattern) VALUES ('svc-b', 'svc-b', 'B', 'https://b.example', 'b.example')`)
	exec(`INSERT INTO api_keys (id, service_id, name, key_encrypted) VALUES ('ka', 'svc-a', 'ka', 'x')`)
	exec(`INSERT INTO api_keys (id, service_id, name, key_encrypted) VALUES ('kb', 'svc-b', 'kb', 'x')`)
	exec(`INSERT INTO api_key_groups (id, service_id, name, strategy) VALUES ('g-rot', 'svc-a', 'rot', 'round_robin')`)
	exec(`INSERT INTO key_groups (id, name, description, service_name, rotation_strategy) VALUES ('g-lease', 'lease', '', 'svc-a', 'score')`)
	for _, c := range []struct{ path, body string }{
		{"/api/groups/g-rot/members", `{"api_key_id":"kb","priority":0}`},
		{"/api/key-groups/g-lease/members", `{"api_key_id":"kb","position":0}`},
	} {
		if rec := credAdminRequest(t, s, shared, http.MethodPost, c.path, c.body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s other-service key: %d %s", c.path, rec.Code, rec.Body.String())
		}
	}
	for _, c := range []struct{ path, body string }{
		{"/api/groups/g-rot/members", `{"api_key_id":"ka","priority":0}`},
		{"/api/key-groups/g-lease/members", `{"api_key_id":"ka","position":0}`},
	} {
		if rec := credAdminRequest(t, s, shared, http.MethodPost, c.path, c.body); rec.Code != http.StatusOK {
			t.Errorf("%s same-service key: %d %s", c.path, rec.Code, rec.Body.String())
		}
	}
}
