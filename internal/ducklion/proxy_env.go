package ducklion

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/hackerduck/duckway/internal/duckwayconfig"
	"gopkg.in/yaml.v3"
)

type duckwayProxyConfig struct {
	ProxyPort int `yaml:"proxy_port"`
}

func duckwayAgentProxyEnv(agentType string) []string {
	if agentType == "shell" {
		return nil
	}
	configDir := duckwayconfig.DefaultConfigDir()
	port, ok := duckwayProxyPort(configDir)
	if !ok || !duckwayProxyAlive(configDir) {
		return nil
	}
	proxyURL := "http://127.0.0.1:" + strconv.Itoa(port)
	noProxy := "localhost,127.0.0.1"
	if existing := strings.TrimSpace(os.Getenv("NO_PROXY")); existing != "" {
		noProxy = ensureLoopbackNoProxy(existing)
	}
	env := []string{
		"HTTP_PROXY=" + proxyURL,
		"HTTPS_PROXY=" + proxyURL,
		"http_proxy=" + proxyURL,
		"https_proxy=" + proxyURL,
		"NO_PROXY=" + noProxy,
		"no_proxy=" + noProxy,
	}
	if ca := duckwayAgentCAEnv(configDir); len(ca) > 0 {
		env = append(env, ca...)
	}
	return env
}

func duckwayProxyPort(configDir string) (int, bool) {
	data, err := os.ReadFile(filepath.Join(configDir, "config.yaml"))
	if err != nil {
		return 0, false
	}
	var cfg duckwayProxyConfig
	if yaml.Unmarshal(data, &cfg) != nil {
		return 0, false
	}
	if cfg.ProxyPort > 0 {
		return cfg.ProxyPort, true
	}
	return 18080, true
}

func duckwayProxyAlive(configDir string) bool {
	data, err := os.ReadFile(filepath.Join(configDir, "proxy.pid"))
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

func duckwayAgentCAEnv(configDir string) []string {
	caPath := filepath.Join(configDir, "agent-ca-bundle.pem")
	if validPEMFile(caPath) {
		return caEnv(caPath)
	}
	caPath = filepath.Join(configDir, "ca.pem")
	if validPEMFile(caPath) {
		return caEnv(caPath)
	}
	return nil
}

func caEnv(path string) []string {
	return []string{
		"SSL_CERT_FILE=" + path,
		"REQUESTS_CA_BUNDLE=" + path,
		"CURL_CA_BUNDLE=" + path,
		"NODE_EXTRA_CA_CERTS=" + path,
	}
}

func validPEMFile(path string) bool {
	data, err := os.ReadFile(path)
	return err == nil && len(bytes.TrimSpace(data)) > 0
}

func ensureLoopbackNoProxy(existing string) string {
	parts := strings.Split(existing, ",")
	hasLocalhost := false
	hasLoopback := false
	for _, part := range parts {
		switch strings.TrimSpace(part) {
		case "localhost":
			hasLocalhost = true
		case "127.0.0.1":
			hasLoopback = true
		}
	}
	if !hasLocalhost {
		parts = append(parts, "localhost")
	}
	if !hasLoopback {
		parts = append(parts, "127.0.0.1")
	}
	return strings.Join(parts, ",")
}

func mergeEnv(base, defaults, explicit []string) []string {
	out := append([]string{}, base...)
	for _, item := range append(defaults, explicit...) {
		if key, ok := envKey(item); ok {
			out = removeEnvKey(out, key)
		}
		out = append(out, item)
	}
	return out
}

func envKey(item string) (string, bool) {
	i := strings.IndexByte(item, '=')
	if i <= 0 {
		return "", false
	}
	return item[:i], true
}

func removeEnvKey(env []string, key string) []string {
	out := env[:0]
	prefix := key + "="
	for _, item := range env {
		if !strings.HasPrefix(item, prefix) {
			out = append(out, item)
		}
	}
	return out
}
