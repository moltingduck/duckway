package ducklord

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadConfigNormalizesClients(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"clients":[{"name":"vulns","host":"vulns.ts","user":"duck","group":"ctf"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Clients[0]
	if got.Ducklion != "ducklion" || got.SSH != "ssh" || got.Target() != "duck@vulns.ts" {
		t.Fatalf("client = %+v", got)
	}
}

func TestLoadConfigRejectsUnsafeHost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"clients":[{"name":"bad","host":"host;touch /tmp/pwn"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("unsafe host accepted")
	}
}

func TestLoadConfigRejectsHostOptionInjection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"clients":[{"name":"bad","host":"-oProxyCommand=/tmp/pwn"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("ssh option host accepted")
	}
}

func TestLoadConfigAcceptsDuckwayDucklionSubcommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"clients":[{"name":"vulns","host":"vulns.ts","ducklion":"duckway ducklion"}]}`), 0600); err != nil {
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
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"clients":[{"name":"vulns","host":"vulns.ts","ssh":"ssh -p 2222 -i /tmp/id_ed25519"}]}`), 0600); err != nil {
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
