package client

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Child processes use inherited pipes for ordering; no shared runtime services
// or timing assumptions establish the stale read. The parent releases the
// holder only after the contender starts and is observed waiting.
func TestCodexConfigLockSubprocessOverlap(t *testing.T) {
	for _, holder := range []string{"hold-mcp", "hold-native", "hold-native-transient"} {
		t.Run(holder, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("CODEX_HOME", dir)
			path := filepath.Join(dir, "config.toml")
			initial := "cli_auth_credentials_store = \"keyring\"\nmodel_provider = \"duckway-openai\"\n[model_providers.duckway-openai]\nname = \"old\"\n[mcp_servers.duckway-cc]\ncommand = \"duckway\"\n"
			if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
				t.Fatal(err)
			}
			a := startCodexLockChild(t, holder)
			a.event(t, "read")
			contender := "native"
			if holder == "hold-native" {
				contender = "mcp"
			}
			if holder == "hold-native-transient" {
				contender = "transient"
			}
			b := startCodexLockChild(t, contender)
			b.event(t, "attempt")
			if contender == "transient" {
				b.event(t, "fetched")
			}
			select {
			case event := <-b.events:
				t.Fatalf("config writer bypassed held transaction lock: %q", event)
			case <-time.After(150 * time.Millisecond):
			}
			a.release(t)
			a.event(t, "done")
			b.event(t, "done")
			a.wait(t)
			b.wait(t)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), "cli_auth_credentials_store = \"file\"") || strings.Contains(string(data), "model_provider = \"duckway-openai\"") || !strings.Contains(string(data), "[mcp_servers.duckway-cc]") {
				t.Fatalf("overlapping processes lost native or MCP publication: %q", data)
			}
		})
	}
}

func TestCodexConfigLockClosesOnExec(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	child := startCodexLockChild(t, "exec")
	child.event(t, "exec-ready")
	path, err := codexConfigTOMLPath()
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(filepath.Join(filepath.Dir(path), ".duckway-config.lock"), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("exec retained config lock: %v", err)
	}
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	child.release(t)
	child.event(t, "done")
	child.wait(t)
}

func TestCodexConfigLockSafetyAndPersistentInode(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("CODEX_HOME", dir)
			path := filepath.Join(dir, ".duckway-config.lock")
			target := filepath.Join(dir, "target")
			if err := os.WriteFile(target, []byte("unchanged"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(target, 0644); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(target, path)
			case "hardlink":
				err = os.Link(target, path)
			case "directory":
				err = os.Mkdir(path, 0700)
			}
			if err != nil {
				t.Fatal(err)
			}
			if unlock, err := lockCodexConfig(); err == nil {
				unlock()
				t.Fatal("unsafe lockfile accepted")
			}
			info, err := os.Stat(target)
			if err != nil || info.Mode().Perm() != 0644 {
				t.Fatalf("lock attempt changed target permissions: %v, %v", info, err)
			}
		})
	}
	t.Run("persistent inode", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("CODEX_HOME", dir)
		path := filepath.Join(dir, ".duckway-config.lock")
		var first os.FileInfo
		for i := 0; i < 2; i++ {
			unlock, err := lockCodexConfig()
			if err != nil {
				t.Fatal(err)
			}
			unlock()
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0600 || (first != nil && !os.SameFile(first, info)) {
				t.Fatal("lockfile permissions or inode changed between transactions")
			}
			first = info
		}
	})
}

