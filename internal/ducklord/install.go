package ducklord

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const defaultDucklionInstallPath = "~/.local/bin/ducklion"

const remoteDucklionInstallScript = `set -eu
dest="$1"
case "$dest" in "~/"*) dest="$HOME/${dest#\~/}" ;; esac
dir=$(dirname "$dest")
tmp="${dest}.tmp.$$"
mkdir -p "$dir"
cat > "$tmp"
chmod 0755 "$tmp"
mv "$tmp" "$dest"
"$dest" version
printf 'DUCKLION_INSTALLED\t%s\n' "$dest"
`

func FindLocalDucklion(source string) (string, error) {
	source = strings.TrimSpace(source)
	if source != "" {
		return executableFile(source)
	}
	if path, err := exec.LookPath("ducklion"); err == nil {
		return path, nil
	}
	exe, err := os.Executable()
	if err == nil {
		if path, err := executableFile(filepath.Join(filepath.Dir(exe), "ducklion")); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("local ducklion binary not found; pass --source <path> or install ducklion beside ducklord")
}

func executableFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.IsDir() || info.Mode().Perm()&0111 == 0 {
		return "", fmt.Errorf("%s is not an executable file", path)
	}
	return path, nil
}

func (Runner) InstallDucklion(ctx context.Context, c Client, source, dest string) (string, error) {
	src, err := FindLocalDucklion(source)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(dest) == "" {
		dest = defaultDucklionInstallPath
	}
	if !safeRemoteInstallPath(dest) {
		return "", fmt.Errorf("invalid remote install path %q", dest)
	}
	f, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer f.Close()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	args := SSHArgs(c, false, "sh", "-lc", remoteDucklionInstallScript, "ducklord-install-ducklion", dest)
	cmd := exec.CommandContext(ctx, c.SSH, args...)
	cmd.Stdin = f
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("ssh install to %s timed out", c.Name)
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("install ducklion on %s: %s", c.Name, msg)
	}
	outText := string(out)
	if !strings.Contains(outText, "ducklion") {
		return "", fmt.Errorf("remote ducklion version check returned unexpected output: %.200s", out)
	}
	for _, line := range strings.Split(outText, "\n") {
		installed, ok := strings.CutPrefix(line, "DUCKLION_INSTALLED\t")
		if ok && strings.TrimSpace(installed) != "" {
			return strings.TrimSpace(installed), nil
		}
	}
	return "", fmt.Errorf("remote ducklion install did not report installed path")
}

func safeRemoteInstallPath(path string) bool {
	if path == "" || len(path) > 255 || strings.HasPrefix(path, "-") {
		return false
	}
	for _, r := range path {
		if r <= 0x20 || r == '\'' || r == '"' || r == '`' || r == '$' || r == ';' || r == '&' || r == '|' || r == '<' || r == '>' || r == '(' || r == ')' {
			return false
		}
	}
	return strings.HasPrefix(path, "~/") || strings.HasPrefix(path, "/")
}
