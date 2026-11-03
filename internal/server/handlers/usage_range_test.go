package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
)

func TestParseUsageRangeValidation(t *testing.T) {
	now := time.Date(2026, 11, 3, 15, 30, 0, 0, time.UTC)
	cases := []struct {
		name, query, field string
	}{
		{"ok single day", "from=2026-11-03&to=2026-11-03", ""},
		{"ok 90 days", "from=2026-08-06&to=2026-11-03", ""},
		{"none", "days=7", ""},
		{"bad from format", "from=11/01/2026&to=2026-11-03", "from"},
		{"bad to format", "from=2026-11-01&to=nov3", "to"},
		{"non-padded", "from=2026-1-1&to=2026-11-03", "from"},
		{"impossible date", "from=2026-02-30&to=2026-11-03", "from"},
		{"from after to", "from=2026-11-03&to=2026-11-01", "from"},
		{"future to", "from=2026-11-01&to=2026-11-04", "to"},
		{"over 90 days", "from=2026-08-05&to=2026-11-03", "to"},
		{"missing to", "from=2026-11-01", "to"},
		{"missing from", "to=2026-11-01", "from"},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodGet, "/api/usage?"+tc.query, nil)
		rng, err := parseUsageRange(r, now)
		if tc.field == "" {
			if err != nil {
				t.Errorf("%s: unexpected error %v", tc.name, err)
			}
			continue
		}
		if err == nil || err.Field != tc.field || rng != nil {
			t.Errorf("%s: got rng=%v err=%v, want field %q", tc.name, rng, err, tc.field)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/x?from=2026-11-01&to=2026-11-03&days=30&hours=24", nil)
	rng, err := parseUsageRange(r, now)
	if err != nil || rng.Days != 3 || rng.Window.Since != "2026-11-01 00:00:00" || rng.Window.Until != "2026-11-04 00:00:00" {
		t.Fatalf("custom wins over days/hours: %+v %v", rng, err)
	}
}

func TestUsageEndpointsRejectInvalidRange(t *testing.T) {
	d := newUsageTestDeps(t)
	calls := map[string]func(http.ResponseWriter, *http.Request){
		"/api/usage":                     d.h.List,
		"/api/usage/clients":             d.h.Clients,
		"/api/usage/sessions":            d.h.Sessions,
		"/api/usage/detail":              d.h.Detail,
		"/api/usage/conversations":       d.h.Conversations,
		"/api/usage/keys/k/detail":       d.h.KeyDetail,
		"/api/usage/key-groups/g/detail": d.h.KeyGroupDetail,
	}
	for path, call := range calls {
		for _, q := range []string{"from=bad&to=2025-03-12", "from=2025-03-12&to=2025-03-10", "from=2025-03-10&to=2999-01-01", "from=2025-01-01&to=2025-06-01"} {
			req := httptest.NewRequest(http.MethodGet, path+"?key_id=k&"+q, nil)
			req.SetPathValue("id", "k")
			rec := httptest.NewRecorder()
			call(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s?%s = %d, want 400", path, q, rec.Code)
				continue
			}
			var body map[string]string
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if !strings.HasPrefix(body["error"], "invalid from") && !strings.HasPrefix(body["error"], "invalid to") {
				t.Errorf("%s error should name field: %q", path, body["error"])
			}
		}
	}
}

func TestUsageEndpointsFilterCustomRange(t *testing.T) {
	d := newUsageTestDeps(t)
	_ = d.svcQ.Create(&models.Service{ID: "svc-rng", Name: "rng", UpstreamURL: "https://example.test", HostPattern: "example.test", IsActive: true})
	_ = d.clientQ.Create(&models.Client{ID: "c-rng", ShortID: "rng", Name: "rngagent", TokenHash: "h"})
	_ = d.apiKeyQ.Create(&models.APIKey{ID: "k-rng", ServiceID: "svc-rng", Name: "rngkey", KeyEncrypted: "x"})
	group, err := queries.CreateKeyGroup(d.db, "rng group", "", "rng", "score")
	if err != nil {
		t.Fatal(err)
	}
	if err := queries.AddKeyToGroup(d.db, group.ID, "k-rng", 0); err != nil {
		t.Fatal(err)
	}
	// Range under test: 2025-03-10 .. 2025-03-12 inclusive.
	stamps := []struct {
		at     string
		tokens int64
	}{
		{"2025-03-09 23:59:59", 1000},  // before
		{"2025-03-10 00:00:00", 1},     // lower boundary (in)
		{"2025-03-11 12:00:00", 10},    // in
		{"2025-03-12 23:59:59", 100},   // upper boundary (in)
		{"2025-03-13 00:00:00", 10000}, // after (exclusive)
	}
	for _, s := range stamps {
		at, _ := time.Parse("2006-01-02 15:04:05", s.at)
		if err := d.convQ.InsertAt(&queries.ConversationUsageRecord{ClientID: "c-rng", APIKeyID: "k-rng", KeyGroupID: group.ID, Provider: "test", Model: "m", ConversationID: "s", InputTokens: s.tokens}, at); err != nil {
			t.Fatal(err)
		}
		if _, err := d.db.Exec(`INSERT INTO request_log (client_id, service_name, method, path, status_code, created_at) VALUES ('c-rng','rng','GET','/',200,?)`, s.at); err != nil {
			t.Fatal(err)
		}
	}
	const rq = "from=2025-03-10&to=2025-03-12"

	get := func(path string, call func(http.ResponseWriter, *http.Request), out interface{}) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.SetPathValue("id", map[bool]string{true: group.ID, false: "k-rng"}[strings.Contains(path, "key-groups")])
		rec := httptest.NewRecorder()
		call(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", path, rec.Code, rec.Body.String())
		}
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatal(err)
		}
	}

	var keys []usageRow
	get("/api/usage?"+rq, d.h.List, &keys)
	if len(keys) != 1 || keys[0].TokensInput != 111 || keys[0].CapturedRequests != 3 {
		t.Errorf("List: %+v", keys)
	}
	var clients []clientUsageView
	get("/api/usage/clients?"+rq, d.h.Clients, &clients)
	if len(clients) != 1 || clients[0].TokensInput != 111 || clients[0].Requests != 3 {
		t.Errorf("Clients: %+v", clients)
	}
	var sess []queries.SessionUsageRow
	get("/api/usage/sessions?"+rq, d.h.Sessions, &sess)
	if len(sess) != 1 || sess[0].Requests != 3 {
		t.Errorf("Sessions: %+v", sess)
	}
	var convs []queries.ConversationUsageRow
	get("/api/usage/conversations?key_id=k-rng&"+rq, d.h.Conversations, &convs)
	if len(convs) != 1 || convs[0].InputTokens != 111 || convs[0].Requests != 3 {
		t.Errorf("Conversations: %+v", convs)
	}
	for _, p := range []struct {
		path string
		call func(http.ResponseWriter, *http.Request)
	}{{"/api/usage/keys/k-rng/detail?" + rq, d.h.KeyDetail}, {"/api/usage/key-groups/x/detail?" + rq, d.h.KeyGroupDetail}} {
		var rows []queries.MeteredUsageDetailRow
		if strings.Contains(p.path, "key-groups") {
			p.path = strings.Replace(p.path, "/x/", "/"+group.ID+"/", 1)
		}
		get(p.path, p.call, &rows)
		var sum int64
		days := map[string]bool{}
		for _, r := range rows {
			sum += r.InputTokens
			days[r.Day] = true
		}
		if sum != 111 || len(days) != 3 || days["2025-03-09"] || days["2025-03-13"] {
			t.Errorf("%s rows=%+v", p.path, rows)
		}
	}
	for _, q := range []string{"key_id=k-rng", "key_group_id=" + group.ID} {
		var det meteredUsageDetailResponse
		get("/api/usage/detail?"+q+"&"+rq, d.h.Detail, &det)
		if det.Summary.Requests != 3 || det.Summary.InputTokens != 111 || det.WindowDays != 3 || det.From != "2025-03-10" || det.To != "2025-03-12" || len(det.Daily) != 3 {
			t.Errorf("Detail %s: %+v", q, det)
		}
	}
	// Single-day boundary range picks only that day's rows.
	var one []clientUsageView
	get("/api/usage/clients?from=2025-03-12&to=2025-03-12", d.h.Clients, &one)
	if len(one) != 1 || one[0].TokensInput != 100 {
		t.Errorf("single day: %+v", one)
	}
	// from/to wins over days/hours.
	var win []clientUsageView
	get("/api/usage/clients?days=1&"+rq, d.h.Clients, &win)
	if len(win) != 1 || win[0].TokensInput != 111 {
		t.Errorf("from/to should win over days: %+v", win)
	}
}

