package services

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hackerduck/duckway/internal/database/queries"
)

const canaryAPI = "https://canarytokens.org/d3aece8093b71007b5ccfedad91ebb11/generate"

// CanaryTokenType defines a supported canary token type and how to deploy it.
type CanaryTokenType struct {
	Type        string
	DisplayName string
	Description string
	Category    string // "api" = canarytokens.org, "local" = self-generated
	DeployPath  string
	FormatFn    func(resp canaryResponse, hostname string) string
}

// All supported canary token types.
// "api" types call canarytokens.org; "local" types are self-generated with embedded canary URL.
var SupportedCanaryTypes = []CanaryTokenType{
	// === canarytokens.org API types ===
	{
		Type:        "aws_keys",
		DisplayName: "AWS Credentials",
		Description: "Fake AWS access key + secret key (~/.aws/credentials)",
		Category:    "api",
		DeployPath:  ".aws/credentials",
		FormatFn: func(r canaryResponse, _ string) string {
			return fmt.Sprintf("[default]\naws_access_key_id = %s\naws_secret_access_key = %s\nregion = us-east-1\n",
				r.AWSAccessKeyID, r.AWSSecretAccessKey)
		},
	},
	{
		Type:        "github",
		DisplayName: "GitHub Token",
		Description: "Fake GitHub personal access token (~/.config/gh/hosts.yml)",
		Category:    "api",
		DeployPath:  ".config/gh/hosts.yml.bak",
		FormatFn: func(r canaryResponse, _ string) string {
			return fmt.Sprintf("github.com:\n    oauth_token: %s\n    user: devops-deploy\n    git_protocol: https\n", r.TokenValue)
		},
	},
	{
		Type:        "kubeconfig",
		DisplayName: "Kubernetes Config",
		Description: "Fake kubeconfig with cluster certs (~/.kube/config.bak)",
		Category:    "api",
		DeployPath:  ".kube/config.bak",
		FormatFn: func(r canaryResponse, _ string) string {
			return r.Kubeconfig
		},
	},
	{
		Type:        "wireguard",
		DisplayName: "WireGuard Config",
		Description: "Fake WireGuard VPN config with private key",
		Category:    "api",
		DeployPath:  ".config/wireguard/wg-company.conf",
		FormatFn: func(r canaryResponse, _ string) string {
			return r.WGConf
		},
	},
	// === Self-generated local types (embed canary URL for DNS triggering) ===
	{
		Type:        "env_file",
		DisplayName: ".env File",
		Description: "Fake .env with mixed API keys, DB creds, and canary URLs",
		Category:    "local",
		DeployPath:  ".env.production.bak",
		FormatFn: func(_ canaryResponse, hostname string) string {
			dbPass := randomHex(16)
			secret := randomHex(32)
			return fmt.Sprintf(`# Production environment — DO NOT COMMIT
DATABASE_URL=postgres://admin:%s@db.internal:5432/production
REDIS_URL=redis://default:%s@cache.internal:6379/0
SECRET_KEY=%s
SENTRY_DSN=https://%s@sentry.io/123456
STRIPE_SECRET_KEY=sk_live_%s
SENDGRID_API_KEY=SG.%s
WEBHOOK_URL=https://%s/webhook/prod
`, dbPass, randomHex(12), secret, randomHex(16), randomHex(24), randomHex(22)+"."+randomHex(22), hostname)
		},
	},
	{
		Type:        "ssh_key",
		DisplayName: "SSH Private Key",
		Description: "Fake SSH private key (triggers on use via canary hostname)",
		Category:    "local",
		DeployPath:  ".ssh/id_deploy",
		FormatFn: func(_ canaryResponse, hostname string) string {
			// Generate a realistic-looking but fake SSH key with embedded canary
			keyBody := randomBase64Lines(24)
			return fmt.Sprintf(`-----BEGIN OPENSSH PRIVATE KEY-----
%s
-----END OPENSSH PRIVATE KEY-----
# deploy key for git@%s
`, keyBody, hostname)
		},
	},
	{
		Type:        "npm_token",
		DisplayName: "NPM Token",
		Description: "Fake .npmrc with auth token",
		Category:    "local",
		DeployPath:  ".npmrc.bak",
		FormatFn: func(_ canaryResponse, hostname string) string {
			token := "npm_" + randomHex(36)
			return fmt.Sprintf(`//registry.npmjs.org/:_authToken=%s
//npm.pkg.github.com/:_authToken=ghp_%s
# canary: %s
`, token, randomHex(36), hostname)
		},
	},
	{
		Type:        "docker_config",
		DisplayName: "Docker Config",
		Description: "Fake Docker Hub credentials (~/.docker/config.json.bak)",
		Category:    "local",
		DeployPath:  ".docker/config.json.bak",
		FormatFn: func(_ canaryResponse, hostname string) string {
			// Base64 of user:token
			auth := randomBase64(32)
			return fmt.Sprintf(`{
	"auths": {
		"https://index.docker.io/v1/": {
			"auth": "%s",
			"email": "deploy@%s"
		},
		"ghcr.io": {
			"auth": "%s"
		}
	}
}
`, auth, hostname, randomBase64(32))
		},
	},
	{
		Type:        "gcp_service_account",
		DisplayName: "GCP Service Account",
		Description: "Fake GCP service account JSON key",
		Category:    "local",
		DeployPath:  ".config/gcloud/application_default_credentials.json.bak",
		FormatFn: func(_ canaryResponse, hostname string) string {
			projectID := "prod-" + randomHex(6)
			clientID := randomDigits(21)
			privKeyID := randomHex(20)
			return fmt.Sprintf(`{
  "type": "service_account",
  "project_id": "%s",
  "private_key_id": "%s",
  "private_key": "-----BEGIN RSA PRIVATE KEY-----\n%s\n-----END RSA PRIVATE KEY-----\n",
  "client_email": "deploy@%s.iam.gserviceaccount.com",
  "client_id": "%s",
  "auth_uri": "https://accounts.google.com/o/oauth2/auth",
  "token_uri": "https://%s/token",
  "client_x509_cert_url": "https://www.googleapis.com/robot/v1/metadata/x509/deploy@%s.iam.gserviceaccount.com"
}
`, projectID, privKeyID, randomBase64Lines(12), projectID, clientID, hostname, projectID)
		},
	},
	{
		Type:        "pypirc",
		DisplayName: "PyPI Token",
		Description: "Fake .pypirc with upload credentials",
		Category:    "local",
		DeployPath:  ".pypirc.bak",
		FormatFn: func(_ canaryResponse, hostname string) string {
			token := "pypi-" + randomBase64(48)
			return fmt.Sprintf(`[distutils]
index-servers =
    pypi

[pypi]
repository = https://upload.pypi.org/legacy/
username = __token__
password = %s
# config source: %s
`, token, hostname)
		},
	},
	{
		Type:        "slack_token",
		DisplayName: "Slack Token",
		Description: "Fake Slack bot/user token in config file",
		Category:    "local",
		DeployPath:  ".config/slack/credentials.bak",
		FormatFn: func(_ canaryResponse, hostname string) string {
			botToken := "xoxb-" + randomDigits(12) + "-" + randomDigits(13) + "-" + randomHex(24)
			userToken := "xoxp-" + randomDigits(12) + "-" + randomDigits(12) + "-" + randomDigits(13) + "-" + randomHex(32)
			return fmt.Sprintf(`# Slack workspace credentials
SLACK_BOT_TOKEN=%s
SLACK_USER_TOKEN=%s
SLACK_WEBHOOK_URL=https://%s/services/T00000000/B00000000/XXXXXXXXXXXXXXXXXXXXXXXX
`, botToken, userToken, hostname)
		},
	},
}

