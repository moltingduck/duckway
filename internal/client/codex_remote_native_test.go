package client

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

// TestCodexRemoteNativeWire is opt-in: DUCKWAY_TEST_CODEX must name an actual
// Codex executable. It uses dummy credentials and loopback destinations only.
// This tests the upstream native wire contract, not provider acceptance.
func TestCodexRemoteNativeWire(t *testing.T) {
	cli := os.Getenv("DUCKWAY_TEST_CODEX")
	if cli == "" {
		t.Skip("set DUCKWAY_TEST_CODEX to an installed Codex executable")
	}
	root := filepath.Join("..", "..", "tmp", "codex-remote-phase1-20261027-native")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	home, err := os.MkdirTemp(root, "home-")
	if err != nil {
		t.Fatal(err)
	}
	home, _ = filepath.Abs(home)
	t.Cleanup(func() { os.RemoveAll(home); os.Remove(root) })
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"dummy-account","chatgpt_plan_type":"plus"},"exp":4102444800}`))
	auth := fmt.Sprintf(`{"auth_mode":"chatgpt","tokens":{"id_token":"e30.%s.dummy","access_token":"dummy-oauth","refresh_token":"dummy-refresh","account_id":"dummy-account"},"last_refresh":"2026-10-27T00:00:00Z"}`, payload)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(auth), 0600); err != nil {
		t.Fatal(err)
	}
	events := make(chan string, 32)
	expire := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	var installation string
	var installMu sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("/wham/remote/control/server/enroll", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		err := json.NewDecoder(r.Body).Decode(&body)
		if err != nil || r.Method != "POST" || r.Header.Get("Authorization") != "Bearer dummy-oauth" {
			events <- "bad enroll"
			http.Error(w, "invalid", 400)
			return
		}
		installMu.Lock()
		installation, _ = body["installation_id"].(string)
		defer installMu.Unlock()
		if installation == "" || r.Header.Get("x-codex-installation-id") != installation || body["app_server_version"] == "" || body["name"] == "" {
			events <- "bad installation"
		}
		events <- "enroll"
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"server_id":"srv_test","environment_id":"env_test","remote_control_token":"dummy-host","expires_at":%q}`, expire)
	})
	mux.HandleFunc("/wham/remote/control/server/pair", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		err := json.NewDecoder(r.Body).Decode(&body)
		if err != nil || r.Method != "POST" || body["manual_code"] != true || r.Header.Get("Authorization") != "Bearer dummy-host" || r.Header.Get("x-codex-installation-id") != "" {
			events <- "bad pair"
		}
		events <- "pair"
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"pairing_code":"fixture-pairing","manual_pairing_code":"ABCD-EFGH","server_id":"srv_test","environment_id":"env_test","expires_at":%q}`, expire)
	})
	mux.Handle("/wham/remote/control/server", websocket.Server{Handshake: func(c *websocket.Config, r *http.Request) error {
		installMu.Lock()
		defer installMu.Unlock()
		if r.Header.Get("Authorization") != "Bearer dummy-host" || r.Header.Get("x-codex-installation-id") != installation || installation == "" || r.Header.Get("x-codex-server-id") != "srv_test" || r.Header.Get("x-codex-protocol-version") != "3" {
			events <- "bad websocket"
			return fmt.Errorf("invalid headers")
		}
		events <- "websocket"
		return nil
	}, Handler: func(ws *websocket.Conn) {
		defer ws.Close()
		if err := ws.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
			events <- "bad deadline"
			return
		}
		if err := websocket.Message.Send(ws, `{"type":"ping","client_id":"fixture-client","stream_id":"fixture-stream"}`); err != nil {
			events <- "bad send"
			return
		}
		for {
			var raw string
			if err := websocket.Message.Receive(ws, &raw); err != nil {
				return
			}
			var msg map[string]any
			if json.Unmarshal([]byte(raw), &msg) == nil && msg["type"] == "pong" {
				events <- "pong"
				return
			}
		}
	}})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	// An explicit rejecting proxy prevents an inherited proxy from sending dummy
	// credentials elsewhere; the only configured API endpoint is loopback.
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		events <- "blocked external request"
		http.Error(w, "denied", http.StatusForbidden)
	}))
	t.Cleanup(deny.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, cli, "-c", "chatgpt_base_url="+fmt.Sprintf("%q", server.URL), "app-server", "--listen", "stdio://")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "CODEX_HOME=" + home, "HTTP_PROXY=" + deny.URL, "HTTPS_PROXY=" + deny.URL, "ALL_PROXY=" + deny.URL, "NO_PROXY=127.0.0.1,localhost", "RUST_LOG=off"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Logf("owned native CLI pid=%d", cmd.Process.Pid)
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	replies := make(chan map[string]any, 32)
	go func() {
		defer close(replies)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 4096), 1024*1024)
		for scanner.Scan() {
			var msg map[string]any
			if json.Unmarshal(scanner.Bytes(), &msg) == nil {
				replies <- msg
			}
		}
	}()
	send := func(s string) {
		t.Helper()
		if _, err := io.WriteString(stdin, s+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	response := func(id float64) map[string]any {
		t.Helper()
		for {
			select {
			case msg, ok := <-replies:
				if !ok {
					t.Fatal("native stdout closed")
				}
				if msg["id"] == id {
					if msg["error"] != nil {
						t.Fatalf("native RPC %v failed", id)
					}
					return msg
				}
			case <-ctx.Done():
				t.Fatal("native RPC timeout")
			}
		}
	}
	send(`{"id":1,"method":"initialize","params":{"clientInfo":{"name":"duckway_fixture","version":"1"},"capabilities":{"experimentalApi":true}}}`)
	response(1)
	send(`{"method":"initialized"}`)
	send(`{"id":2,"method":"remoteControl/enable","params":{"ephemeral":true}}`)
	response(2)
	send(`{"id":3,"method":"remoteControl/pairing/start","params":{"manualCode":true}}`)
	pair := response(3)
	result, _ := pair["result"].(map[string]any)
	for key, want := range map[string]string{"pairingCode": "fixture-pairing", "manualPairingCode": "ABCD-EFGH", "environmentId": "env_test"} {
		if result[key] != want {
			t.Fatalf("native pairing response field %s mismatch", key)
		}
	}
	if result["expiresAt"] == nil {
		t.Fatal("native pairing expiry absent")
	}
	seen := map[string]bool{}
	for len(seen) < 4 {
		select {
		case event := <-events:
			if event == "blocked external request" {
				continue
			}
			if strings.HasPrefix(event, "bad ") {
				t.Fatal(event)
			}
			seen[event] = true
		case <-ctx.Done():
			t.Fatalf("native wire timeout; observed stages %v", seen)
		}
	}
	for _, event := range []string{"enroll", "pair", "websocket", "pong"} {
		if !seen[event] {
			t.Fatalf("missing native stage %s", event)
		}
	}
}
