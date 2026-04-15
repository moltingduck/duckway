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
	// Try different distro paths
	targets := []struct {
		dir    string
		update string
	}{
		{"/usr/local/share/ca-certificates", "update-ca-certificates"},
		{"/etc/pki/ca-trust/source/anchors", "update-ca-trust"},
	}

	for _, t := range targets {
		if _, err := os.Stat(t.dir); err == nil {
			dest := filepath.Join(t.dir, "duckway-ca.crt")
			data, _ := os.ReadFile(certPath)
			if err := os.WriteFile(dest, data, 0644); err != nil {
				return fmt.Errorf("copy cert: %w (try with sudo)", err)
			}
			cmd := exec.Command(t.update)
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("%s failed: %s", t.update, string(out))
			}
			return nil
		}
	}
	return fmt.Errorf("could not find system CA directory")
}

func installCAMacOS(certPath string) error {
	cmd := exec.Command("security", "add-trusted-cert", "-d", "-r", "trustRoot",
		"-k", "/Library/Keychains/System.keychain", certPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("security add-trusted-cert failed: %s", string(out))
	}
	return nil
}
