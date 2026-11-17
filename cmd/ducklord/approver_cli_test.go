package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func approverTestServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token || r.URL.Path != "/approver/approvals" {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`[{"id":"a1","status":"pending"},{"id":"a2","status":"pending"}]`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestApproverLoginStoresValidatedTokenWith0600AndLogoutRemoves(t *testing.T) {
	srv := approverTestServer(t, "dwa_good")
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	var out bytes.Buffer
	if err := runApproverCommand([]string{"login", "--server", srv.URL, "--config", cfg}, strings.NewReader("dwa_good\n"), &out); err != nil {
		t.Fatalf("login: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "dwa_good") || !strings.Contains(out.String(), "2 pending") {
		t.Fatalf("output = %q", out.String())
	}
	path := filepath.Join(dir, "approver.json")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("stat = %v, %v", info, err)
	}
	creds, err := ducklord.LoadApproverCredentials(path)
	if err != nil || creds.Token != "dwa_good" {
		t.Fatalf("creds = %v, %v", creds, err)
	}
	// config.yaml is never created or modified by login.
	if _, err := os.Stat(cfg); !os.IsNotExist(err) {
		t.Fatalf("login touched config.yaml: %v", err)
	}
	// A repeat login reuses the stored server.
	out.Reset()
	if err := runApproverCommand([]string{"login", "--config", cfg}, strings.NewReader("dwa_good\n"), &out); err != nil {
		t.Fatalf("relogin: %v", err)
	}
	if err := runApproverCommand([]string{"logout", "--config", cfg}, nil, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("credentials remain after logout: %v", err)
	}
}

func TestApproverLoginRejectsBadTokenWithoutStoring(t *testing.T) {
	srv := approverTestServer(t, "dwa_good")
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	err := runApproverCommand([]string{"login", "--server", srv.URL, "--config", cfg}, strings.NewReader("dwa_bad\n"), &bytes.Buffer{})
	if err == nil || strings.Contains(err.Error(), "dwa_bad") {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(approverCredentialsFor(cfg)); !os.IsNotExist(statErr) {
		t.Fatal("rejected token was stored")
	}
	for _, args := range [][]string{{"login", "--config", cfg}, {"login", "--server", srv.URL, "--config", cfg}, {}, {"bogus"}, {"logout", "--server", "x"}} {
		in := strings.NewReader("")
		if runApproverCommand(args, in, &bytes.Buffer{}) == nil {
			t.Fatalf("args %v accepted", args)
		}
	}
}

func TestApproverLoginRefusesPlainHTTPForNonLoopbackUnlessInsecure(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	var out bytes.Buffer
	err := runApproverCommand([]string{"login", "--server", "http://duck.example.com:9", "--config", cfg}, strings.NewReader("dwa_x\n"), &out)
	if err == nil || !strings.Contains(err.Error(), "--insecure") {
		t.Fatalf("plain http to a remote host must be refused, got %v", err)
	}
	if _, serr := os.Stat(filepath.Join(filepath.Dir(cfg), "approver.json")); !os.IsNotExist(serr) {
		t.Fatalf("credentials stored after refusal: %v", serr)
	}
	// --insecure passes the scheme gate (the unreachable host then fails validation, not the scheme check).
	err = runApproverCommand([]string{"login", "--server", "http://127.0.0.1:1", "--insecure", "--config", cfg}, strings.NewReader("dwa_x\n"), &out)
	if err == nil || strings.Contains(err.Error(), "refusing plain http") {
		t.Fatalf("loopback with --insecure: %v", err)
	}
	if err := runApproverCommand([]string{"login", "--server", "http://duck.example.com:9", "--insecure", "--config", cfg}, strings.NewReader("dwa_x\n"), &out); err != nil && strings.Contains(err.Error(), "refusing plain http") {
		t.Fatalf("--insecure not honoured: %v", err)
	}
	if !strings.Contains(out.String(), "plain http") {
		t.Fatalf("expected the plain-http warning with --insecure:\n%s", out.String())
	}
}

func TestApproverLoginWarnsWhenAgentsRunLocally(t *testing.T) {
	old := localAgentProbe
	localAgentProbe = func() (string, bool) { return "/x/ducklion.sock", true }
	t.Cleanup(func() { localAgentProbe = old })
	srv := approverTestServer(t, "dwa_good")
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	var out bytes.Buffer
	if err := runApproverCommand([]string{"login", "--server", srv.URL, "--config", cfg}, strings.NewReader("dwa_good\n"), &out); err != nil {
		t.Fatalf("a warning must not block login: %v", err)
	}
	if !strings.Contains(out.String(), "warning: a local ducklion daemon") || strings.Contains(out.String(), "dwa_good") {
		t.Fatalf("output = %q", out.String())
	}
	localAgentProbe = func() (string, bool) { return "", false }
	out.Reset()
	if err := runApproverCommand([]string{"login", "--config", cfg}, strings.NewReader("dwa_good\n"), &out); err != nil || strings.Contains(out.String(), "warning") {
		t.Fatalf("no daemon, no warning: %v %q", err, out.String())
	}
}
