package client

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoctorSnapshotAccessAndObservation(t *testing.T) {
	p := &httpsProxy{token: "private-token"}
	for _, tc := range []struct {
		name, peer, host, target, token string
		want                            int
	}{
		{"valid", "127.0.0.1:22", "127.0.0.1:18080", "/proxy/doctor", "private-token", 200},
		{"remote", "192.0.2.1:22", "127.0.0.1:18080", "/proxy/doctor", "private-token", 403},
		{"authority", "127.0.0.1:22", "evil.example:18080", "/proxy/doctor", "private-token", 403},
		{"port", "127.0.0.1:22", "127.0.0.1:1", "/proxy/doctor", "private-token", 403},
		{"absolute", "127.0.0.1:22", "127.0.0.1:18080", "http://127.0.0.1:18080/proxy/doctor", "private-token", 403},
		{"unauthorized", "127.0.0.1:22", "127.0.0.1:18080", "/proxy/doctor", "", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.target, nil)
			req.RemoteAddr = tc.peer
			req.Host = tc.host
			req.Header.Set("X-Duckway-Token", tc.token)
			req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 18080}))
			w := httptest.NewRecorder()
			p.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("status %d", w.Code)
			}
			if strings.Contains(w.Body.String(), "private-token") {
				t.Fatal("leaked token")
			}
		})
	}
	req := httptest.NewRequest("GET", "/secret?token=secret", nil)
	p.observeDoctor(hostEntry{Service: "anthropic"}, req, false)
	req.Header.Set("Authorization", "Bearer real-secret")
	p.observeDoctor(hostEntry{Service: "anthropic"}, req, false)
	p.observeDoctor(hostEntry{Service: "anthropic"}, req, true)
	p.observeDoctor(hostEntry{Service: "malicious-secret"}, req, true)
	if p.doctor.counts[3] != (doctorCounts{Missing: 1, Other: 1, Phantom: 1}) {
		t.Fatalf("counts %+v", p.doctor.counts)
	}
}
func TestDoctorCredentialsBoundedAndSecretSafe(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	secret := "actual-secret-token"
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"tokens":{"access_token":"`+secret+`"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"dw_fixture"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "keys.env"), []byte("ANTHROPIC_API_KEY=dw_fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var r DoctorReport
	r.addCredentialChecks(home, nil)
	out := r.FormatText()
	if strings.Contains(out, secret) || strings.Contains(out, "dw_fixture") {
		t.Fatal("credential leaked")
	}
	for _, want := range []string{"CODEX_HOME override", "matches synced key", "CLAUDE_CONFIG_DIR", "not recognized"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s", want)
		}
	}
	if err := os.Symlink(filepath.Join(home, "auth.json"), filepath.Join(home, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := doctorFile(filepath.Join(home, "link")); err == nil {
		t.Fatal("accepted symlink")
	}
	if err := os.WriteFile(filepath.Join(home, "big"), make([]byte, (1<<20)+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := doctorFile(filepath.Join(home, "big")); err == nil {
		t.Fatal("accepted oversized file")
	}
}
func TestDoctorObservedUnknownAndUnsupported(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(map[bool]string{true: "unknown", false: "old"}[valid], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if valid {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"schema":"duckway-proxy-doctor-v1","counts":[{},{},{},{}]}`))
				} else {
					http.Error(w, "secret raw error", 404)
				}
			}))
			t.Cleanup(server.Close)
			port := server.Listener.Addr().(*net.TCPAddr).Port
			var r DoctorReport
			r.addObservedRequests(&Config{ProxyPort: port, Token: "secret"})
			out := r.FormatText()
			if !strings.Contains(out, "UNKNOWN") || strings.Contains(out, "secret") {
				t.Fatal(out)
			}
		})
	}
}

func TestDoctorExternalAbsolutePathForwarded(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/proxy/doctor" || r.Header.Get("X-Duckway-Token") != "" {
			t.Error("unexpected path or forwarded diagnostic credential")
		}
		_, _ = w.Write([]byte("external origin"))
	}))
	t.Cleanup(origin.Close)
	p := &httpsProxy{token: "private-token", httpClient: &http.Client{Transport: directTransport}}
	req := httptest.NewRequest("GET", origin.URL+"/proxy/doctor", nil)
	req.Header.Set("X-Duckway-Token", "private-token")
	req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 18080}))
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)
	if w.Code != 200 || w.Body.String() != "external origin" {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
}
