package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

var errDoctorUnsafe = errors.New("unsafe file")
var errDoctorLarge = errors.New("oversized file")
var errDoctorJSON = errors.New("malformed JSON object")

func doctorFileFailure(err error) string {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "file not found"
	case errors.Is(err, os.ErrPermission):
		return "permission denied"
	case errors.Is(err, syscall.ELOOP), errors.Is(err, errDoctorUnsafe):
		return "unsafe source: symlink or nonregular file"
	case errors.Is(err, errDoctorLarge):
		return "file exceeds 1 MiB limit"
	case errors.Is(err, errDoctorJSON):
		return "malformed JSON object"
	default:
		return "file could not be read"
	}
}

// Read only bounded regular files; never execute helpers or open pipes/devices.
func doctorFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errDoctorUnsafe
	}
	if st.Size() > 1<<20 {
		return nil, errDoctorLarge
	}
	b, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if len(b) > 1<<20 {
		return nil, errDoctorLarge
	}
	return b, err
}
func doctorJSON(path string) (map[string]interface{}, error) {
	b, err := doctorFile(path)
	if err != nil {
		return nil, err
	}
	var v map[string]interface{}
	err = json.Unmarshal(b, &v)
	if v == nil && err == nil {
		err = errDoctorJSON
	}
	if err != nil {
		return nil, errDoctorJSON
	}
	return v, nil
}
func doctorString(m map[string]interface{}, key string) string { s, _ := m[key].(string); return s }
func doctorObject(m map[string]interface{}, key string) map[string]interface{} {
	v, _ := m[key].(map[string]interface{})
	return v
}
func doctorPath(p string) string {
	if strings.Contains(p, "://") || strings.ContainsAny(p, "?\r\n") {
		return "[path hidden: unsafe display characters]"
	}
	return fmt.Sprintf("%q", p)
}
func (r *DoctorReport) addCredentialChecks(configDir string, cfg *Config) {
	keys := map[string]string{}
	if data, err := doctorFile(filepath.Join(configDir, "keys.env")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			k, v, ok := strings.Cut(line, "=")
			if ok {
				keys[strings.TrimSpace(strings.TrimPrefix(k, "export "))] = strings.Trim(strings.TrimSpace(v), "\"'")
			}
		}
	}
	codexPath, _ := codexAuthJSONPath()
	source := "default ~/.codex"
	if os.Getenv("CODEX_HOME") != "" {
		source = "CODEX_HOME override"
	}
	r.add("Codex credential source", "INFO", source+"; selected file "+doctorPath(codexPath), "use the same CODEX_HOME for sync and launch; config/keyring/CLI overrides may select another source")
	if configPath, e := codexConfigTOMLPath(); e == nil {
		if b, e := doctorFile(configPath); e == nil {
			store := "not explicitly selected"
			for _, stmt := range tomlStatements(string(b)) {
				if stmt.header != "" {
					break
				}
				if stmt.isKey("cli_auth_credentials_store") {
					if stmt.stringIs("file") {
						store = "file"
					} else {
						store = "non-file store (file may be ignored)"
					}
				}
			}
			r.add("Codex credential store", "INFO", store, "Duckway native OAuth expects file storage; inspect config/profile/CLI overrides locally")
		}
	}
	auth, err := doctorJSON(codexPath)
	if err != nil {
		r.add("Codex auth file", "MISSING", doctorFileFailure(err), "run duckway sync with the intended CODEX_HOME")
	} else {
		tokens := doctorObject(auth, "tokens")
		token := doctorString(tokens, "access_token")
		if token == "" {
			token = doctorString(auth, "OPENAI_API_KEY")
		}
		r.addTokenCheck("Codex configured credential", token, keys["OPENAI_API_KEY"])
		if doctorString(tokens, "access_token") != "" && (doctorString(tokens, "id_token") == "" || doctorString(tokens, "refresh_token") == "") {
			r.add("Codex OAuth fields", "WARN", "OAuth token set is incomplete", "sync the full assigned Codex OAuth credentials")
		}
	}
	home, _ := os.UserHomeDir()
	claudeDir := os.Getenv("CLAUDE_CONFIG_DIR")
	if claudeDir == "" {
		claudeDir = filepath.Join(home, ".claude")
	}
	claudePath := filepath.Join(claudeDir, ".credentials.json")
	r.add("Claude credential source", "INFO", "selected file candidate "+doctorPath(claudePath)+"; native keychain/helper/CLI settings may take precedence", "Duckway sync writes ~/.claude/.credentials.json; align CLAUDE_CONFIG_DIR manually when overridden")
	credential, err := doctorJSON(claudePath)
	if err != nil {
		r.add("Claude credential file", "MISSING", doctorFileFailure(err), "check CLAUDE_CONFIG_DIR and run duckway sync")
	} else {
		r.addTokenCheck("Claude configured credential", doctorString(doctorObject(credential, "claudeAiOauth"), "accessToken"), keys["ANTHROPIC_API_KEY"])
	}
	for _, name := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR", "OPENAI_BASE_URL", "ANTHROPIC_BASE_URL", "CHATGPT_BASE_URL", "CODEX_EXEC_SERVER_URL"} {
		if os.Getenv(name) != "" {
			r.add("environment "+name, "WARN", "set (value hidden); may override credential or routing selection", "compare the agent's selected source; proxy exec applies synced key overrides")
		}
	}
	for _, path := range []string{filepath.Join(claudeDir, "settings.json"), filepath.Join(home, ".claude.json")} {
		if m, e := doctorJSON(path); e == nil {
			if _, ok := m["apiKeyHelper"]; ok {
				r.add("Claude apiKeyHelper", "WARN", "configured in "+doctorPath(path)+"; not executed", "inspect helper and project/managed settings locally; helper credentials may override files")
			}
		}
	}
	r.addProxyEnvironment(configDir, cfg)
}
func (r *DoctorReport) addTokenCheck(name, token, expected string) {
	if token == "" {
		r.add(name, "MISSING", "no usable string token in inspected file", "sync the assigned credentials to this source")
		return
	}
	if !tokenUsesDuckwayPhantom(token) {
		r.add(name, "WARN", "configured token is not recognized as a Duckway phantom", "check credential source and sync; this is a file observation only")
		return
	}
	if expected != "" && token == expected {
		r.add(name, "OK", "configured phantom matches synced key; no request observed by this check", "")
		return
	}
	r.add(name, "INFO", "configured phantom marker present; exact synced-key match not established", "OAuth-shaped phantoms can differ from keys.env; reproduce a request and inspect observed counts")
}
func (r *DoctorReport) addProxyEnvironment(configDir string, cfg *Config) {
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		v := os.Getenv(name)
		status := "MISSING"
		detail := "unset in doctor environment"
		if v != "" {
			status = "WARN"
			detail = "set to a different proxy (value hidden)"
			if cfg != nil && v == LocalProxyURL(cfg.ProxyPort) {
				status = "OK"
				detail = "matches configured local proxy"
			}
		}
		r.add(name, status, detail, "launch with duckway proxy exec -- CMD to prepare the child environment")
	}
	for _, name := range []string{"NO_PROXY", "no_proxy"} {
		if v := os.Getenv(name); v != "" {
			r.add(name, "WARN", "set (value hidden); matching provider hosts bypass the proxy", "check for wildcard/provider exclusions; proxy exec sets loopback-only exclusions")
		}
	}
	for _, name := range []string{"CODEX_CA_CERTIFICATE", "SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE", "NODE_EXTRA_CA_CERTS"} {
		if v := os.Getenv(name); v != "" {
			status := "WARN"
			if v == filepath.Join(configDir, "ca.pem") {
				status = "OK"
			}
			r.add(name, status, "set; expected Duckway ca.pem path compared without printing value", "launch through proxy exec to prepare CA environment")
		}
	}
	if cfg == nil {
		return
	}
	r.addObservedRequests(cfg)
}
func (r *DoctorReport) addObservedRequests(cfg *Config) {
	client := &http.Client{Timeout: 2 * time.Second, Transport: directTransport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequest(http.MethodGet, LocalProxyURL(cfg.ProxyPort)+"/proxy/doctor", nil)
	if err != nil {
		return
	}
	req.Header.Set("X-Duckway-Token", cfg.Token)
	resp, err := client.Do(req)
	if err != nil {
		r.add("observed requests", "UNKNOWN", "local proxy diagnostics unavailable", "start the current proxy, reproduce the agent request, then rerun doctor")
		return
	}
	defer resp.Body.Close()
	var snap struct {
		Schema string         `json:"schema"`
		Counts []doctorCounts `json:"counts"`
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, 8193))
	if e != nil || len(b) > 8192 || resp.StatusCode != http.StatusOK || json.Unmarshal(b, &snap) != nil || snap.Schema != "duckway-proxy-doctor-v1" || len(snap.Counts) != len(doctorServices) || resp.Header.Get("Content-Type") != "application/json" {
		r.add("observed requests", "UNKNOWN", "local proxy diagnostics unsupported or rejected", "restart the proxy with the current Duckway version; no request observation inferred")
		return
	}
	for i, s := range doctorServices {
		c := snap.Counts[i]
		status := "INFO"
		detail := fmt.Sprintf("since proxy startup: phantom=%d missing-auth-headers=%d nonphantom-auth-headers=%d", c.Phantom, c.Missing, c.Other)
		if c.Phantom+c.Missing+c.Other == 0 {
			status = "UNKNOWN"
			detail = "no request observed since proxy startup"
		}
		r.add("observed "+s, status, detail, "reproduce the request then rerun doctor; public requests without auth headers can be normal; counts do not prove auth success or identify a specific agent")
	}
}
