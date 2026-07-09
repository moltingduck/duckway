package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGitHubAppMinterWriteLive(t *testing.T) {
	if os.Getenv("DUCKWAY_TEST_GITHUB_APP_WRITE_LIVE") != "1" {
		t.Skip("set DUCKWAY_TEST_GITHUB_APP_WRITE_LIVE=1 to run")
	}
	cfg := loadGitHubAppWriteLiveConfig(t)
	credJSON, _ := json.Marshal(map[string]interface{}{
		"type":            "github_app",
		"app_id":          cfg.AppID,
		"installation_id": cfg.InstallationID,
		"private_key":     cfg.PrivateKey,
		"base_url":        cfg.BaseURL,
	})
	cred, ok, err := parseGitHubAppCredential(string(credJSON))
	if err != nil {
		t.Fatalf("parse github app credential: %v", err)
	}
	if !ok {
		t.Fatal("live credential is not a github_app credential")
	}
	jwt, err := githubAppJWT(cred.AppID, cred.PrivateKey, time.Now())
	if err != nil {
		t.Fatalf("sign github app jwt: %v", err)
	}
	_, repo, err := parseGitHubOwnerRepo(cfg.Repository)
	if err != nil {
		t.Fatal(err)
	}
	body := githubInstallationTokenRequest{
		Repositories: []string{repo},
		Permissions:  map[string]string{"contents": "write"},
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	baseURL := strings.TrimRight(cred.BaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	minted, err := mintGitHubInstallationToken(ctx, http.DefaultClient, cred, baseURL, jwt, bodyBytes, time.Now())
	if err != nil {
		t.Fatalf("mint contents:write token: %v", err)
	}
	t.Logf("GitHub App token permissions for %s: %+v", cfg.Repository, minted.Permissions)
	if minted.Permissions["contents"] != "write" {
		t.Fatalf("contents permission = %q, want write; update the GitHub App installation permissions for %s", minted.Permissions["contents"], cfg.Repository)
	}
}

type githubAppWriteLiveConfig struct {
	AppID          int64  `json:"app_id"`
	InstallationID int64  `json:"installation_id"`
	PrivateKey     string `json:"private_key"`
	Repository     string `json:"repository"`
	BaseURL        string `json:"base_url,omitempty"`
}

func loadGitHubAppWriteLiveConfig(t *testing.T) githubAppWriteLiveConfig {
	t.Helper()
	configPath := os.Getenv("DUCKWAY_GITHUB_APP_LIVE_CONFIG")
	if configPath == "" {
		configPath = findGitHubAppWriteLiveConfig(t)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read live config %s: %v", configPath, err)
	}
	var cfg githubAppWriteLiveConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse live config %s: %v", configPath, err)
	}
	if cfg.AppID <= 0 || cfg.InstallationID <= 0 || strings.TrimSpace(cfg.PrivateKey) == "" || strings.TrimSpace(cfg.Repository) == "" {
		t.Fatalf("live config %s requires app_id, installation_id, private_key, and repository", configPath)
	}
	return cfg
}

func findGitHubAppWriteLiveConfig(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	for dir := wd; ; dir = filepath.Dir(dir) {
		candidate := filepath.Join(dir, "secrets", "github-app-live.json")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		if parent := filepath.Dir(dir); parent == dir {
			break
		}
	}
	return filepath.Join("secrets", "github-app-live.json")
}
