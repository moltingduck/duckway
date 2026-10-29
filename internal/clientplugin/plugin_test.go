package clientplugin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

func TestMain(m *testing.M) {
	if os.Getenv("DUCKWAY_TEST_PLUGIN_HELPER") == "1" {
		switch os.Getenv("DUCKWAY_TEST_PLUGIN_MODE") {
		case "host":
			code, err := (Host{ConfigDir: os.Getenv("DUCKWAY_TEST_PLUGIN_CONFIG")}).Run(context.Background(), "duckllo", nil, RunOptions{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Env: []string{"DUCKWAY_TEST_PLUGIN_HELPER=1", "GOCOVERDIR=" + os.Getenv("GOCOVERDIR")}})
			fg, fgErr := unix.IoctlGetInt(int(os.Stdin.Fd()), unix.TIOCGPGRP)
			if err != nil || code != 37 || fgErr != nil || fg != syscall.Getpgrp() {
				fmt.Fprintf(os.Stdout, "HOST_FAILURE %d %v %d %v\n", code, err, fg, fgErr)
				os.Exit(2)
			}
			fmt.Fprintln(os.Stdout, "HOST_TERMINAL_RESTORED")
			os.Exit(0)
		case "wait":
			child := exec.Command(os.Args[0])
			child.Env = append(os.Environ(), "DUCKWAY_TEST_PLUGIN_MODE=child")
			if err := child.Start(); err != nil {
				os.Exit(3)
			}
			_ = os.WriteFile(os.Getenv("DUCKWAY_TEST_PLUGIN_PID"), []byte(fmt.Sprint(child.Process.Pid)), 0600)
			_ = child.Wait()
			os.Exit(0)
		case "child":
			time.Sleep(time.Hour)
			os.Exit(0)
		default:
			input, _ := io.ReadAll(os.Stdin)
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"args": os.Args[1:], "input": string(input), "proxy": os.Getenv("HTTPS_PROXY"), "ca": os.Getenv("SSL_CERT_FILE"), "name": os.Getenv("DUCKWAY_PLUGIN_NAME"), "api": os.Getenv("DUCKWAY_PLUGIN_API_VERSION"), "origin": os.Getenv("DUCKWAY_PLUGIN_ORIGIN"), "data": os.Getenv("DUCKWAY_PLUGIN_CONFIG_DIR"), "token": os.Getenv("DUCKWAY_TOKEN")})
			fmt.Fprint(os.Stderr, "helper stderr")
			os.Exit(37)
		}
	}
	os.Exit(m.Run())
}

type fixture struct {
	host         Host
	server       *httptest.Server
	mu           sync.Mutex
	manifest     Manifest
	artifact     []byte
	status       int
	redirect     string
	reportBodies [][]byte
	reportStatus int
}

