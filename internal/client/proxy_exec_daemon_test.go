package client

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCodexDaemonOperation(t *testing.T) {
	cases := []struct {
		name                string
		args                []string
		needed, mutate, bad bool
	}{
		{"default", []string{"codex"}, true, false, false},
		{"explicit foreground", []string{"codex", "--no-daemon"}, false, false, false},
		{"exec", []string{"codex", "exec", "remote-control"}, false, false, false},
		{"login", []string{"codex", "login"}, false, false, false},
		{"prompt", []string{"codex", "--", "--remote=x"}, true, false, false},
		{"option value", []string{"codex", "--model", "--help"}, true, false, false},
		{"profile", []string{"codex", "-pwork"}, false, false, false},
		{"embedded config", []string{"codex", "-cmodel=\"x\""}, false, false, false},
		{"legacy feature alias", []string{"codex", "-c", "use_legacy_landlock=true"}, true, true, false},
		{"quoted key remains embedded", []string{"codex", "-c", "\"features\".example=true"}, false, false, false},
		{"feature config", []string{"codex", "-c", "features.example=true"}, true, true, false},
		{"feature table", []string{"codex", "--config=features={example=true}"}, true, true, false},
		{"feature", []string{"codex", "--enable", "example"}, true, true, false},
		{"remote bypass", []string{"codex", "--no-daemon", "--remote", "ws://example"}, false, false, true},
		{"session bypass", []string{"codex", "-pwork", "--remote-session=abc"}, false, false, true},
		{"rc foreground", []string{"codex", "remote-control", "--json"}, false, false, false},
		{"rc start", []string{"codex", "remote-control", "start", "--json"}, true, true, false},
		{"rc pair", []string{"codex", "remote-control", "pair"}, true, false, false},
		{"daemon restart", []string{"codex", "app-server", "daemon", "restart"}, true, true, false},
		{"stdio", []string{"codex", "app-server", "--listen", "stdio://"}, false, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			op, err := codexDaemonOperation(c.args)
			if (err != nil) != c.bad || op.needed != c.needed || op.mutate != c.mutate {
				t.Fatalf("operation=%+v error=%v", op, err)
			}
		})
	}
}

func TestCodexDaemonFingerprintTrustAndCredentials(t *testing.T) {
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, []byte("first CA"), 0600); err != nil {
		t.Fatal(err)
	}
	env := []string{"HTTPS_PROXY=http://fixture", "SSL_CERT_FILE=" + ca, "OPENAI_API_KEY=fixture"}
	first, err := daemonEnvFingerprint(env)
	if err != nil {
		t.Fatal(err)
	}
	reordered := []string{env[2], env[0], env[1]}
	same, err := daemonEnvFingerprint(reordered)
	if err != nil || same != first {
		t.Fatal("environment order changed fingerprint")
	}
	if err := os.WriteFile(ca, []byte("second CA"), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := daemonEnvFingerprint(env)
	if err != nil || second == first {
		t.Fatal("in-place CA replacement was not detected")
	}
	changed := setDaemonEnv(env, "OPENAI_API_KEY", "other fixture")
	third, err := daemonEnvFingerprint(changed)
	if err != nil || third == second {
		t.Fatal("credential replacement was not detected")
	}
}
