//go:build linux

package client

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"golang.org/x/net/websocket"
	"io"
	"math/big"
	"net"
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
)

// Opt-in actual upstream binary; all credentials and endpoints are fixture-only.
func TestCodexDaemonNativeProxy(t *testing.T) {
	cli := os.Getenv("DUCKWAY_TEST_CODEX")
	if cli == "" {
		t.Skip("set DUCKWAY_TEST_CODEX to actual Codex CLI")
	}
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	config := filepath.Join(dir, "duckway")
	for _, path := range []string{home, config} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
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
		if err := ws.SetDeadline(time.Now().Add(45 * time.Second)); err != nil {
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

	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Duckway fixture CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{"chatgpt.com"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, caTemplate, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(mux)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}}}
	server.StartTLS()
	t.Cleanup(server.Close)

	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0600); err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	endpoint := "chatgpt.com:" + port
	proxyEvents := make(chan string, 32)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" || r.Host != endpoint {
			http.Error(w, "fixture destination only", http.StatusForbidden)
			return
		}
		upstream, err := net.DialTimeout("tcp", server.Listener.Addr().String(), time.Second)
		if err != nil {
			http.Error(w, "unavailable", http.StatusBadGateway)
			return
		}
		downstream, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			upstream.Close()
			return
		}
		proxyEvents <- r.Host
		_, _ = io.WriteString(downstream, "HTTP/1.1 200 Connection Established\r\n\r\n")
		go func() { defer upstream.Close(); defer downstream.Close(); _, _ = io.Copy(upstream, downstream) }()
		_, _ = io.Copy(downstream, upstream)
	}))
	t.Cleanup(proxy.Close)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(fmt.Sprintf("chatgpt_base_url = %q\n", "https://"+endpoint)), 0600); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "CODEX_HOME=" + home, "RUST_LOG=off", "HTTP_PROXY=" + proxy.URL, "HTTPS_PROXY=" + proxy.URL, "ALL_PROXY=" + proxy.URL, "NO_PROXY=localhost,127.0.0.1", "CODEX_CA_CERTIFICATE=" + ca, "SSL_CERT_FILE=" + ca}

	var physicalSocket string
	// Native stop only touches this test's private home; registered before startup.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, cli, "app-server", "daemon", "stop")
		cmd.Env = env
		if err := cmd.Run(); err != nil {
			t.Errorf("cleanup native stop failed: %v", err)
			return
		}
		for _, name := range []string{"daemon.pid", "app-server.pid", "daemon-updater.pid", "app-server-updater.pid"} {
			data, _ := os.ReadFile(filepath.Join(home, "app-server-daemon", name))
			var record struct {
				PID int `json:"pid"`
			}
			if json.Unmarshal(data, &record) == nil && record.PID > 0 {
				actual, alive, err := codexDaemonProcessEnvironment(record.PID)
				if err != nil {
					t.Errorf("cleanup process %d unverifiable", record.PID)
					return
				}
				if alive {
					if !strings.Contains(name, "updater") || daemonEnvMap(actual)["CODEX_HOME"] != home {
						t.Errorf("cleanup process %d ownership unverified", record.PID)
						return
					}
					// Native stop deliberately leaves its updater running. This
					// fixture owns this private home and explicitly reaps it.
					if err := syscall.Kill(record.PID, syscall.SIGTERM); err != nil {
						t.Error(err)
						return
					}
					deadline := time.Now().Add(5 * time.Second)
					for alive && time.Now().Before(deadline) {
						time.Sleep(25 * time.Millisecond)
						_, alive, err = codexDaemonProcessEnvironment(record.PID)
						if err != nil {
							t.Error(err)
							return
						}
					}
					if alive {
						t.Errorf("cleanup updater %d remains live", record.PID)
						return
					}
				}
			}
		}
		if physicalSocket != "" {
			_ = os.Remove(physicalSocket)
			_ = os.Remove(physicalSocket + ".lock")
		}
	})
	p, prepared, err := PrepareCodexDaemon([]string{cli}, env, config)
	if err != nil {
		t.Fatal(err)
	}
	physicalSocket, _ = filepath.EvalSymlinks(filepath.Join(home, "app-server-control", "app-server-control.sock"))
	t.Logf("owned home %s; physical socket %s", home, physicalSocket)
	if p == nil {
		t.Fatal("daemon was not prepared")
	}
	q, _, err := PrepareCodexDaemon([]string{cli}, env, config)
	if err != nil || q == nil {
		t.Fatalf("reconnect: %v", err)
	}
	if _, err := p.verify(true); err != nil {
		t.Fatal(err)
	}

	before, err := os.ReadFile(filepath.Join(home, "app-server-daemon", "daemon.pid"))
	if err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		argv := append([]string{cli}, args...)
		prep, childEnv, err := PrepareCodexDaemon(argv, env, config)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, cli, args...)
		cmd.Env = childEnv
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("native %v: %v %s", args, err, output)
		}
		if err := prep.VerifyAfter(); err != nil {
			t.Fatal(err)
		}
	}
	foreignConfig := filepath.Join(dir, "other-launcher")
	if err := os.Mkdir(foreignConfig, 0700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{cli, "remote-control", "start", "--json"}, {cli, "-c", "use_legacy_landlock=true"}} {
		if _, _, err := PrepareCodexDaemon(args, env, foreignConfig); err == nil {
			t.Fatal("foreign owner lifecycle accepted")
		}
	}
	run("remote-control", "start", "--json")
	waitEvent := func(want string) {
		t.Helper()
		deadline := time.After(10 * time.Second)
		for {
			select {
			case event := <-events:
				if strings.HasPrefix(event, "bad") || event == "blocked external request" {
					t.Fatalf("fixture protocol: %s", event)
				}
				if event == want {
					return
				}
			case <-deadline:
				t.Fatalf("missing native event %s", want)
			}
		}
	}
	waitEvent("enroll")
	waitEvent("websocket")
	waitEvent("pong")
	select {
	case <-proxyEvents:
	case <-time.After(time.Second):
		t.Fatal("daemon did not use CONNECT proxy")
	}
	p2, _, err := PrepareCodexDaemon([]string{cli}, env, config)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(home, "app-server-daemon", "daemon.pid"))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = PrepareCodexDaemon([]string{cli}, env, config)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(filepath.Join(home, "app-server-daemon", "daemon.pid"))
	if string(again) != string(after) {
		t.Fatal("reconnect replaced native daemon")
	}
	if _, err := p2.verify(true); err != nil {
		t.Fatal(err)
	}
	// Drain startup tunnels: only a later CONNECT can prove reattachment traffic.
	for len(proxyEvents) > 0 {
		<-proxyEvents
	}
	run("remote-control", "pair", "--json")
	waitEvent("pair")
	select {
	case <-proxyEvents:
	case <-time.After(time.Second):
		t.Fatal("native client reattachment did not use observed proxy")
	}
	afterPair, err := os.ReadFile(filepath.Join(home, "app-server-daemon", "daemon.pid"))
	if err != nil || string(afterPair) != string(after) {
		t.Fatal("native client reattachment replaced daemon")
	}
	stale := append([]string(nil), env...)
	stale = setDaemonEnv(stale, "HTTPS_PROXY", "http://stale.invalid")
	if _, _, err := PrepareCodexDaemon([]string{cli}, stale, config); err == nil {
		t.Fatal("stale daemon accepted")
	}
	live, _ := os.ReadFile(filepath.Join(home, "app-server-daemon", "daemon.pid"))
	if string(live) != string(after) {
		t.Fatal("conflict changed native daemon")
	}
	run("remote-control", "stop", "--json")
	if exists, err := p2.verify(false); err != nil || exists {
		t.Fatalf("native stop: exists=%v err=%v", exists, err)
	}
	var initialPID, reconnectPID struct {
		PID int `json:"pid"`
	}
	_ = json.Unmarshal(before, &initialPID)
	_ = json.Unmarshal(after, &reconnectPID)
	t.Logf("native bootstrap PID=%d; remote-control/reconnect PID=%d; TLS CONNECT, enrollment, relay ping/pong, pairing, conflict preservation and stop passed", initialPID.PID, reconnectPID.PID)
	_ = prepared

}