func TestValidateNameReservesCompletionCommand(t *testing.T) {
	if err := ValidateName("completion"); err == nil {
		t.Fatal("completion shadows Duckway's built-in shell completion command")
	}
	if err := ValidateName("duckllo"); err != nil {
		t.Fatalf("ordinary plugin name rejected: %v", err)
	}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	f := &fixture{host: Host{ConfigDir: filepath.Join(t.TempDir(), "config")}, artifact: data, status: http.StatusOK}
	f.manifest = Manifest{APIVersion: 1, Name: "duckllo", Version: "sha256-" + digest, Artifacts: []Artifact{{OS: runtime.GOOS, Arch: runtime.GOARCH, URL: "/client/plugins/duckllo/releases/sha256-" + digest + "/" + runtime.GOOS + "/" + runtime.GOARCH, SHA256: digest, Size: int64(len(data))}}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Header.Get("X-Duckway-Token") != "fixture-client-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/client/plugins/report" {
			body, _ := io.ReadAll(r.Body)
			f.reportBodies = append(f.reportBodies, body)
			if f.reportStatus != 0 {
				w.WriteHeader(f.reportStatus)
			} else {
				w.WriteHeader(http.StatusNoContent)
			}
			return
		}
		if f.redirect != "" {
			http.Redirect(w, r, f.redirect, http.StatusFound)
			return
		}
		if f.status != http.StatusOK {
			w.WriteHeader(f.status)
			return
		}
		if r.URL.Path == "/client/plugins/duckllo/manifest" {
			_ = json.NewEncoder(w).Encode(f.manifest)
			return
		}
		if r.URL.Path == "/client/plugins" {
			_ = json.NewEncoder(w).Encode([]Manifest{f.manifest})
			return
		}
		_, _ = w.Write(f.artifact)
	}))
	f.host.ServerURL = f.server.URL
	f.host.Token = "fixture-client-token"
	t.Cleanup(f.server.Close)
	return f
}
func (f *fixture) install(t *testing.T) Installed {
	t.Helper()
	p, err := f.host.Install(context.Background(), "duckllo")
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func helperEnv(t *testing.T, extra ...string) []string {
	t.Helper()
	return append([]string{"DUCKWAY_TEST_PLUGIN_HELPER=1", "GOCOVERDIR=" + t.TempDir()}, extra...)
}

func mustWrite(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
}

func TestManifestValidation(t *testing.T) {
	f := newFixture(t)
	valid := f.manifest
	cases := []struct {
		name   string
		mutate func(*Manifest)
	}{
		{"api", func(m *Manifest) { m.APIVersion = 2 }},
		{"traversal", func(m *Manifest) { m.Name = "../escape" }},
		{"core", func(m *Manifest) { m.Name = "proxy" }},
		{"version", func(m *Manifest) { m.Version = "latest" }},
		{"duplicate", func(m *Manifest) { m.Artifacts = append(m.Artifacts, m.Artifacts[0]) }},
		{"size", func(m *Manifest) { m.Artifacts[0].Size = MaxArtifactSize + 1 }},
		{"digest", func(m *Manifest) { m.Artifacts[0].SHA256 = strings.Repeat("a", 63) }},
		{"absolute-url", func(m *Manifest) { m.Artifacts[0].URL = "https://evil.invalid/artifact" }},
		{"cross-origin", func(m *Manifest) { m.Artifacts[0].URL = "//evil.invalid/artifact" }},
		{"encoded-traversal", func(m *Manifest) { m.Artifacts[0].URL = "/safe/%2e%2e/evil" }},
		{"query", func(m *Manifest) { m.Artifacts[0].URL = "/artifact?secret=value" }},
		{"fragment", func(m *Manifest) { m.Artifacts[0].URL = "/artifact#secret" }},
		{"dot-segment", func(m *Manifest) { m.Artifacts[0].URL = "/safe/../evil" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := valid
			m.Artifacts = append([]Artifact(nil), valid.Artifacts...)
			tc.mutate(&m)
			if err := m.Validate(); err == nil {
				t.Fatal("accepted invalid manifest")
			}
		})
	}
	for _, raw := range []string{"https://user:secret@example.invalid/m", "https://example.invalid/m?token=secret", "https://example.invalid/m#secret", "file:///manifest", "http://example.invalid/a/../m", "https://example.invalid/m?", "https://example.invalid/m#"} {
		if _, err := SourceURL(raw); err == nil || strings.Contains(err.Error(), "secret") {
			t.Errorf("unsafe source accepted or leaked: %q", raw)
		}
	}
	if _, err := DecodeManifest(strings.NewReader(`{"api_version":1,"unknown":1}`)); err == nil {
		t.Fatal("unknown manifest fields accepted")
	}
	if _, err := DecodeManifest(strings.NewReader(strings.Repeat("x", maxManifestSize+1))); err == nil {
		t.Fatal("oversized manifest accepted")
	}
}

func TestInstallRunUpdateAndPreserveData(t *testing.T) {
	f := newFixture(t)
	first := f.install(t)
	list, err := f.host.List()
	if err != nil || len(list) != 1 || list[0].Manifest.Name != "duckllo" {
		t.Fatalf("list: %v %v", list, err)
	}
	dataPath := filepath.Join(f.host.root(), "duckllo", "data", "keep")
	mustWrite(t, dataPath, []byte("private state"), 0600)
	var out, stderr bytes.Buffer
	args := []string{"pair", "--server", "https://example.invalid", "literal $(touch never)", "", "a b", "--"}
	code, err := f.host.Run(context.Background(), "duckllo", args, RunOptions{Stdin: strings.NewReader("invite via stdin"), Stdout: &out, Stderr: &stderr, Env: helperEnv(t, "DUCKWAY_PLUGIN_NAME=spoof", "HTTPS_PROXY=http://127.0.0.1:18080", "SSL_CERT_FILE=/fixture/ca.pem", "DUCKWAY_TOKEN=must-not-reach-plugin")})
	if err != nil || code != 37 {
		t.Fatalf("run code=%d err=%v", code, err)
	}
	var got struct {
		Args                                             []string
		Input, Proxy, CA, Name, API, Origin, Data, Token string
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got.Args) != fmt.Sprint(args) || got.Input != "invite via stdin" || got.Proxy != "http://127.0.0.1:18080" || got.CA != "/fixture/ca.pem" || got.Name != "duckllo" || got.API != "1" || got.Origin != f.server.URL || got.Token != "" || stderr.String() != "helper stderr" {
		t.Fatalf("forwarding mismatch: %+v stderr=%q", got, stderr.String())
	}
	if got.Data != filepath.Dir(dataPath) {
		t.Fatalf("wrong plugin data path %s", got.Data)
	}
	f.mu.Lock()
	f.manifest.Version = "sha256-" + strings.Repeat("a", 64)
	f.manifest.Artifacts[0].URL = managedArtifactPath(f.manifest, f.manifest.Artifacts[0])
	f.mu.Unlock()
	second, err := f.host.Update(context.Background(), "duckllo")
	if err != nil {
		t.Fatal(err)
	}
	if second.Release == first.Release {
		t.Fatal("update did not atomically replace release")
	}
	data, err := os.ReadFile(dataPath)
	if err != nil || string(data) != "private state" {
		t.Fatal("update changed plugin data")
	}
	if _, err := f.host.Install(context.Background(), "duckllo"); err == nil {
		t.Fatal("reinstall silently replaced plugin")
	}
}

func TestFailedUpdatesPreserveActiveRelease(t *testing.T) {
	f := newFixture(t)
	before := f.install(t)
	metadata := filepath.Join(f.host.root(), "duckllo", "current.json")
	original, err := os.ReadFile(metadata)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		mutate func()
	}{
		{"checksum", func() { f.artifact = bytes.Repeat([]byte("x"), len(f.artifact)) }},
		{"truncated", func() { f.artifact = f.artifact[:len(f.artifact)-1] }},
		{"http", func() { f.status = http.StatusBadGateway }},
		{"rename", func() { f.manifest.Name = "other" }},
		{"platform", func() { f.manifest.Artifacts[0].Arch = "unknown" }},
	}
	originalArtifact := append([]byte(nil), f.artifact...)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f.mu.Lock()
			f.artifact = append([]byte(nil), originalArtifact...)
			f.manifest.Name = "duckllo"
			f.manifest.Artifacts[0].Arch = runtime.GOARCH
			f.status = http.StatusOK
			tc.mutate()
			f.mu.Unlock()
			if _, err := f.host.Update(context.Background(), "duckllo"); err == nil {
				t.Fatal("invalid update succeeded")
			}
			current, err := os.ReadFile(metadata)
			if err != nil || !bytes.Equal(current, original) {
				t.Fatal("failed update changed activation")
			}
			entries, err := os.ReadDir(filepath.Join(f.host.root(), "duckllo", "releases"))
			if err != nil || len(entries) != 1 || entries[0].Name() != before.Release {
				t.Fatal("failed update left staged release")
			}
		})
	}
}

