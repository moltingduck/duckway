package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
)

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
		httpClient: &http.Client{},
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
	req, _ := http.NewRequest("GET", c.baseURL+"/client/ca-key", nil)
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

	os.WriteFile(configDir+"/ca.pem", certPEM, 0644)
	os.WriteFile(configDir+"/ca-key.pem", keyPEM, 0600)
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
	json.NewDecoder(resp.Body).Decode(&cfg)
	return cfg, nil
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
