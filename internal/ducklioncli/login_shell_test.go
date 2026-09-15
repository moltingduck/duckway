package ducklioncli

import (
	"os"
	"reflect"
	"testing"
)

func TestHostLoginShellUsesRemoteAccountDatabase(t *testing.T) {
	for _, tc := range []struct {
		name, goos, database, passwd, environment, want string
	}{
		{"NSS overrides environment and passwd", "linux", "remote:x:123:123::/home/remote:/usr/bin/fish\n", "remote:x:123:123::/home/remote:/bin/bash\n", "/bin/zsh", "/usr/bin/fish"},
		{"local account fallback", "linux", "", "other:x:12:12::/:/bin/false\nremote:x:123:123::/home/remote:/bin/zsh\n", "/bin/bash", "/bin/zsh"},
		{"empty account shell", "linux", "remote:x:123:123::/home/remote:\n", "", "/bin/zsh", "/bin/sh"},
		{"mac directory service", "darwin", "UserShell: /bin/zsh\n", "", "/bin/bash", "/bin/zsh"},
		{"unavailable account database", "linux", "", "", "/bin/fish", "/bin/fish"},
		{"missing environment", "linux", "", "", "", "/bin/sh"},
		{"relative environment rejected", "linux", "", "", "fish", "/bin/sh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveHostLoginShell("123", tc.goos, tc.environment, func(path string) ([]byte, error) {
				if path != "/etc/passwd" {
					t.Fatalf("unexpected file %q", path)
				}
				return []byte(tc.passwd), nil
			}, func(path string, args ...string) ([]byte, error) {
				if tc.goos == "darwin" {
					if path != "/usr/bin/dscl" {
						return nil, os.ErrNotExist
					}
					if !reflect.DeepEqual(args, []string{".", "-read", "/Users/remote", "UserShell"}) {
						t.Fatalf("dscl args: %q", args)
					}
				} else if !reflect.DeepEqual(args, []string{"passwd", "123"}) {
					t.Fatalf("getent args: %q", args)
				}
				return []byte(tc.database), nil
			}, func() (string, error) { return "remote", nil })
			if got != tc.want {
				t.Fatalf("shell=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestPasswdLoginShellMatchesExactUIDAndRejectsMalformedRecords(t *testing.T) {
	for _, data := range []string{"remote:x:1234:123::/:/bin/zsh", "remote:x:123", "remote:x:123:123::/:zsh"} {
		if shell, ok := passwdLoginShell(data, "123"); ok {
			t.Fatalf("accepted %q as %q", data, shell)
		}
	}
}