func TestRedirectsNeverFollowed(t *testing.T) {
	f := newFixture(t)
	var hit bool
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true; w.WriteHeader(200) }))
	t.Cleanup(destination.Close)
	f.mu.Lock()
	f.redirect = destination.URL
	f.mu.Unlock()
	if _, err := f.host.Install(context.Background(), "duckllo"); err == nil {
		t.Fatal("redirect accepted")
	}
	if hit {
		t.Fatal("redirect destination contacted")
	}
}

func TestRegistryRejectsUnsafePathsAndTampering(t *testing.T) {
	for _, target := range []string{"config", "plugins", "name", "metadata", "release", "executable"} {
		t.Run(target, func(t *testing.T) {
			f := newFixture(t)
			if target == "config" || target == "plugins" {
				outside := t.TempDir()
				path := f.host.ConfigDir
				if target == "plugins" {
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
					path = f.host.root()
				}
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
				if _, err := f.host.Install(context.Background(), "duckllo"); err == nil {
					t.Fatal("symlink registry accepted")
				}
				entries, _ := os.ReadDir(outside)
				if len(entries) != 0 {
					t.Fatal("wrote outside registry")
				}
				return
			}
			p := f.install(t)
			path := filepath.Join(f.host.root(), "duckllo")
			switch target {
			case "metadata":
				path = filepath.Join(path, "current.json")
			case "release":
				path = filepath.Join(path, "releases", p.Release)
			case "executable":
				path = filepath.Join(path, "releases", p.Release, "executable")
			}
			outside := t.TempDir()
			if target == "metadata" || target == "executable" {
				outside = filepath.Join(outside, "outside")
				mustWrite(t, outside, []byte("unchanged"), 0600)
			}
			if err := os.RemoveAll(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, path); err != nil {
				t.Fatal(err)
			}
			if _, err := f.host.Run(context.Background(), "duckllo", nil, RunOptions{}); err == nil {
				t.Fatal("symlink path accepted")
			}
		})
	}
	f := newFixture(t)
	p := f.install(t)
	mustWrite(t, filepath.Join(f.host.root(), "duckllo", "releases", p.Release, "executable"), []byte("corrupt"), 0700)
	if _, err := f.host.Run(context.Background(), "duckllo", nil, RunOptions{}); err == nil {
		t.Fatal("modified executable ran")
	}
	if _, err := f.host.Run(context.Background(), "proxy", nil, RunOptions{}); err == nil {
		t.Fatal("reserved core plugin ran")
	}
}

