package handlers

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	"github.com/hackerduck/duckway/internal/server/middleware"
	"github.com/hackerduck/duckway/internal/server/services"
)

func TestCodexRemoteCredentialIsolation(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	access := testCodexJWT(`{"exp":1893456000,"https://api.openai.com/auth":{"chatgpt_account_id":"account-one"}}`)
	encrypted, _ := crypto.Encrypt(access)
	refresh, _ := crypto.Encrypt("dummy-refresh")
	svc := queries.NewServiceQueries(db)
	openai, err := svc.GetByName("openai")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"one", "two"} {
		if _, err = db.Exec(`INSERT INTO clients(id,name,token_hash) VALUES (?,?,?)`, id, id, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.Exec(`INSERT INTO api_keys(id,service_id,name,key_encrypted,refresh_token,expires_at,token_endpoint,subscription_info) VALUES ('key',?,'remote',?,?,1893456000,'https://auth.openai.com/oauth/token','{"credential_kind":"codex_oauth","auth_mode":"chatgpt","source":"codex"}')`, openai.ID, encrypted, refresh); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"one", "two"} {
		if _, err = db.Exec(`INSERT INTO placeholder_keys(id,env_name,placeholder,service_id,api_key_id,client_id,requires_approval) VALUES (?,'OPENAI_API_KEY',?,?,'key',?,0)`, id, "sk-proj-dw_"+id, openai.ID, id); err != nil {
			t.Fatal(err)
		}
	}
	keys := queries.NewAPIKeyQueries(db)
	ph := queries.NewPlaceholderQueries(db)
	approvals := queries.NewApprovalQueries(db)
	h := NewProxyHandler(svc, keys, services.NewKeyResolver(crypto, keys, ph, queries.NewGroupQueries(db), approvals), nil, approvals, nil, nil).WithCrypto(crypto).WithCodexRemote(queries.NewCodexRemoteQueries(db))
	calls := 0
	upstreamStatus := 200
	customHeader := ""
	var relayPeer net.Conn
	relayReady := make(chan struct{}, 1)
	h.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.Host != "chatgpt.com" {
			t.Fatalf("unexpected destination %s", req.URL)
		}
		auth := req.Header.Get("Authorization")
		if strings.HasSuffix(req.URL.Path, "/enroll") || strings.HasSuffix(req.URL.Path, "/refresh") {
			if auth != "Bearer "+access || req.Header.Get("ChatGPT-Account-Id") != "account-one" {
				t.Fatal("OAuth/account mapping failed")
			}
		} else if auth != "Bearer provider-host-token" || req.Header.Get("ChatGPT-Account-Id") != "" {
			t.Fatal("pair used wrong credential")
		}
		if req.Header.Get("Cookie") != "" || req.Header.Get("X-Duckway-Test") != "" {
			t.Fatal("forwarded gateway credentials")
		}
		if req.URL.Path == codexRemoteBase {
			upstream, peer := net.Pipe()
			relayPeer = peer
			t.Cleanup(func() { upstream.Close(); peer.Close() })
			relayReady <- struct{}{}
			return &http.Response{StatusCode: 101, Header: http.Header{"Connection": {"Upgrade"}, "Upgrade": {"websocket"}, "Sec-Websocket-Accept": {"fixture"}}, Body: upstream, Request: req}, nil
		}
		body := `{"claimed":true}`
		if strings.HasSuffix(req.URL.Path, "/enroll") || strings.HasSuffix(req.URL.Path, "/refresh") {
			body = `{"server_id":"srv_test","environment_id":"env_test","remote_control_token":"provider-host-token","expires_at":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`
		}
		return &http.Response{StatusCode: upstreamStatus, Header: http.Header{"Retry-After": {"120"}, "Set-Cookie": {"secret=never"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}
	request := func(client, token, path, body, installation string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/proxy/openai-chatgpt"+codexRemoteBase+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		if installation != "" {
			req.Header.Set("X-Codex-Installation-Id", installation)
		}
		req.Header.Set("Cookie", "gateway-cookie")
		req.Header.Set("X-Duckway-Test", "gateway-secret")
		req.Header.Set("X-Custom-Auth", customHeader)
		req = req.WithContext(context.WithValue(req.Context(), middleware.ClientKey, &models.Client{ID: client, Name: client}))
		rec := httptest.NewRecorder()
		h.Handle(rec, req)
		return rec
	}
	enroll := request("one", "sk-proj-dw_one", "/enroll", `{"installation_id":"install-one","name":"test","os":"linux","arch":"aarch64","app_server_version":"0.164.0"}`, "install-one")
	if enroll.Code != 200 {
		t.Fatalf("enroll %d %s", enroll.Code, enroll.Body.String())
	}
	var doc map[string]string
	if err = json.Unmarshal(enroll.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	capability := doc["remote_control_token"]
	monitorQueries := queries.NewCodexRemoteQueries(db)
	monitorBinding, monitorErr := monitorQueries.Capability(services.HashToken(capability), "one", time.Now().Unix())
	if monitorErr != nil {
		t.Fatal(monitorErr)
	}
	if _, monitorErr = monitorQueries.RelayAuthorization(context.Background(), monitorBinding, time.Now().Unix()); monitorErr != nil {
		t.Fatalf("valid relay snapshot: %v", monitorErr)
	}
	if !strings.HasPrefix(capability, codexRemoteCapabilityPrefix) || strings.Contains(enroll.Body.String(), "provider-host-token") || doc["environment_id"] != "env_test" {
		t.Fatal("host credential leaked or response damaged")
	}
	var storedHash, storedToken string
	if err = db.QueryRow(`SELECT capability_hash,token_encrypted FROM codex_remote_enrollments`).Scan(&storedHash, &storedToken); err != nil {
		t.Fatal(err)
	}
	if storedHash == capability || storedToken == "provider-host-token" {
		t.Fatal("credentials stored in plaintext")
	}
	// The native pair requests deliberately have no installation header.
	for _, path := range []string{"/pair", "/pair/status"} {
		body := `{"manual_code":true}`
		if path == "/pair/status" {
			body = `{"pairing_code":"pairing"}`
		}
		r := request("one", capability, path, body, "")
		if r.Code != 200 {
			t.Fatalf("native pair %s: %d %s", path, r.Code, r.Body.String())
		}
	}
	for _, tc := range []struct{ client, token, path, body, installation string }{
		{"two", capability, "/pair", `{}`, ""},
		{"one", "provider-host-token", "/pair", `{}`, ""},
		{"one", "sk-proj-dw_one", "/pair", `{}`, ""},
		{"one", "sk-proj-dw_one", "/refresh", `{"server_id":"srv_test","installation_id":"other"}`, "other"},
		{"one", "sk-proj-dw_one", "/enroll", `{"installation_id":"other"}`, "install-one"},
		{"one", capability, "/unknown", `{}`, ""},
	} {
		before := calls
		r := request(tc.client, tc.token, tc.path, tc.body, tc.installation)
		if r.Code < 400 || calls != before {
			t.Fatalf("invalid request forwarded: %+v status%d", tc, r.Code)
		}
	}

	// Capability binding wins over an alternate same-key custom-header placeholder.
	openai.AuthHeader = "X-Custom-Auth"
	if err = svc.Update(openai); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO placeholder_keys(id,env_name,placeholder,service_id,api_key_id,client_id,requires_approval) VALUES ('alternate','ALTERNATE_KEY','sk-proj-dw_alternate',?,'key','one',0)`, openai.ID); err != nil {
		t.Fatal(err)
	}
	customHeader = "sk-proj-dw_alternate"
	if r := request("one", capability, "/pair", `{"manual_code":true}`, ""); r.Code != 200 {
		t.Fatalf("capability binding changed by custom header: %d", r.Code)
	}
	customHeader = ""
	openai.AuthHeader = "Authorization"
	if err = svc.Update(openai); err != nil {
		t.Fatal(err)
	}
	for _, status := range []int{429, 503} {
		upstreamStatus = status
		r := request("one", "sk-proj-dw_one", "/refresh", `{"server_id":"srv_test","installation_id":"install-one"}`, "install-one")
		if r.Code != status || r.Header().Get("Retry-After") != "120" || r.Header().Get("Set-Cookie") != "" {
			t.Fatalf("throttle headers: %d %v", r.Code, r.Header())
		}
	}
	upstreamStatus = 200
	// Exercise this route's real HTTP upgrade and both stream directions.
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(context.WithValue(r.Context(), middleware.ClientKey, &models.Client{ID: "one"}))
		h.Handle(w, r)
	}))
	t.Cleanup(gateway.Close)
	conn, dialErr := net.Dial("tcp", strings.TrimPrefix(gateway.URL, "http://"))
	if dialErr != nil {
		t.Fatal(dialErr)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "GET /proxy/openai-chatgpt%s HTTP/1.1\r\nHost: duckway\r\nAuthorization: Bearer %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGVzdC1rZXktMTIzNDU2Nw==\r\nX-Codex-Installation-Id: install-one\r\nX-Codex-Server-Id: srv_test\r\n\r\n", codexRemoteBase, capability)
	response, readErr := http.ReadResponse(bufio.NewReader(conn), nil)
	if readErr != nil || response.StatusCode != 101 {
		t.Fatalf("upgrade: %v %v", response, readErr)
	}
	<-relayReady
	frame := []byte{0x81, 0x82, 1, 2, 3, 4, 'h' ^ 1, 'i' ^ 2}
	sent := make(chan error, 1)
	go func() { _, e := conn.Write(frame); sent <- e }()
	got := make([]byte, len(frame))
	if _, err = io.ReadFull(relayPeer, got); err != nil || string(got) != string(frame) {
		t.Fatalf("client frame: %x %v", got, err)
	}
	if err = <-sent; err != nil {
		t.Fatal(err)
	}
	go func() { _, e := relayPeer.Write([]byte{0x81, 2, 'o', 'k'}); sent <- e }()
	got = make([]byte, 4)
	if _, err = io.ReadFull(conn, got); err != nil || string(got) != string([]byte{0x81, 2, 'o', 'k'}) {
		t.Fatalf("upstream frame: %x %v", got, err)
	}
	if err = <-sent; err != nil {
		t.Fatal(err)
	}
	rotated := request("one", "sk-proj-dw_one", "/refresh", `{"server_id":"srv_test","installation_id":"install-one"}`, "install-one")
	if rotated.Code != 200 {
		t.Fatalf("refresh %d %s", rotated.Code, rotated.Body.String())
	}
	var closed [1]byte
	if _, closeErr := conn.Read(closed[:]); closeErr != io.EOF {
		t.Fatalf("rotation did not terminate relay: %v", closeErr)
	}
	if r := request("one", capability, "/pair", `{}`, ""); r.Code != 403 {
		t.Fatal("old capability survived rotation")
	}
	json.Unmarshal(rotated.Body.Bytes(), &doc)
	capability = doc["remote_control_token"]
	if _, err = db.Exec(`UPDATE placeholder_keys SET is_active=0 WHERE id='one'`); err != nil {
		t.Fatal(err)
	}
	before := calls
	if r := request("one", capability, "/pair", `{}`, ""); r.Code != 403 || calls != before {
		t.Fatal("revoked assignment remained usable")
	}
}

func TestCodexRemoteRelayWatcher(t *testing.T) {
	for _, mode := range []string{"revoked", "expired", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			local, peer := net.Pipe()
			t.Cleanup(func() { local.Close(); peer.Close() })
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			checked := make(chan struct{}, 1)
			expiry := time.Now().Add(time.Hour)
			if mode == "expired" {
				expiry = time.Now().Add(-time.Second)
			}
			stop := watchCodexRemoteRelay(ctx, local, expiry, time.Millisecond, func(context.Context) bool {
				select {
				case checked <- struct{}{}:
				default:
				}
				return mode != "revoked"
			})
			t.Cleanup(stop)
			if mode == "cancelled" {
				cancel()
			}
			peer.SetReadDeadline(time.Now().Add(time.Second))
			var b [1]byte
			if _, err := peer.Read(b[:]); err != io.EOF {
				t.Fatalf("relay did not close: %v", err)
			}
			if mode == "revoked" {
				select {
				case <-checked:
				default:
					t.Fatal("assignment not checked")
				}
			}
		})
	}
}

func TestCodexRemoteHostRotationLock(t *testing.T) {
	q := queries.NewCodexRemoteQueries(nil)
	unlock := q.LockHost("client", "installation")
	entered := make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(entered)
		release := q.LockHost("client", "\u00a0installation\u00a0")
		close(done)
		release()
	}()
	<-entered
	select {
	case <-done:
		t.Fatal("same-host rotation entered before first save completed")
	default:
	}
	unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("host rotation lock not released")
	}
}

// A held sole database connection must not prevent expiry/cancellation from
// closing the relay, and stop must join the context-cancelled checker.
func TestCodexRemoteRelayWatcherDatabaseContention(t *testing.T) {
	for _, mode := range []string{"expiry", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			db, err := database.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Close() })
			db.SetMaxOpenConns(1)
			held, err := db.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { held.Close() })
			local, peer := net.Pipe()
			t.Cleanup(func() { local.Close(); peer.Close() })
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			started, finished := make(chan struct{}), make(chan struct{})
			expiry := time.Now().Add(time.Hour)
			if mode == "expiry" {
				expiry = time.Now().Add(15 * time.Millisecond)
			}
			q := queries.NewCodexRemoteQueries(db)
			stop := watchCodexRemoteRelay(ctx, local, expiry, 10*time.Millisecond, func(checkCtx context.Context) bool {
				close(started)
				defer close(finished)
				_, err := q.RelayAuthorization(checkCtx, &queries.CodexRemoteEnrollment{}, time.Now().Unix())
				return err == nil
			})
			t.Cleanup(stop)
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("checker not started")
			}
			if mode == "cancel" {
				cancel()
			}
			peer.SetReadDeadline(time.Now().Add(time.Second))
			var b [1]byte
			if _, err := peer.Read(b[:]); err != io.EOF {
				t.Fatalf("relay remained open: %v", err)
			}
			stopped := make(chan struct{})
			go func() { stop(); close(stopped) }()
			select {
			case <-stopped:
			case <-time.After(time.Second):
				t.Fatal("stop blocked on database")
			}
			select {
			case <-finished:
			default:
				t.Fatal("checker orphaned")
			}
		})
	}
}
