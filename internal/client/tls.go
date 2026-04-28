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
	// (anchor dir, update command) for known distros.
	// Detect by which update binary is in PATH — directory may not exist yet.
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
		// Binary exists — ensure the anchor dir is present, then copy.
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
	return fmt.Errorf("no supported CA update tool found (install 'ca-certificates' package)")
}

func installCAMacOS(certPath string) error {
	cmd := exec.Command("security", "add-trusted-cert", "-d", "-r", "trustRoot",
		"-k", "/Library/Keychains/System.keychain", certPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("security add-trusted-cert failed: %s", string(out))
	}
	return nil
}