func TestCancelTerminatesPluginProcessGroup(t *testing.T) {
	f := newFixture(t)
	f.install(t)
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		_, err := f.host.Run(ctx, "duckllo", nil, RunOptions{Env: helperEnv(t, "DUCKWAY_TEST_PLUGIN_MODE=wait", "DUCKWAY_TEST_PLUGIN_PID="+pidFile)})
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	var pid int
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(pidFile)
		if err == nil {
			_, _ = fmt.Sscan(string(data), &pid)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		cancel()
		waitErr := <-done
		t.Fatalf("helper did not start (run error: %v)", waitErr)
	}
	// Cleanup only the exact helper group descendant if a failed test leaves it.
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancellation not reported")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("plugin cancellation did not wait/return")
	}
	// An adopted zombie may await container PID1; it is no longer executing.
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return
		}
		data, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if strings.Contains(string(data), ") Z ") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("child process survived cancellation")
}

func TestConcurrentInstallHasSingleActivation(t *testing.T) {
	f := newFixture(t)
	done := make(chan error, 2)
	for range 2 {
		go func() { _, err := f.host.Install(context.Background(), "duckllo"); done <- err }()
	}
	a, b := <-done, <-done
	if (a == nil) == (b == nil) {
		t.Fatalf("wanted one success: %v / %v", a, b)
	}
	list, err := f.host.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("list after race: %v %v", list, err)
	}
}

func TestCancelWhileWaitingForRegistryLock(t *testing.T) {
	f := newFixture(t)
	lock, err := f.host.lock(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unlock(lock) })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	t.Cleanup(cancel)
	started := time.Now()
	if _, err := f.host.Install(ctx, "duckllo"); err != context.DeadlineExceeded {
		t.Fatalf("lock cancellation: %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("lock cancellation was not bounded")
	}
}

func TestArtifactRedirectAndOversizeRejected(t *testing.T) {
	for _, mode := range []string{"redirect", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("artifact redirect was followed") }))
			t.Cleanup(destination.Close)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/client/plugins/duckllo/manifest" {
					_ = json.NewEncoder(w).Encode(f.manifest)
					return
				}
				if mode == "redirect" {
					http.Redirect(w, r, destination.URL, http.StatusFound)
					return
				}
				w.Header().Set("Content-Type", "application/octet-stream")
				w.(http.Flusher).Flush() // unknown Content-Length exercises streaming size bound
				_, _ = w.Write(append(f.artifact, byte('x')))
			}))
			t.Cleanup(server.Close)
			f.host.ServerURL = server.URL
			if _, err := f.host.Install(context.Background(), "duckllo"); err == nil {
				t.Fatal("unsafe artifact accepted")
			}
			list, err := f.host.List()
			if err != nil || len(list) != 0 {
				t.Fatalf("failed artifact activated: %v %v", list, err)
			}
		})
	}
}

