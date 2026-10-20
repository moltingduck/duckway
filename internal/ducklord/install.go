package ducklord

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const defaultDucklionInstallPath = "~/.local/bin/ducklion"
const maxDucklionInstallSize = 128 << 20
const remoteDucklionInstallTimeout = 5 * time.Minute

const remoteDucklionInstallScript = `set -eu
dest="$1"
sha="$2"
case "$dest" in "~/"*) dest="$HOME/${dest#\~/}" ;; esac
dir=$(dirname "$dest")
tmp="${dest}.tmp.$$"
mkdir -p "$dir"
trap 'rm -f "$tmp"' EXIT HUP INT TERM
cat > "$tmp"
chmod 0755 "$tmp"
"$tmp" management install-standalone "$dest" "$sha"
if ! "$dest" daemon status | grep -q 'running=true'; then
  "$dest" daemon start
fi
"$dest" version
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

func (*Runner) InstallDucklion(ctx context.Context, c Client, source, dest string) (string, error) {
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
	return installDucklionReader(ctx, c, f, dest)
}

// InstallDucklionBytes installs a binary fetched and verified by the caller.
// It deliberately accepts bytes rather than a local path so default remote
// host setup cannot accidentally stream the controller's Ducklion binary.
func (*Runner) InstallDucklionBytes(ctx context.Context, c Client, binary []byte, dest string) (string, error) {
	return installDucklionReader(ctx, c, bytes.NewReader(binary), dest)
}

func installDucklionReader(ctx context.Context, c Client, f io.Reader, dest string) (string, error) {
	if strings.TrimSpace(dest) == "" {
		dest = defaultDucklionInstallPath
	}
	if !safeRemoteInstallPath(dest) {
		return "", fmt.Errorf("invalid remote install path %q", dest)
	}
	content, err := io.ReadAll(io.LimitReader(f, maxDucklionInstallSize+1))
	if err != nil {
		return "", err
	}
	if len(content) > maxDucklionInstallSize {
		return "", fmt.Errorf("ducklion binary exceeds maximum size of %d bytes", maxDucklionInstallSize)
	}
	hash := sha256.Sum256(content)
	wantSHA := hex.EncodeToString(hash[:])
	args := SSHArgs(c, false, "sh", "-lc", remoteDucklionInstallScript, "ducklord-install-ducklion", dest, wantSHA)
	sshParts := c.SSHCommandParts()
	installCtx := ctx
	var cancel context.CancelFunc
	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline || time.Until(deadline) > remoteDucklionInstallTimeout {
		installCtx, cancel = context.WithTimeout(ctx, remoteDucklionInstallTimeout)
		defer cancel()
	}
	cmd := exec.CommandContext(installCtx, sshParts[0], append(sshParts[1:], args...)...)
	cmd.Stdin = bytes.NewReader(content)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if installCtx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("ssh install to %s timed out: %w", c.Name, installCtx.Err())
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
