package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

// directTransport is an HTTP transport that never reads HTTPS_PROXY / HTTP_PROXY
// from the environment. All duckway-to-server requests use it so the proxy can
// start and sync without trying to route traffic through itself.
var directTransport = &http.Transport{
	// Proxy: nil → proxyForRequest returns (nil, nil) → no proxy used.
	// Explicit nil beats the DefaultTransport default of ProxyFromEnvironment.
	Proxy: nil,
	DialContext: (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext,
	TLSHandshakeTimeout: 10 * time.Second,
	IdleConnTimeout:     90 * time.Second,
}

// directClient is the shared HTTP client for all duckway-internal requests.
var directClient = &http.Client{Transport: directTransport}

// APIClient talks to the Duckway server.
type APIClient struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

func NewAPIClient(baseURL, token string) *APIClient {
	return &APIClient{
		baseURL:    baseURL,
		token:      token,
		httpClient: directClient,
	}
}

type PlaceholderKeyInfo struct {
	EnvName     string `json:"env_name"`
	Placeholder string `json:"placeholder"`
	ServiceName string `json:"service_name"`
	KeyPath     string `json:"key_path,omitempty"`
}

func (c *APIClient) FetchKeys() ([]PlaceholderKeyInfo, error) {
	req, err := http.NewRequest("GET", c.baseURL+"/client/keys", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Duckway-Token", c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch keys: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("server returned %d: %s", resp.StatusCode, string(body))
	}

	var keys []PlaceholderKeyInfo
	if err := json.NewDecoder(resp.Body).Decode(&keys); err != nil {
		return nil, fmt.Errorf("decode keys: %w", err)
	}
	return keys, nil
}

// FetchStatusline returns the admin-configured statusline script body
// or an empty string when nothing is set on the server. Errors only
// for transport failures or non-200 responses — an empty body is a
// valid "no statusline" signal and surfaces as ("", nil).
func (c *APIClient) FetchStatusline() (string, error) {
	req, err := http.NewRequest("GET", c.baseURL+"/client/statusline", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Duckway-Token", c.token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch statusline: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("server returned %d: %s", resp.StatusCode, string(body))
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// FetchClaudeCredentials gets phantom Claude OAuth credentials.
func (c *APIClient) FetchClaudeCredentials() (map[string]interface{}, error) {
	req, err := http.NewRequest("GET", c.baseURL+"/client/claude-credentials", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Duckway-Token", c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil // endpoint may not exist on older servers
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, nil
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, nil
	}
	// Only return if it has actual OAuth data
	if _, ok := result["claudeAiOauth"]; !ok {
		return nil, nil
	}
	return result, nil
}

// FetchSupplyChainRC returns the package-manager rc-file hardening settings to
// apply, keyed by rc file path relative to $HOME (e.g. ".npmrc"). Returns nil
// (no-op) when the server is older and lacks the endpoint.
func (c *APIClient) FetchSupplyChainRC() (map[string][]string, error) {
	req, err := http.NewRequest("GET", c.baseURL+"/client/supply-chain-rc", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Duckway-Token", c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil // older server — skip silently
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, nil
	}
	var result map[string][]string
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, nil
	}
	return result, nil
}

type CanaryDeploy struct {
	TokenType     string `json:"token_type"`
	DeployPath    string `json:"deploy_path"`
	DeployMode    string `json:"deploy_mode"` // "create" or "append"
	DeployContent string `json:"deploy_content"`
}

func (c *APIClient) FetchCanaries() ([]CanaryDeploy, error) {
	req, err := http.NewRequest("GET", c.baseURL+"/client/canaries", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Duckway-Token", c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch canaries: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, nil // canaries endpoint may not exist on older servers
	}

	var canaries []CanaryDeploy
	if err := json.NewDecoder(resp.Body).Decode(&canaries); err != nil {
		return nil, fmt.Errorf("decode canaries: %w", err)
	}
	return canaries, nil
}

// Heartbeat tests the proxy path by calling /proxy/heartbeat/ping
func (c *APIClient) Heartbeat() error {
	req, err := http.NewRequest("GET", c.baseURL+"/proxy/heartbeat/ping", nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Duckway-Token", c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("proxy unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("heartbeat returned %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// DownloadCA downloads the CA cert and key from the server.
func (c *APIClient) DownloadCA(configDir string) error {
	// Download cert
	resp, err := c.httpClient.Get(c.baseURL + "/skill/ca.pem")
	if err != nil {
		return fmt.Errorf("download CA cert: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("CA cert not available (status %d)", resp.StatusCode)
	}
	certPEM, _ := io.ReadAll(resp.Body)

	// Download key (requires client auth)
	req, err := http.NewRequest("GET", c.baseURL+"/client/ca-key", nil)
	if err != nil {
		return fmt.Errorf("build CA key request: %w", err)
	}
	req.Header.Set("X-Duckway-Token", c.token)
	resp2, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("download CA key: %w", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		return fmt.Errorf("CA key not available (status %d)", resp2.StatusCode)
	}
	keyPEM, _ := io.ReadAll(resp2.Body)

	if err := os.WriteFile(configDir+"/ca.pem", certPEM, 0644); err != nil {
		return fmt.Errorf("write CA cert: %w", err)
	}
	if err := os.WriteFile(configDir+"/ca-key.pem", keyPEM, 0600); err != nil {
		return fmt.Errorf("write CA key: %w", err)
	}
	return nil
}

// FetchConfig gets gateway configuration (proxy port, etc.)
func (c *APIClient) FetchConfig() (map[string]string, error) {
	resp, err := c.httpClient.Get(c.baseURL + "/client/config")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("config endpoint returned %d", resp.StatusCode)
	}
	var cfg map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	return cfg, nil
}

// CCAssignment mirrors what GET /client/cc returns. CC v2: a client has
// AT MOST one CC, so the response is a single object, not a list. We keep
// the slice-returning Fetch helper so callers (sync, daemon) treat 0 vs 1
// uniformly.
type CCAssignment struct {
	CCID             string `json:"cc_id"`
	CCName           string `json:"cc_name"`
	AgentType        string `json:"agent_type"`
	ManagementHandle string `json:"management_handle"`
}

// FetchCC asks the server for the (single) CC bound to this client.
// Returns nil when none is assigned. Returns nil on 404 too — that means
// the server is older or the endpoint doesn't exist.
func (c *APIClient) FetchCC() ([]CCAssignment, error) {
	req, err := http.NewRequest("GET", c.baseURL+"/client/cc", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Duckway-Token", c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch cc: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return nil, nil
	}
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("server returned %d: %s", resp.StatusCode, string(body))
	}
	var raw struct {
		Assigned         bool   `json:"assigned"`
		CCID             string `json:"cc_id"`
		CCName           string `json:"cc_name"`
		AgentType        string `json:"agent_type"`
		ManagementHandle string `json:"management_handle"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("parse cc: %w", err)
	}
	if !raw.Assigned {
		return nil, nil
	}
	return []CCAssignment{{
		CCID: raw.CCID, CCName: raw.CCName, AgentType: raw.AgentType,
		ManagementHandle: raw.ManagementHandle,
	}}, nil
}

// CCChannelInfo mirrors the public fields /client/cc/channels exposes.
type CCChannelInfo struct {
	Handle    string `json:"handle"`
	Name      string `json:"name"`
	Topic     string `json:"topic"`
	Kind      string `json:"kind"`
	SessionID string `json:"session_id,omitempty"`
	Cwd       string `json:"cwd,omitempty"`
	Archived  bool   `json:"archived"`
}

// FetchCCChannels returns the channels under this client's CC.
func (c *APIClient) FetchCCChannels() ([]CCChannelInfo, error) {
	req, err := http.NewRequest("GET", c.baseURL+"/client/cc/channels", nil)
	if err != nil {
		return nil, fmt.Errorf("build channels request: %w", err)
	}
	req.Header.Set("X-Duckway-Token", c.token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch channels: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("server %d: %s", resp.StatusCode, string(body))
	}
	var out []CCChannelInfo
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("parse channels: %w", err)
	}
	return out, nil
}

// CreateCCChannelResult is what POST /client/cc/channels returns.
type CreateCCChannelResult struct {
	Handle string `json:"handle"`
	Name   string `json:"name"`
	Topic  string `json:"topic"`
	Cwd    string `json:"cwd"`
	Kind   string `json:"kind"`
}

// CreateCCChannel provisions a new task channel under this client's CC.
// Server picks the dwch_ handle and forwards to Discord.
func (c *APIClient) CreateCCChannel(ctx context.Context, name, topic, cwd string) (*CreateCCChannelResult, error) {
	body, _ := json.Marshal(map[string]string{"name": name, "topic": topic, "cwd": cwd})
	req, err := http.NewRequestWithContext(ctx, "POST",
		c.baseURL+"/client/cc/channels", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Duckway-Token", c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("create channel: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("server %d: %s", resp.StatusCode, string(raw))
	}
	var out CreateCCChannelResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	return &out, nil
}

// PostCC posts a bot-author message to a CC channel by handle.
func (c *APIClient) PostCC(ctx context.Context, handle, content string) error {
	body, _ := json.Marshal(map[string]string{"content": content})
	req, err := http.NewRequestWithContext(ctx, "POST",
		c.baseURL+"/client/cc/channels/"+handle+"/messages", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("X-Duckway-Token", c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("server %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

func (c *APIClient) Ping() error {
	req, err := http.NewRequest("GET", c.baseURL+"/client/keys", nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Duckway-Token", c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("server unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 401 {
		return fmt.Errorf("invalid token")
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("server returned %d", resp.StatusCode)
	}
	return nil
}

// Register calls the admin API to create a client. Requires admin cookie, not client token.
// This is used during `duckway init` when the admin provides credentials.
func RegisterClient(baseURL, adminSession, clientName string) (clientID, token string, err error) {
	body, _ := json.Marshal(map[string]string{"name": clientName})
	req, err := http.NewRequest("POST", baseURL+"/api/clients", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", "duckway_session="+adminSession)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("register: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 201 {
		respBody, _ := io.ReadAll(resp.Body)
		return "", "", fmt.Errorf("register failed (%d): %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", err
	}
	return result.ID, result.Token, nil
}

// AdminLogin gets a session cookie for admin API access.
func AdminLogin(baseURL, username, password string) (session string, err error) {
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	resp, err := http.Post(baseURL+"/api/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("login failed (status %d)", resp.StatusCode)
	}

	for _, c := range resp.Cookies() {
		if c.Name == "duckway_session" {
			return c.Value, nil
		}
	}
	return "", fmt.Errorf("no session cookie in response")
}
