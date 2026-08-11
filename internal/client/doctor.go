package client

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/hackerduck/duckway/internal/version"
)

type DoctorLine struct {
	Name    string
	Status  string
	Detail  string
	Missing string
}

type DoctorReport struct {
	ConfigDir string
	Version   string
	Client    string
	ServerURL string
	Lines     []DoctorLine
}

func RunDoctor(configDir string) DoctorReport {
	return RunDoctorWithConfig(configDir, nil)
}

func RunDoctorWithConfig(configDir string, cfg *Config) DoctorReport {
	report := DoctorReport{ConfigDir: configDir, Version: version.Get()}
	if cfg == nil {
		loaded, err := LoadConfig(configDir)
		if err != nil {
			report.add("config", "MISSING", "config.yaml not loaded", "run `duckway init`")
			report.add("proxy daemon", "UNKNOWN", "requires client config", "run `duckway init`")
			report.add("cc watch", "UNKNOWN", "requires client config", "run `duckway init`")
			report.add("server", "UNKNOWN", "requires client config", "run `duckway init`")
			report.addLocalOnlyChecks(configDir, nil)
			return report
		}
		cfg = loaded
	}
	report.Client = cfg.ClientName
	report.ServerURL = cfg.ServerURL
	report.add("config", "OK", fmt.Sprintf("client=%s server=%s", emptyDash(cfg.ClientName), emptyDash(cfg.ServerURL)), "")

	api := NewAPIClient(cfg.ServerURL, cfg.Token)
	if err := api.Ping(); err != nil {
		report.add("server auth", "ERROR", err.Error(), "check server URL/token or re-run `duckway init`")
	} else {
		report.add("server auth", "OK", "client token accepted", "")
	}
	if info, err := CheckUpdateInfo(cfg.ServerURL, version.Get()); err != nil {
		report.add("updates", "WARN", err.Error(), "server may be old or unreachable")
	} else if info.UpdateRequired {
		report.add("updates", "MISSING", "client update required: "+info.ClientRecommendedVersion, "run `duckway update --restart`")
	} else if info.UpdateRecommended {
		report.add("updates", "WARN", "client update available: "+info.ClientRecommendedVersion, "run `duckway update --restart`")
	} else {
		report.add("updates", "OK", "up to date", "")
	}
	if ccs, err := api.FetchCC(); err != nil {
		report.add("cc assignment", "WARN", err.Error(), "run `duckway sync` after server connectivity is fixed")
	} else if len(ccs) == 0 {
		report.add("cc assignment", "MISSING", "no CC assigned to this client", "assign a control channel on the server")
	} else {
		cc := ccs[0]
		report.add("cc assignment", "OK", fmt.Sprintf("%s (%s)", emptyDash(cc.CCName), emptyDash(cc.AgentType)), "")
	}
	report.addDaemonChecks(configDir, cfg)
	report.addLocalOnlyChecks(configDir, cfg)
	return report
}

func (r *DoctorReport) addDaemonChecks(configDir string, cfg *Config) {
	if pid, alive := doctorReadPID(filepath.Join(configDir, "proxy.pid")); alive {
		r.add("proxy daemon", "OK", fmt.Sprintf("running pid=%d", pid), "")
	} else {
		r.add("proxy daemon", "MISSING", "not running", "run `duckway proxy -d` or `duckway start`")
	}
	proxyURL := LocalProxyURL(cfg.ProxyPort)
	localHTTP := &http.Client{Timeout: 2 * time.Second, Transport: directTransport}
	resp, err := localHTTP.Get(proxyURL + "/proxy/heartbeat/ping")
	if err == nil {
		_ = resp.Body.Close()
		r.add("local proxy port", "OK", proxyURL+" responds", "")
	} else {
		r.add("local proxy port", "MISSING", proxyURL+" not reachable", "start the proxy daemon")
	}
	if state, err := LoadCCState(configDir); err != nil {
		r.add("cc sync state", "WARN", err.Error(), "run `duckway sync`")
	} else if len(state.CCs) == 0 {
		r.add("cc sync state", "MISSING", "no synced CC assignment", "run `duckway sync` after assigning CC")
	} else {
		r.add("cc sync state", "OK", fmt.Sprintf("%d assignment(s)", len(state.CCs)), "")
	}
	if pid, alive := doctorReadPID(filepath.Join(configDir, "cc-watch.pid")); alive {
		r.add("cc watch", "OK", fmt.Sprintf("running pid=%d", pid), "")
	} else {
		r.add("cc watch", "MISSING", "not running", "run `duckway cc watch -d` or `duckway start`")
	}
}