type canaryResponse struct {
	Token              string `json:"token"`
	Hostname           string `json:"hostname"`
	AuthToken          string `json:"auth_token"`
	Email              string `json:"email"`
	TokenType          string `json:"token_type"`
	AWSAccessKeyID     string `json:"aws_access_key_id"`
	AWSSecretAccessKey string `json:"aws_secret_access_key"`
	TokenValue         string `json:"token_value"`
	Kubeconfig         string `json:"kubeconfig"`
	WGConf             string `json:"wg_conf"`
	Error              string `json:"error"`
}

type CanaryService struct {
	canaryQ *queries.CanaryQueries
	client  *http.Client
}

func NewCanaryService(canaryQ *queries.CanaryQueries) *CanaryService {
	return &CanaryService{
		canaryQ: canaryQ,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

// GenerateForClient creates canary tokens for a client based on settings.
// Called automatically on client registration unless the client is excluded
// or has canary_enabled=false.
func (s *CanaryService) GenerateForClient(clientID, clientName string) error {
	settings, err := s.canaryQ.GetSettings()
	if err != nil {
		return fmt.Errorf("get settings: %w", err)
	}

	if settings.Email == "" {
		return nil
	}

	// Check exclude list
	var excludeList []string
	if settings.ExcludeClients != "" {
		json.Unmarshal([]byte(settings.ExcludeClients), &excludeList)
	}
	for _, ex := range excludeList {
		if ex == clientName || ex == clientID {
			log.Printf("Client %s is excluded from canary tokens", clientName)
			return nil
		}
	}

	// Get enabled types (default: API types only)
	var enabledTypes []string
	if err := json.Unmarshal([]byte(settings.EnabledTypes), &enabledTypes); err != nil || len(enabledTypes) == 0 {
		for _, t := range SupportedCanaryTypes {
			if t.Category == "api" {
				enabledTypes = append(enabledTypes, t.Type)
			}
		}
	}

	for _, typeName := range enabledTypes {
		tokenType := findType(typeName)
		if tokenType == nil {
			continue
		}

		memo := fmt.Sprintf("duckway-canary/%s/%s", clientName, typeName)

		if tokenType.Category == "api" {
			s.generateAPIToken(tokenType, clientID, clientName, settings.Email, memo)
		} else {
			s.generateLocalToken(tokenType, clientID, clientName, settings.Email, memo)
		}
	}

	return nil
}

func (s *CanaryService) generateAPIToken(tokenType *CanaryTokenType, clientID, clientName, email, memo string) {
	resp, err := s.createToken(tokenType.Type, email, memo)
	if err != nil {
		log.Printf("Failed to create canary token %s for %s: %v", tokenType.Type, clientName, err)
		return
	}

	tokenValue := resp.TokenValue
	var secretValue *string

	if tokenType.Type == "aws_keys" {
		tokenValue = resp.AWSAccessKeyID
		sv := resp.AWSSecretAccessKey
		secretValue = &sv
	}

	deployContent := ""
	if tokenType.FormatFn != nil {
		deployContent = tokenType.FormatFn(*resp, resp.Hostname)
	}

	id, _ := GenerateToken(16)
	ct := &queries.CanaryToken{
		ID:            id,
		ClientID:      clientID,
		TokenType:     tokenType.Type,
		CanaryToken:   resp.Token,
		AuthToken:     resp.AuthToken,
		TokenValue:    tokenValue,
		SecretValue:   secretValue,
		Memo:          memo,
		DeployPath:    tokenType.DeployPath,
		DeployContent: deployContent,
	}

	if err := s.canaryQ.Create(ct); err != nil {
		log.Printf("Failed to save canary token: %v", err)
	} else {
		log.Printf("Created canary token %s for client %s", tokenType.Type, clientName)
	}
}

func (s *CanaryService) generateLocalToken(tokenType *CanaryTokenType, clientID, clientName, email, memo string) {
	// For local types, we first create a DNS canary token to get a hostname for embedding
	resp, err := s.createToken("dns", email, memo)
	if err != nil {
		log.Printf("Failed to create DNS canary for %s/%s: %v", clientName, tokenType.Type, err)
		return
	}

	deployContent := ""
	if tokenType.FormatFn != nil {
		deployContent = tokenType.FormatFn(canaryResponse{}, resp.Hostname)
	}

	id, _ := GenerateToken(16)
	ct := &queries.CanaryToken{
		ID:            id,
		ClientID:      clientID,
		TokenType:     tokenType.Type,
		CanaryToken:   resp.Token,
		AuthToken:     resp.AuthToken,
		TokenValue:    resp.Hostname,
		Memo:          memo,
		DeployPath:    tokenType.DeployPath,
		DeployContent: deployContent,
	}

	if err := s.canaryQ.Create(ct); err != nil {
		log.Printf("Failed to save local canary token: %v", err)
	} else {
		log.Printf("Created local canary token %s for client %s (DNS: %s)", tokenType.Type, clientName, resp.Hostname)
	}
}

func (s *CanaryService) createToken(tokenType, email, memo string) (*canaryResponse, error) {
	form := url.Values{
		"type":  {tokenType},
		"email": {email},
		"memo":  {memo},
	}

	resp, err := s.client.Post(canaryAPI, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("post to canarytokens: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("canarytokens returned %d: %s", resp.StatusCode, string(body))
	}

	var result canaryResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	if result.Error != "" {
		return nil, fmt.Errorf("canarytokens error: %s", result.Error)
	}

	return &result, nil
}

func findType(name string) *CanaryTokenType {
	for i := range SupportedCanaryTypes {
		if SupportedCanaryTypes[i].Type == name {
			return &SupportedCanaryTypes[i]
		}
	}
	return nil
}

// Helper functions for generating realistic fake credentials

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)[:n]
}

func randomDigits(n int) string {
	const digits = "0123456789"
	b := make([]byte, n)
	rand.Read(b)
	for i := range b {
		b[i] = digits[int(b[i])%len(digits)]
	}
	return string(b)
}

func randomBase64(n int) string {
	const charset = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	b := make([]byte, n)
	rand.Read(b)
	for i := range b {
		b[i] = charset[int(b[i])%len(charset)]
	}
	return string(b)
}

func randomBase64Lines(lines int) string {
	var parts []string
	for i := 0; i < lines; i++ {
		parts = append(parts, randomBase64(64))
	}
	return strings.Join(parts, "\n")
}