func TestForegroundTerminalInputAndRestoration(t *testing.T) {
	f := newFixture(t)
	f.install(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), "DUCKWAY_TEST_PLUGIN_HELPER=1", "DUCKWAY_TEST_PLUGIN_MODE=host", "DUCKWAY_TEST_PLUGIN_CONFIG="+f.host.ConfigDir, "GOCOVERDIR="+t.TempDir())
	terminal, err := pty.Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = terminal.Close(); _ = cmd.Process.Kill() })
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	// Canonical input supplies a line followed by EOF to the actual foreground
	// plugin, proving it can read its controlling TTY without SIGTTIN.
	if _, err := terminal.Write([]byte("terminal input\n\x04")); err != nil {
		t.Fatal(err)
	}
	output := make(chan string, 1)
	go func() { data, _ := io.ReadAll(terminal); output <- string(data) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("terminal host: %v", err)
		}
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = terminal.Close()
		waitErr := <-done
		t.Fatalf("interactive plugin stopped or did not return (run error: %v)", waitErr)
	}
	select {
	case got := <-output:
		if !strings.Contains(got, "HOST_TERMINAL_RESTORED") || !strings.Contains(got, `"input":"terminal input\n"`) {
			t.Fatalf("terminal output: %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("PTY output did not close")
	}
}

func TestManagedCatalogAndServerIdentity(t *testing.T) {
	f := newFixture(t)
	catalog, err := f.host.Catalog(context.Background())
	if err != nil || len(catalog) != 1 || catalog[0].Name != "duckllo" {
		t.Fatalf("catalog: %v %v", catalog, err)
	}
	first := f.install(t)
	if first.ServerOrigin != f.server.URL {
		t.Fatal("configured server origin was not persisted")
	}
	other := newFixture(t)
	changed := f.host
	changed.ServerURL = other.server.URL
	if _, err := changed.Update(context.Background(), "duckllo"); err == nil {
		t.Fatal("update switched configured server")
	}
	installed, err := f.host.List()
	if err != nil || installed[0].Release != first.Release {
		t.Fatal("server identity failure changed activation")
	}
	missing := f.host
	missing.Token = ""
	if _, err := missing.Catalog(context.Background()); err == nil {
		t.Fatal("catalog permitted missing client token")
	}
	wrong := f.host
	wrong.Token = "wrong"
	if _, err := wrong.Install(context.Background(), "other"); err == nil {
		t.Fatal("install permitted invalid client token")
	}
	if _, err := f.host.Install(context.Background(), f.server.URL+"/arbitrary"); err == nil {
		t.Fatal("client accepted arbitrary source URL")
	}
	f.mu.Lock()
	f.manifest.Artifacts[0].URL = "/download/duckway-client-linux-arm64"
	f.mu.Unlock()
	if _, err := f.host.Catalog(context.Background()); err == nil {
		t.Fatal("catalog accepted a non-plugin server route")
	}
	if _, err := f.host.Update(context.Background(), "duckllo"); err == nil {
		t.Fatal("update accepted a non-plugin server route")
	}
	for _, raw := range []string{"https://user:password@example.invalid", "https://example.invalid/path", "https://example.invalid/?token=secret"} {
		h := Host{ServerURL: raw, Token: "fixture"}
		if _, err := h.Catalog(context.Background()); err == nil || strings.Contains(err.Error(), "password") || strings.Contains(err.Error(), "secret") {
			t.Fatal("invalid server URL accepted or leaked")
		}
	}
}

func TestUnsafeAncestorsRejected(t *testing.T) {
	for _, mode := range []os.FileMode{0777, 0770} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			ancestor := filepath.Join(t.TempDir(), "ancestor")
			if err := os.Mkdir(ancestor, mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(ancestor, mode); err != nil {
				t.Fatal(err)
			}
			if err := privateDir(filepath.Join(ancestor, "owned", "config")); err == nil {
				t.Fatal("nonsticky writable ancestor accepted")
			}
		})
	}
	sticky := filepath.Join(t.TempDir(), "sticky")
	if err := os.Mkdir(sticky, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sticky, os.ModeSticky|0777); err != nil {
		t.Fatal(err)
	}
	if err := privateDir(filepath.Join(sticky, "owned", "config")); err != nil {
		t.Fatalf("safe sticky parent rejected: %v", err)
	}
	// Foreign ownership can only be constructed without privileges in a separate
	// user namespace. Root test containers exercise this check without touching any
	// pre-existing directory or changing the test process's effective identity.
	if os.Geteuid() == 0 {
		foreign := filepath.Join(t.TempDir(), "foreign")
		if err := os.Mkdir(foreign, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(foreign, 12345, 12345); err != nil {
			t.Skip("test namespace cannot chown owned fixture")
		}
		t.Cleanup(func() { _ = os.Chown(foreign, 0, 0) })
		if err := privateDir(filepath.Join(foreign, "config")); err == nil {
			t.Fatal("foreign-owned ancestor accepted")
		}
	}
}
