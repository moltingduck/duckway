package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/server/services"
)

func approvalForTest() services.ApprovalNotification {
	return services.ApprovalNotification{ApprovalID: "a1", ClientName: "c", ServiceName: "s", Method: "GET", Path: "/x"}
}

const (
	ntTgToken   = "123456:AAH-secret-telegram-token"
	ntHookToken = "whsecret-ABCDEF-123456"
	ntSharedSec = "shared-secret-value-xyz"
)

type ntEnv struct {
	t *testing.T
	s *Server
	c *http.Cookie
}

func newNtEnv(t *testing.T) *ntEnv {
	s, shared := newShellTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	return &ntEnv{t: t, s: s, c: shared.AdminAuth.CreateSession("duckway", req)}
}

func (e *ntEnv) do(method, path string, body interface{}) *httptest.ResponseRecorder {
	e.t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(e.c)
	rec := httptest.NewRecorder()
	e.s.ServeHTTP(rec, req)
	return rec
}

func (e *ntEnv) create(typ, name string, cfg map[string]string) string {
	e.t.Helper()
	cj, _ := json.Marshal(cfg)
	rec := e.do("POST", "/api/notifications", map[string]string{"channel_type": typ, "name": name, "config": string(cj)})
	if rec.Code != http.StatusCreated {
		e.t.Fatalf("create %s: %d %s", typ, rec.Code, rec.Body.String())
	}
	var v struct {
		ID string `json:"id"`
	}
	json.Unmarshal(rec.Body.Bytes(), &v)
	return v.ID
}

func (e *ntEnv) storedConfig(id string) string {
	var c string
	if err := e.s.db.QueryRow("SELECT config FROM notification_channels WHERE id = ?", id).Scan(&c); err != nil {
		e.t.Fatal(err)
	}
	return c
}

func seedAllTypes(e *ntEnv) (tg, dc, bot, wh string) {
	tg = e.create("telegram", "tg", map[string]string{"bot_token": ntTgToken, "chat_id": "-100123"})
	dc = e.create("discord", "dc", map[string]string{"webhook_url": "https://discord.com/api/webhooks/1/" + ntHookToken})
	bot = e.create("discord_bot", "bot", map[string]string{"bot_token": "MTIz.bot.token-secret", "channel_id": "555"})
	wh = e.create("webhook", "wh", map[string]string{"url": "https://example.com/hook/" + ntHookToken, "secret": ntSharedSec})
	return
}

func TestNotificationSecretsNeverInGET(t *testing.T) {
	e := newNtEnv(t)
	tg, dc, bot, wh := seedAllTypes(e)
	secrets := []string{ntTgToken, ntHookToken, ntSharedSec, "MTIz.bot.token-secret"}
	check := func(what, body string) {
		for _, sec := range secrets {
			if strings.Contains(body, sec) {
				t.Errorf("%s leaks secret %q: %s", what, sec, body)
			}
		}
	}
	rec := e.do("GET", "/api/notifications", nil)
	check("list", rec.Body.String())
	if !strings.Contains(rec.Body.String(), "********") {
		t.Error("list should carry the mask sentinel")
	}
	for _, id := range []string{tg, dc, bot, wh} {
		check("detail "+id, e.do("GET", "/api/notifications/"+id, nil).Body.String())
	}
	// non-secret fields stay visible so the form can show them
	if !strings.Contains(e.do("GET", "/api/notifications/"+tg, nil).Body.String(), "-100123") {
		t.Error("chat_id should be returned")
	}
	// the create response and the HTML page are masked too
	cj, _ := json.Marshal(map[string]string{"bot_token": "tok-create-secret-1", "chat_id": "1"})
	c := e.do("POST", "/api/notifications", map[string]string{"channel_type": "telegram", "name": "n", "config": string(cj)})
	check("create", c.Body.String())
	if strings.Contains(c.Body.String(), "tok-create-secret-1") {
		t.Error("create response leaks token")
	}
	page := e.do("GET", "/admin/notifications", nil)
	check("page", page.Body.String())
}

func TestNotificationMaskedPutKeepsAndNewValueReplaces(t *testing.T) {
	e := newNtEnv(t)
	_, _, _, wh := seedAllTypes(e)
	// masked sentinel keeps both secrets; non-secret change applies
	rec := e.do("PUT", "/api/notifications/"+wh, map[string]string{"name": "renamed", "config": `{"url":"********","secret":"********"}`})
	if rec.Code != 200 {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}
	var cfg map[string]string
	json.Unmarshal([]byte(e.storedConfig(wh)), &cfg)
	if cfg["url"] != "https://example.com/hook/"+ntHookToken || cfg["secret"] != ntSharedSec {
		t.Fatalf("masked PUT lost the stored secrets: %v", cfg)
	}
	// a new value replaces only that secret
	rec = e.do("PUT", "/api/notifications/"+wh, map[string]string{"name": "renamed", "config": `{"url":"********","secret":"brand-new-secret"}`})
	if rec.Code != 200 {
		t.Fatalf("put2: %d %s", rec.Code, rec.Body.String())
	}
	json.Unmarshal([]byte(e.storedConfig(wh)), &cfg)
	if cfg["secret"] != "brand-new-secret" || cfg["url"] != "https://example.com/hook/"+ntHookToken {
		t.Fatalf("replace failed: %v", cfg)
	}
	// optional secret cleared by empty string
	e.do("PUT", "/api/notifications/"+wh, map[string]string{"name": "renamed", "config": `{"url":"********","secret":""}`})
	json.Unmarshal([]byte(e.storedConfig(wh)), &cfg)
	if cfg["secret"] != "" {
		t.Fatalf("secret not cleared: %v", cfg)
	}
	// the sentinel is never stored literally
	if strings.Contains(e.storedConfig(wh), "********") {
		t.Fatal("mask stored literally")
	}
}