func (r *DoctorReport) addLocalOnlyChecks(configDir string, cfg *Config) {
	r.addDucklionCheck()
	agents := availableAgents()
	if len(agents) == 0 {
		r.add("agent binaries", "MISSING", "claude/codex not found in PATH", "install Claude Code or Codex CLI")
	} else {
		r.add("agent binaries", "OK", strings.Join(agents, ", "), "")
	}
	if projects, err := NewCCProjectStore(configDir).List(); err != nil {
		r.add("projects", "WARN", err.Error(), "fix cc-projects.json or re-add projects")
	} else if len(projects) == 0 {
		r.add("projects", "MISSING", "no saved projects", "run `duckway projects add <path>`")
	} else {
		r.add("projects", "OK", fmt.Sprintf("%d saved project(s)", len(projects)), "")
	}
	r.addCACheck(configDir)
	if cfg == nil {
		return
	}
	if cfg.ProxyPort <= 0 {
		r.add("proxy config", "WARN", "invalid proxy port", "re-run `duckway init`")
	} else {
		r.add("proxy config", "OK", fmt.Sprintf("port=%d", cfg.ProxyPort), "")
	}
}

func (r *DoctorReport) addDucklionCheck() {
	if path, err := exec.LookPath("ducklion"); err == nil {
		r.add("ducklion", "OK", path, "")
		return
	}
	exe, err := os.Executable()
	if err == nil {
		sibling := filepath.Join(filepath.Dir(exe), "ducklion")
		if info, statErr := os.Stat(sibling); statErr == nil && !info.IsDir() && info.Mode().Perm()&0111 != 0 {
			r.add("ducklion", "OK", sibling+" (beside duckway)", "")
			return
		}
	}
	r.add("ducklion", "MISSING", "not in PATH or beside duckway", "update Duckway or install standalone ducklion")
}

func (r *DoctorReport) addCACheck(configDir string) {
	data, err := os.ReadFile(filepath.Join(configDir, "ca.pem"))
	if err != nil {
		r.add("CA cert", "MISSING", "ca.pem not found", "run `duckway init`")
		return
	}
	block, _ := pem.Decode(data)
	if block == nil {
		r.add("CA cert", "WARN", "could not parse PEM", "re-run `duckway init`")
		return
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		r.add("CA cert", "WARN", err.Error(), "re-run `duckway init`")
		return
	}
	now := time.Now()
	if now.After(cert.NotAfter) {
		r.add("CA cert", "MISSING", "expired "+cert.NotAfter.Format("2006-01-02"), "re-run `duckway init`")
		return
	}
	days := int(cert.NotAfter.Sub(now) / (24 * time.Hour))
	if days < 30 {
		r.add("CA cert", "WARN", fmt.Sprintf("expires in %d days", days), "re-run `duckway init` soon")
		return
	}
	r.add("CA cert", "OK", fmt.Sprintf("expires in %d days", days), "")
}

func (r *DoctorReport) add(name, status, detail, missing string) {
	r.Lines = append(r.Lines, DoctorLine{Name: name, Status: status, Detail: detail, Missing: missing})
}

func (r DoctorReport) FormatText() string {
	var b strings.Builder
	b.WriteString("Duckway doctor\n")
	b.WriteString("Version: " + r.Version + "\n")
	b.WriteString("Config: " + r.ConfigDir + "\n")
	if r.Client != "" {
		b.WriteString("Client: " + r.Client + "\n")
	}
	if r.ServerURL != "" {
		b.WriteString("Server: " + r.ServerURL + "\n")
	}
	b.WriteString("\n")
	for _, line := range r.Lines {
		fmt.Fprintf(&b, "[%s] %s: %s", line.Status, line.Name, line.Detail)
		if line.Missing != "" {
			b.WriteString(" | fix: " + line.Missing)
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

func doctorReadPID(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid); err != nil || pid <= 0 {
		return pid, false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return pid, false
	}
	return pid, true
}

func availableAgents() []string {
	var out []string
	for _, bin := range []string{"claude", "codex"} {
		if path, err := exec.LookPath(bin); err == nil {
			out = append(out, bin+"="+path)
		}
	}
	return out
}

func emptyDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
