package client

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// InstallCACert installs the Duckway CA cert to the system trust store.
func InstallCACert(configDir string) error {
	certPath := filepath.Join(configDir, "ca.pem")
	if _, err := os.Stat(certPath); err != nil {
		return fmt.Errorf("CA cert not found at %s", certPath)
	}

	switch runtime.GOOS {
	case "linux":
		return installCALinux(certPath)
	case "darwin":
		return installCAMacOS(certPath)
	default:
		return fmt.Errorf("automatic CA install not supported on %s — install manually", runtime.GOOS)
	}
}

func installCALinux(certPath string) error {
	// Strategy 1 (preferred): use the distro's CA update tool — properly
	// integrates with the system bundle so future updates don't lose us.
	// Detect by which update binary is in PATH — anchor dir may not exist yet.
	targets := []struct {
		dir    string
		update string
	}{
		{"/usr/local/share/ca-certificates", "update-ca-certificates"}, // Debian/Ubuntu/Alpine/Kali
		{"/etc/pki/ca-trust/source/anchors", "update-ca-trust"},        // RHEL/Fedora/CentOS
		{"/etc/ca-certificates/trust-source/anchors", "trust"},         // Arch (uses `trust extract-compat`)
	}

	for _, t := range targets {
		if _, err := exec.LookPath(t.update); err != nil {
			continue
		}
		if err := os.MkdirAll(t.dir, 0755); err != nil {
			return fmt.Errorf("create %s: %w (try with sudo)", t.dir, err)
		}
		dest := filepath.Join(t.dir, "duckway-ca.crt")
		data, _ := os.ReadFile(certPath)
		if err := os.WriteFile(dest, data, 0644); err != nil {
			return fmt.Errorf("copy cert to %s: %w (try with sudo)", dest, err)
		}
		var cmd *exec.Cmd
		if t.update == "trust" {
			cmd = exec.Command("trust", "extract-compat")
		} else {
			cmd = exec.Command(t.update)
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%s failed: %s", t.update, string(out))
		}
		return nil
	}

	// Strategy 2 (fallback): no update tool installed (minimal Alpine, busybox-
	// only systems, etc.). Try installing into well-known bundle locations
	// directly. Less ideal because system updates may overwrite, but it works
	// without the ca-certificates package.
	if err := installCAFallback(certPath); err == nil {
		return nil
	} else {
		return fmt.Errorf("no supported CA update tool found and fallback failed: %w "+
			"(try: apk add ca-certificates  /  apt install ca-certificates)", err)
	}
}

// installCAFallback writes the cert to /etc/ssl/* directly when no update tool
// is available. Tries each path in order; succeeds on the first one that works.
func installCAFallback(certPath string) error {
	data, err := os.ReadFile(certPath)
	if err != nil {
		return fmt.Errorf("read source cert: %w", err)
	}

	// 2a. /etc/ssl/certs/duckway-ca.pem — Alpine, minimal containers, busybox.
	//     Many TLS libs (LibreSSL, mbedTLS, busybox wget) read /etc/ssl/certs/.
	//     We add a hash symlink if openssl is available so c_rehash-style
	//     consumers find it; if not, the bare .pem still works for path lookups.
	dir := "/etc/ssl/certs"
	if _, err := os.Stat(dir); err == nil {
		dest := filepath.Join(dir, "duckway-ca.pem")
		if err := os.WriteFile(dest, data, 0644); err == nil {
			// Try to add the OpenSSL hash symlink so libs that look up by
			// subject hash can find the cert too. Best-effort.
			if hashOut, hashErr := exec.Command("openssl", "x509", "-hash", "-noout", "-in", dest).CombinedOutput(); hashErr == nil {
				hash := string(hashOut)
				if i := len(hash) - 1; i >= 0 && (hash[i] == '\n' || hash[i] == '\r') {
					hash = hash[:i]
				}
				symlink := filepath.Join(dir, hash+".0")
				_ = os.Remove(symlink)
				_ = os.Symlink("duckway-ca.pem", symlink)
			}
			return nil
		}
	}

	// 2b. /etc/ssl/cert.pem — single-bundle file used by LibreSSL on
	//     Alpine and OpenBSD. Append to it (idempotent — skip if our cert
	//     is already in there).
	bundle := "/etc/ssl/cert.pem"
	if existing, err := os.ReadFile(bundle); err == nil {
		// Idempotency: skip if the cert is already appended (compare by
		// the BEGIN CERTIFICATE block, which is stable).
		needle := []byte("# Duckway CA")
		if !bytesContains(existing, needle) {
			f, err := os.OpenFile(bundle, os.O_APPEND|os.O_WRONLY, 0644)
			if err != nil {
				return fmt.Errorf("open %s: %w", bundle, err)
			}
			defer f.Close()
			if _, err := f.WriteString("\n# Duckway CA\n"); err != nil {
				return err
			}
			if _, err := f.Write(data); err != nil {
				return err
			}
		}
		return nil
	}

	return fmt.Errorf("none of /etc/ssl/certs or /etc/ssl/cert.pem exist on this system")
}

func bytesContains(haystack, needle []byte) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if string(haystack[i:i+len(needle)]) == string(needle) {
			return true
		}
	}
	return false
}

func installCAMacOS(certPath string) error {
	cmd := exec.Command("security", "add-trusted-cert", "-d", "-r", "trustRoot",
		"-k", "/Library/Keychains/System.keychain", certPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("security add-trusted-cert failed: %s", string(out))
	}
	return nil
}