func TestNotificationValidationAndMaskOnCreate(t *testing.T) {
	e := newNtEnv(t)
	for name, tc := range map[string]struct{ typ, cfg string }{
		"missing chat":   {"telegram", `{"bot_token":"abc"}`},
		"bad url":        {"webhook", `{"url":"ftp://x"}`},
		"mask on create": {"telegram", `{"bot_token":"********","chat_id":"1"}`},
		"not json":       {"webhook", `nope`},
		"discord no url": {"discord", `{}`},
	} {
		rec := e.do("POST", "/api/notifications", map[string]string{"channel_type": tc.typ, "name": "x", "config": tc.cfg})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: want 400 got %d %s", name, rec.Code, rec.Body.String())
		}
	}
	_, _, _, wh := seedAllTypes(e)
	rec := e.do("PUT", "/api/notifications/"+wh, map[string]string{"name": "x", "config": `{"url":"not a url","secret":"********"}`})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad url on PUT: %d", rec.Code)
	}
	if e.do("PUT", "/api/notifications/missing", map[string]string{"name": "x", "config": "{}"}).Code != http.StatusNotFound {
		t.Error("unknown id should 404")
	}
}

func TestNotificationEnableSwitchAndDispatcher(t *testing.T) {
	var onHits, offHits int32
	on := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&onHits, 1) }))
	defer on.Close()
	off := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&offHits, 1) }))
	defer off.Close()
	e := newNtEnv(t)
	idOn := e.create("webhook", "on", map[string]string{"url": on.URL})
	idOff := e.create("webhook", "off", map[string]string{"url": off.URL})
	if rec := e.do("POST", "/api/notifications/"+idOff+"/active", map[string]bool{"active": false}); rec.Code != 200 {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body.String())
	}
	var list []struct {
		ID       string `json:"id"`
		IsActive bool   `json:"is_active"`
	}
	json.Unmarshal(e.do("GET", "/api/notifications", nil).Body.Bytes(), &list)
	for _, c := range list {
		if c.ID == idOff && c.IsActive {
			t.Fatal("channel still active after disable")
		}
		if c.ID == idOn && !c.IsActive {
			t.Fatal("other channel was disabled")
		}
	}
	if e.do("POST", "/api/notifications/"+idOff+"/active", map[string]string{}).Code != http.StatusBadRequest {
		t.Error("missing active should 400")
	}
	e.s.notifier.NotifyApprovalNeeded(approvalForTest())
	deadline := time.Now().Add(3 * time.Second)
	for atomic.LoadInt32(&onHits) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if atomic.LoadInt32(&onHits) != 1 {
		t.Errorf("enabled channel hits = %d, want 1", onHits)
	}
	if atomic.LoadInt32(&offHits) != 0 {
		t.Errorf("disabled channel received %d notifications", offHits)
	}
	// re-enable: it is sent to again
	e.do("POST", "/api/notifications/"+idOff+"/active", map[string]bool{"active": true})
	e.s.notifier.NotifyApprovalNeeded(approvalForTest())
	deadline = time.Now().Add(3 * time.Second)
	for atomic.LoadInt32(&offHits) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if atomic.LoadInt32(&offHits) != 1 {
		t.Errorf("re-enabled channel hits = %d, want 1", offHits)
	}
}

func TestNotificationTestMessageScrubsSecrets(t *testing.T) {
	e := newNtEnv(t)
	// connection refused: the transport error echoes the request URL
	id := e.create("webhook", "dead", map[string]string{"url": "http://127.0.0.1:1/hooks/" + ntHookToken})
	rec := e.do("POST", "/api/notifications/"+id+"/test", nil)
	if strings.Contains(rec.Body.String(), ntHookToken) {
		t.Fatalf("test result leaks the token: %s", rec.Body.String())
	}
}

func TestNotificationHostileNameIsStoredVerbatimAndEscapedInAPIAndPage(t *testing.T) {
	e := newNtEnv(t)
	name := `<img src=x onerror=alert(1)>"'`
	e.create("webhook", name, map[string]string{"url": "https://example.com/h"})
	rec := e.do("GET", "/api/notifications", nil)
	var list []struct {
		Name string `json:"name"`
	}
	json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) != 1 || list[0].Name != name {
		t.Fatalf("name not round-tripped: %v", list)
	}
	if strings.Contains(e.do("GET", "/admin/notifications", nil).Body.String(), "<img src=x") {
		t.Fatal("hostile name reached the page unescaped")
	}
}

func TestNotificationsMigrationHasEnabledFlagDefaultOn(t *testing.T) {
	e := newNtEnv(t)
	rows, err := e.s.db.Query("PRAGMA table_info(notification_channels)")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt *string
		rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk)
		if name == "is_active" {
			found = true
			if notnull != 1 || dflt == nil || *dflt != "1" {
				t.Errorf("is_active must be NOT NULL DEFAULT 1, got notnull=%d default=%v", notnull, dflt)
			}
		}
	}
	if !found {
		t.Fatal("notification_channels.is_active missing: no migration needed only if it exists")
	}
}