func TestUsageLegacyDaysHoursUnchanged(t *testing.T) {
	d := newUsageTestDeps(t)
	_ = d.svcQ.Create(&models.Service{ID: "svc-leg", Name: "leg", UpstreamURL: "https://example.test", HostPattern: "example.test", IsActive: true})
	_ = d.apiKeyQ.Create(&models.APIKey{ID: "k-leg", ServiceID: "svc-leg", Name: "legkey", KeyEncrypted: "x"})
	now := time.Now().UTC()
	for _, h := range []int{1, 30, 24 * 10} {
		if err := d.convQ.InsertAt(&queries.ConversationUsageRecord{APIKeyID: "k-leg", Provider: "test", Model: "m", InputTokens: int64(h)}, now.Add(-time.Duration(h)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	check := func(path string, want int64) {
		t.Helper()
		rec := httptest.NewRecorder()
		d.h.Clients(rec, httptest.NewRequest(http.MethodGet, path, nil))
		var c []clientUsageView
		_ = json.Unmarshal(rec.Body.Bytes(), &c)
		var got int64
		for _, x := range c {
			got += x.TokensInput
		}
		if got != want {
			t.Errorf("%s = %d, want %d", path, got, want)
		}
	}
	check("/api/usage/clients?days=1", 1)
	check("/api/usage/clients?days=3", 31)
	check("/api/usage/clients?days=30", 271)
	rec := httptest.NewRecorder()
	d.h.List(rec, httptest.NewRequest(http.MethodGet, "/api/usage?hours=24", nil))
	var keys []usageRow
	_ = json.Unmarshal(rec.Body.Bytes(), &keys)
	if len(keys) != 1 || keys[0].TokensInput != 1 {
		t.Errorf("hours=24: %+v", keys)
	}
}
