package ducklord

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadConfigNormalizesClients(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("name: desk-1\nhosts:\n  - name: vulns\n    host: vulns.ts\n    user: duck\n    group: ctf\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Clients[0]
	if cfg.Name != "desk-1" {
		t.Fatalf("name = %q", cfg.Name)
	}
	if got.Ducklion != "ducklion" || got.SSH != "ssh" || got.Target() != "duck@vulns.ts" {
		t.Fatalf("client = %+v", got)
	}
}

func TestSaveConfigWritesYAMLAndMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &Config{Name: "desk-1", Clients: []Client{{Name: "host-a", Host: "host-a"}}}
	if err := SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "name: desk-1") || strings.HasPrefix(string(data), "{") {
		t.Fatalf("unexpected YAML: %q", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
}

func TestResolveOwnerNamePrecedenceAndValidation(t *testing.T) {
	name, err := ResolveOwnerName("flag-name", "config-name")
	if err != nil || name != "flag-name" {
		t.Fatalf("name=%q err=%v", name, err)
	}
	name, err = ResolveOwnerName("", "config-name")
	if err != nil || name != "config-name" {
		t.Fatalf("name=%q err=%v", name, err)
	}
	for _, invalid := range []string{"has space", "中文", "slash/name", strings.Repeat("a", 65)} {
		if ValidOwnerName(invalid) {
			t.Fatalf("invalid owner accepted: %q", invalid)
		}
	}
}

func TestInstanceLockRejectsConcurrentTUI(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	first, err := AcquireInstanceLock()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := AcquireInstanceLock(); err == nil {
		t.Fatal("second instance lock acquired")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := AcquireInstanceLock()
	if err != nil {
		t.Fatalf("lock not reusable after close: %v", err)
	}
	second.Close()
}

func TestLoadConfigRejectsUnknownFieldsAndMultipleDocuments(t *testing.T) {
	for name, contents := range map[string]string{
		"unknown":  "hostz: []\n",
		"multiple": "hosts: []\n---\nhosts: []\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadConfig(path); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}

func TestLoadConfigRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.yaml")
	if err := os.WriteFile(target, []byte("hosts: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "config.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(link); err == nil {
		t.Fatal("symlink config accepted")
	}
}

func TestSaveConfigRejectsDuplicateNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &Config{Clients: []Client{{Name: "same", Host: "one"}, {Name: "same", Host: "two"}}}
	if err := SaveConfig(path, cfg); err == nil {
		t.Fatal("duplicate names saved")
	}
}

func TestLoadConfigAllowsEmptyClients(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`{"hosts":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Clients) != 0 {
		t.Fatalf("clients = %+v", cfg.Clients)
	}
}

func TestLoadConfigRejectsUnsafeHost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`{"hosts":[{"name":"bad","host":"host;touch /tmp/pwn"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("unsafe host accepted")
	}
}

func TestLoadConfigRejectsHostOptionInjection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`{"hosts":[{"name":"bad","host":"-oProxyCommand=/tmp/pwn"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("ssh option host accepted")
	}
}

func TestLoadConfigAcceptsDuckwayDucklionSubcommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`{"hosts":[{"name":"vulns","host":"vulns.ts","ducklion":"duckway ducklion"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Clients[0].DucklionArgs("list")
	want := []string{"duckway", "ducklion", "list"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DucklionArgs = %#v, want %#v", got, want)
	}
}

func TestLoadConfigAcceptsSSHCommandLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`{"hosts":[{"name":"vulns","host":"vulns.ts","ssh":"ssh -p 2222 -i /tmp/id_ed25519"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Clients[0].SSHCommandParts()
	want := []string{"ssh", "-p", "2222", "-i", "/tmp/id_ed25519"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SSHCommandParts = %#v, want %#v", got, want)
	}
}

func TestConfigRemoveClient(t *testing.T) {
	cfg := &Config{Clients: []Client{{Name: "a", Host: "a"}, {Name: "b", Host: "b"}}}
	if !cfg.RemoveClient("a") {
		t.Fatal("client a was not removed")
	}
	if len(cfg.Clients) != 1 || cfg.Clients[0].Name != "b" {
		t.Fatalf("clients = %+v", cfg.Clients)
	}
	if cfg.RemoveClient("missing") {
		t.Fatal("missing client removed")
	}
}

func TestSSHArgsDoNotUseLocalShell(t *testing.T) {
	c := Client{Name: "vulns", Host: "vulns.ts", User: "duck", Ducklion: "ducklion", SSH: "ssh"}
	got := SSHArgs(c, false, "ducklion", "send", "alpha", "hello; rm -rf /")
	want := []string{"-o", "BatchMode=yes", "-o", "ForwardAgent=no", "-o", "ClearAllForwardings=yes", "duck@vulns.ts", "ducklion send alpha 'hello; rm -rf /'"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SSHArgs = %#v, want %#v", got, want)
	}
}

func TestSSHArgsPutTTYBeforeTarget(t *testing.T) {
	c := Client{Name: "vulns", Host: "vulns.ts", User: "duck", Ducklion: "ducklion", SSH: "ssh"}
	got := SSHArgs(c, true, "ducklion", "attach", "alpha")
	want := []string{"-o", "BatchMode=yes", "-o", "ForwardAgent=no", "-o", "ClearAllForwardings=yes", "-t", "duck@vulns.ts", "ducklion attach alpha"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SSHArgs = %#v, want %#v", got, want)
	}
}

func TestSSHArgsSupportDuckwayDucklionSubcommand(t *testing.T) {
	c := Client{Name: "vulns", Host: "vulns.ts", User: "duck", Ducklion: "duckway ducklion", SSH: "ssh"}
	got := SSHArgs(c, false, c.DucklionArgs("list", "--json")...)
	want := []string{"-o", "BatchMode=yes", "-o", "ForwardAgent=no", "-o", "ClearAllForwardings=yes", "duck@vulns.ts", "duckway ducklion list --json"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SSHArgs = %#v, want %#v", got, want)
	}
}