// Invoked only by startCodexLockChild, in a separate test process.
func TestCodexConfigLockChild(t *testing.T) {
	mode := os.Getenv("DUCKWAY_CODEX_LOCK_CHILD")
	if mode == "" {
		return
	}
	ready, release := os.NewFile(3, "ready"), os.NewFile(4, "release")
	t.Cleanup(func() { _ = ready.Close(); _ = release.Close() })
	event := func(s string) {
		if _, err := fmt.Fprintln(ready, s); err != nil {
			t.Fatal(err)
		}
	}
	wait := func() {
		var b [1]byte
		if _, err := release.Read(b[:]); err != nil {
			t.Fatal(err)
		}
	}
	if mode == "after-exec" {
		event("exec-ready")
		wait()
		event("done")
		return
	}
	if mode == "exec" {
		unlock, err := lockCodexConfig()
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		if err := os.Setenv("DUCKWAY_CODEX_LOCK_CHILD", "after-exec"); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Exec(os.Args[0], []string{os.Args[0], "-test.run=^TestCodexConfigLockChild$"}, os.Environ()); err != nil {
			t.Fatal(err)
		}
	}
	if strings.HasPrefix(mode, "hold-") {
		unlock, err := lockCodexConfig()
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		path, err := codexConfigTOMLPath()
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		event("read")
		wait()
		next := string(data)
		if mode == "hold-mcp" {
			next = replaceTOMLSection(next, "mcp_servers.duckway-cc", codexMCPSection("fixture"))
		} else {
			auth, err := json.Marshal(testManagedCodexPhantomCredentials())
			if err != nil {
				t.Fatal(err)
			}
			if err := writeFileAtomic(filepath.Join(filepath.Dir(path), "auth.json"), auth, 0600); err != nil {
				t.Fatal(err)
			}
			next = replaceTopLevelTOMLString(next, "cli_auth_credentials_store", "file")
			next = removeTopLevelTOMLStringValue(next, "model_provider", "duckway-openai")
			next = removeTOMLSection(next, "model_providers.duckway-openai")
		}
		if err := writeFileAtomic(path, []byte(next), 0600); err != nil {
			t.Fatal(err)
		}
		event("done")
		return
	}
	switch mode {
	case "native", "transient":
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if mode == "transient" {
				event("fetched")
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_ = json.NewEncoder(w).Encode(testManagedCodexPhantomCredentials())
		}))
		t.Cleanup(server.Close)
		event("attempt")
		if err := SyncCodexAuthConfig(t.TempDir(), &Config{ServerURL: server.URL}); err != nil {
			t.Fatal(err)
		}
	case "mcp":
		event("attempt")
		if err := writeCodexMCP("fixture", nil); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown child mode %q", mode)
	}
	event("done")
}

type codexLockChild struct {
	cmd      *exec.Cmd
	output   bytes.Buffer
	events   chan string
	finished chan struct{}
	releaseW *os.File
	err      error
}

func startCodexLockChild(t *testing.T, mode string) *codexLockChild {
	t.Helper()
	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = readyR.Close(); _ = readyW.Close() })
	releaseR, releaseW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = releaseR.Close(); _ = releaseW.Close() })
	child := &codexLockChild{events: make(chan string, 8), finished: make(chan struct{}), releaseW: releaseW}
	child.cmd = exec.Command(os.Args[0], "-test.run=^TestCodexConfigLockChild$")
	child.cmd.Env = append(os.Environ(), "DUCKWAY_CODEX_LOCK_CHILD="+mode)
	child.cmd.ExtraFiles = []*os.File{readyW, releaseR}
	child.cmd.Stdout, child.cmd.Stderr = &child.output, &child.output
	if err := child.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = readyW.Close()
	_ = releaseR.Close()
	go func() {
		scanner := bufio.NewScanner(readyR)
		for scanner.Scan() {
			child.events <- scanner.Text()
		}
		close(child.events)
	}()
	go func() { child.err = child.cmd.Wait(); close(child.finished) }()
	t.Cleanup(func() {
		_ = child.cmd.Process.Kill()
		select {
		case <-child.finished:
		case <-time.After(5 * time.Second):
			t.Error("config lock subprocess did not exit after kill")
		}
	})
	return child
}

func (c *codexLockChild) event(t *testing.T, want string) {
	t.Helper()
	select {
	case got := <-c.events:
		if got != want {
			t.Fatalf("child event = %q, want %q", got, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for child event %q", want)
	}
}

func (c *codexLockChild) release(t *testing.T) {
	t.Helper()
	if _, err := c.releaseW.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
}

func (c *codexLockChild) wait(t *testing.T) {
	t.Helper()
	select {
	case <-c.finished:
		if c.err != nil {
			t.Fatalf("config lock child failed: %v\n%s", c.err, c.output.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for config lock child exit")
	}
}
