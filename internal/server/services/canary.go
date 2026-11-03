package services

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/hackerduck/duckway/internal/database/queries"
)

const canaryAPI = "https://canarytokens.org/d3aece8093b71007b5ccfedad91ebb11/generate"

// CanaryTokenType defines a supported canary token type and how to deploy it.
type CanaryTokenType struct {
	Type           string
	DisplayName    string
	Description    string
	Category       string // "api" = canarytokens.org, "local" = self-generated
	DeployMode     string // "create" (default), "append" = merge with existing file, "bak" = always use .bak
	DeployPath     string
	DefaultEnabled bool // true = enabled by default in settings
	FormatFn       func(resp canaryResponse, hostname string) string
}

// All supported canary token types.
// "api" types call canarytokens.org; "local" types are self-generated with embedded canary URL.
// DefaultEnabled controls which types are on when no explicit selection is saved.
var SupportedCanaryTypes = []CanaryTokenType{
	// === canarytokens.org API types (real tripwire credentials) ===
	{
		Type:           "aws_keys",
		DisplayName:    "AWS Credentials",
		Description:    "Fake AWS access key + secret key",
		Category:       "api",
		DeployMode:     "append", // merge: add [canary] profile alongside real [default]
		DeployPath:     ".aws/credentials",
		DefaultEnabled: true,
		FormatFn: func(r canaryResponse, _ string) string {
			return fmt.Sprintf("\n[canary-prod-backup]\naws_access_key_id = %s\naws_secret_access_key = %s\nregion = us-east-1\n",
				r.AWSAccessKeyID, r.AWSSecretAccessKey)
		},
	},
	{
		Type:           "kubeconfig",
		DisplayName:    "Kubernetes Config",
		Description:    "Fake kubeconfig with cluster certs",
		Category:       "api",
		DeployMode:     "bak", // k8s config doesn't support multi-file merge
		DeployPath:     ".kube/config.bak",
		DefaultEnabled: true,
		FormatFn: func(r canaryResponse, _ string) string {
			return r.Kubeconfig
		},
	},
	{
		Type:           "wireguard",
		DisplayName:    "WireGuard Config",
		Description:    "Fake WireGuard VPN config with private key",
		Category:       "api",
		DeployMode:     "create", // separate file, won't conflict
		DeployPath:     ".config/wireguard/wg-company.conf",
		DefaultEnabled: true,
		FormatFn: func(r canaryResponse, _ string) string {
			return r.WGConf
		},
	},
	// === Self-generated local types (embed canary DNS hostname) ===
	{
		Type:           "github_token",
		DisplayName:    "GitHub Token",
		Description:    "Fake GitHub PAT in git credential store (classic + fine-grained)",
		Category:       "local",
		DeployMode:     "append", // .git-credentials supports multiple lines
		DeployPath:     ".git-credentials",
		DefaultEnabled: true,
		FormatFn: func(_ canaryResponse, hostname string) string {
			// Classic PAT (ghp_*) — still common in CI pipelines
			ghToken := "ghp_" + randomHex(36)
			// Fine-grained PAT (github_pat_*) — GitHub's recommended format;
			// format: github_pat_ + 11-char owner-id + _ + 59-char body = 82 chars
			pat := "github_pat_" + randomHex(11) + "_" + randomBase64(59)
			return fmt.Sprintf("\nhttps://deploy-bot:%s@github.com\nhttps://ci-runner:%s@github.com\nhttps://admin:%s@%s\n",
				ghToken, pat, randomHex(20), hostname)
		},
	},
	{
		Type:           "env_file",
		DisplayName:    ".env File",
		Description:    "Fake .env with mixed API keys, DB creds, and canary URLs",
		Category:       "local",
		DeployMode:     "create", // standalone decoy file
		DeployPath:     ".env.production.bak",
		DefaultEnabled: true,
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
		Type:           "ssh_key",
		DisplayName:    "SSH Private Key",
		Description:    "Fake SSH private key (triggers on use via canary hostname)",
		Category:       "local",
		DeployMode:     "create",
		DeployPath:     ".ssh/id_deploy",
		DefaultEnabled: true,
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
		Type:           "npm_token",
		DisplayName:    "NPM Token",
		Description:    "Fake .npmrc with auth token",
		Category:       "local",
		DeployPath:     ".npmrc.bak",
		DefaultEnabled: false,
		FormatFn: func(_ canaryResponse, hostname string) string {
			token := "npm_" + randomHex(36)
			return fmt.Sprintf(`//registry.npmjs.org/:_authToken=%s
//npm.pkg.github.com/:_authToken=ghp_%s
# canary: %s
`, token, randomHex(36), hostname)
		},
	},
	{
		Type:           "docker_config",
		DisplayName:    "Docker Config",
		Description:    "Fake Docker Hub credentials",
		Category:       "local",
		DeployPath:     ".docker/config.json.bak",
		DefaultEnabled: false,
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
		Type:           "gcp_service_account",
		DisplayName:    "GCP Service Account",
		Description:    "Fake GCP service account JSON key",
		Category:       "local",
		DeployPath:     ".config/gcloud/application_default_credentials.json.bak",
		DefaultEnabled: false,
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
		Type:           "pypirc",
		DisplayName:    "PyPI Token",
		Description:    "Fake .pypirc with upload credentials",
		Category:       "local",
		DeployPath:     ".pypirc.bak",
		DefaultEnabled: false,
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
		Type:           "slack_token",
		DisplayName:    "Slack Token",
		Description:    "Fake Slack bot/user token in config file",
		Category:       "local",
		DeployPath:     ".config/slack/credentials.bak",
		DefaultEnabled: false,
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
	// === Stealth canary types — blend into existing files ===
	{
		Type:           "bash_history",
		DisplayName:    "Bash History",
		Description:    "Append realistic export commands with fake creds to .bash_history",
		Category:       "local",
		DeployMode:     "append",
		DeployPath:     ".bash_history",
		DefaultEnabled: true,
		FormatFn: func(_ canaryResponse, hostname string) string {
			awsKey := "AKIA" + randomHex(16)
			awsSecret := randomBase64(40)
			ghPAT := "github_pat_" + randomHex(11) + "_" + randomBase64(59)
			apiKey := "sk-proj-" + randomHex(48)
			antKey := "sk-ant-" + randomHex(40)
			slackToken := "xoxb-" + randomDigits(12) + "-" + randomDigits(13) + "-" + randomHex(24)
			dbPass := randomHex(16)
			stripeKey := "sk_live_" + randomHex(24)
			return fmt.Sprintf(`export AWS_ACCESS_KEY_ID=%s
export AWS_SECRET_ACCESS_KEY=%s
export OPENAI_API_KEY=%s
export ANTHROPIC_API_KEY=%s
export GITHUB_TOKEN=%s
export SLACK_BOT_TOKEN=%s
export STRIPE_SECRET_KEY=%s
export DATABASE_URL=postgres://admin:%s@db-prod.internal:5432/production
curl -X POST https://%s/webhook/deploy -d '{"status":"complete"}'
ssh -i ~/.ssh/id_deploy deploy@prod-worker-03.internal
kubectl --kubeconfig ~/.kube/config.bak get pods -n production
`, awsKey, awsSecret, apiKey, antKey, ghPAT, slackToken, stripeKey, dbPass, hostname)
		},
	},
	{
		Type:           "zsh_history",
		DisplayName:    "Zsh History",
		Description:    "Append realistic export commands with fake creds to .zsh_history (extended format)",
		Category:       "local",
		DeployMode:     "append",
		DeployPath:     ".zsh_history",
		DefaultEnabled: true,
		FormatFn: func(_ canaryResponse, hostname string) string {
			awsKey := "AKIA" + randomHex(16)
			awsSecret := randomBase64(40)
			ghPAT := "github_pat_" + randomHex(11) + "_" + randomBase64(59)
			apiKey := "sk-proj-" + randomHex(48)
			antKey := "sk-ant-" + randomHex(40)
			slackToken := "xoxb-" + randomDigits(12) + "-" + randomDigits(13) + "-" + randomHex(24)
			dbPass := randomHex(16)
			stripeKey := "sk_live_" + randomHex(24)
			// Use zsh extended history format: ": timestamp:elapsed;command"
			// Timestamps are spaced ~1 minute apart, anchored ~7 days in the past.
			base := time.Now().Unix() - 86400*7
			return fmt.Sprintf(
				": %d:0;export AWS_ACCESS_KEY_ID=%s\n"+
					": %d:0;export AWS_SECRET_ACCESS_KEY=%s\n"+
					": %d:0;export OPENAI_API_KEY=%s\n"+
					": %d:0;export ANTHROPIC_API_KEY=%s\n"+
					": %d:0;export GITHUB_TOKEN=%s\n"+
					": %d:0;export SLACK_BOT_TOKEN=%s\n"+
					": %d:0;export STRIPE_SECRET_KEY=%s\n"+
					": %d:0;export DATABASE_URL=postgres://admin:%s@db-prod.internal:5432/production\n"+
					": %d:0;curl -X POST https://%s/webhook/deploy -d '{\"status\":\"complete\"}'\n"+
					": %d:0;ssh -i ~/.ssh/id_deploy deploy@prod-worker-03.internal\n"+
					": %d:0;kubectl --kubeconfig ~/.kube/config.bak get pods -n production\n",
				base, awsKey,
				base+63, awsSecret,
				base+127, apiKey,
				base+191, antKey,
				base+254, ghPAT,
				base+318, slackToken,
				base+382, stripeKey,
				base+445, dbPass,
				base+509, hostname,
				base+572,
				base+636,
			)
		},
	},
	{
		Type:           "netrc",
		DisplayName:    ".netrc Credentials",
		Description:    "Fake .netrc with machine credentials (grep catches 'password')",
		Category:       "local",
		DeployMode:     "append", // .netrc supports multiple machine blocks
		DeployPath:     ".netrc",
		DefaultEnabled: false,
		FormatFn: func(_ canaryResponse, hostname string) string {
			return fmt.Sprintf(`machine github.com
  login deploy-bot
  password ghp_%s

machine registry.npmjs.org
  login company-ci
  password npm_%s

machine %s
  login admin
  password %s
`, randomHex(36), randomHex(36), hostname, randomHex(24))
		},
	},
	{
		Type:           "git_credentials",
		DisplayName:    "Git Credential Store (extra entries)",
		Description:    "Additional fake entries in .git-credentials",
		Category:       "local",
		DeployMode:     "append",
		DeployPath:     ".git-credentials",
		DefaultEnabled: false, // github_token already appends to this file
		FormatFn: func(_ canaryResponse, hostname string) string {
			ghToken := "ghp_" + randomHex(36)
			glToken := "glpat-" + randomHex(20)
			return fmt.Sprintf(`https://deploy-bot:%s@github.com
https://ci-runner:%s@gitlab.company.com
https://admin:%s@%s
`, ghToken, glToken, randomHex(20), hostname)
		},
	},
	{
		Type:           "pgpass",
		DisplayName:    "PostgreSQL .pgpass",
		Description:    "Fake .pgpass with database credentials",
		Category:       "local",
		DeployMode:     "append", // .pgpass supports multiple lines
		DeployPath:     ".pgpass",
		DefaultEnabled: true,
		FormatFn: func(_ canaryResponse, hostname string) string {
			return fmt.Sprintf(`# PostgreSQL password file
db-prod.internal:5432:production:admin:%s
db-replica.internal:5432:production:readonly:%s
%s:5432:canary:admin:%s
`, randomHex(24), randomHex(20), hostname, randomHex(24))
		},
	},
	{
		Type:           "bashrc_exports",
		DisplayName:    "Commented .bashrc Exports",
		Description:    "Append commented-out credential exports to .bashrc",
		Category:       "local",
		DeployMode:     "append",
		DeployPath:     ".bashrc",
		DefaultEnabled: true,
		FormatFn: func(_ canaryResponse, hostname string) string {
			return fmt.Sprintf(`
# old production credentials (moved to vault 2024-03)
# export AWS_ACCESS_KEY_ID=AKIA%s
# export AWS_SECRET_ACCESS_KEY=%s
# export GITHUB_TOKEN=ghp_%s
# export DATABASE_URL=postgres://admin:%s@%s:5432/production
`, randomHex(16), randomBase64(40), randomHex(36), randomHex(16), hostname)
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
	locks   sync.Map // clientID -> *sync.Mutex
}

func NewCanaryService(canaryQ *queries.CanaryQueries) *CanaryService {
	return &CanaryService{
		canaryQ: canaryQ,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

// clientLock serialises token generation for one client so concurrent
// generate/regenerate calls cannot duplicate tokens or delete each other's
// fresh tokens.
func (s *CanaryService) clientLock(clientID string) func() {
	m, _ := s.locks.LoadOrStore(clientID, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// ErrCanaryClientExcluded means the client is on the exclusion list or has
// canary_enabled=false, so no tokens may be generated for it.
var ErrCanaryClientExcluded = errors.New("this client is excluded from canary tokens or has canary disabled")

// ErrCanaryExclusionUnreadable means the stored exclusion list is not valid
// JSON, so exclusion cannot be evaluated; generation is refused (fail closed).
var ErrCanaryExclusionUnreadable = errors.New("canary exclusion list is unreadable; fix it in the canary settings")

// CanaryExcluded reports whether a client matches the exclusion list by name
// or ID. A non-empty list that fails to parse returns an error.
func CanaryExcluded(excludeJSON, clientName, clientID string) (bool, error) {
	if strings.TrimSpace(excludeJSON) == "" {
		return false, nil
	}
	var list []string
	if err := json.Unmarshal([]byte(excludeJSON), &list); err != nil {
		return false, ErrCanaryExclusionUnreadable
	}
	for _, ex := range list {
		if ex == clientName || ex == clientID {
			return true, nil
		}
	}
	return false, nil
}

// GenerateForClient creates canary tokens for a client based on settings.
// Called automatically on client registration. Excluded or canary-disabled
// clients are skipped silently.
func (s *CanaryService) GenerateForClient(clientID, clientName, shortID string) error {
	unlock := s.clientLock(clientID)
	defer unlock()
	existing, err := s.canaryQ.ListByClient(clientID)
	if err != nil {
		return fmt.Errorf("list tokens: %w", err)
	}
	if len(existing) > 0 {
		return nil
	}
	settings, err := s.canaryQ.GetSettings()
	if err != nil {
		return fmt.Errorf("get settings: %w", err)
	}
	if settings.Email == "" {
		return nil
	}
	if err := s.checkEligible(settings, clientID, clientName); err != nil {
		if errors.Is(err, ErrCanaryClientExcluded) {
			log.Printf("Client %s is excluded from canary tokens", clientName)
			return nil
		}
		return err
	}
	_, err = s.generateForClientInner(settings, clientID, clientName, shortID)
	return err
}

func (s *CanaryService) checkEligible(settings *queries.CanarySettings, clientID, clientName string) error {
	excluded, err := CanaryExcluded(settings.ExcludeClients, clientName, clientID)
	if err != nil {
		return err
	}
	if excluded {
		return ErrCanaryClientExcluded
	}
	enabled, found, err := s.canaryQ.ClientCanaryEnabled(clientID)
	if err != nil {
		return fmt.Errorf("get client: %w", err)
	}
	if found && !enabled {
		return ErrCanaryClientExcluded
	}
	return nil
}

type canaryGenResult struct {
	Created     []queries.CanaryToken
	FailedTypes []string
}

// generateForClientInner creates one token per enabled type. The caller holds
// the client lock and has validated settings and eligibility.
func (s *CanaryService) generateForClientInner(settings *queries.CanarySettings, clientID, clientName, shortID string) (canaryGenResult, error) {
	var res canaryGenResult
	email := tagEmail(settings.Email, shortID)

	var enabledTypes []string
	if err := json.Unmarshal([]byte(settings.EnabledTypes), &enabledTypes); err != nil || len(enabledTypes) == 0 {
		enabledTypes = nil
		for _, t := range SupportedCanaryTypes {
			if t.DefaultEnabled {
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
		var ct *queries.CanaryToken
		var err error
		if tokenType.Category == "api" {
			ct, err = s.generateAPIToken(tokenType, clientID, clientName, email, memo)
		} else {
			ct, err = s.generateLocalToken(tokenType, clientID, clientName, email, memo)
		}
		if err != nil {
			res.FailedTypes = append(res.FailedTypes, typeName)
			continue
		}
		res.Created = append(res.Created, *ct)
	}
	return res, nil
}

// ErrCanaryNoAlertEmail is returned by RegenerateForClient when no alert email
// is configured: nothing could be generated, so nothing may be deleted.
var ErrCanaryNoAlertEmail = errors.New("no canary alert email is set; existing tokens were kept")

// ErrCanaryNothingGenerated means generation produced no new tokens (for
// example canarytokens.org was unreachable).
var ErrCanaryNothingGenerated = errors.New("no new canary tokens could be generated; existing tokens were kept")

// RegenerateResult reports what a regenerate run did.
type RegenerateResult struct {
	Created      int `json:"created"`
	Replaced     int `json:"replaced"`      // old tokens removed
	FailedTypes  int `json:"failed_types"`  // types that could not be regenerated; their old tokens were kept
	DeleteFailed int `json:"delete_failed"` // old tokens kept because the remote delete failed
}

// RegenerateForClient generates fresh tokens and then deletes only the old
// tokens of the types that got a fresh replacement. Each old token is deleted
// from canarytokens.org first and its DB row removed only after that succeeds,
// so a failure never leaves a live remote token the DB no longer tracks.
func (s *CanaryService) RegenerateForClient(clientID, clientName, shortID string) (RegenerateResult, error) {
	var out RegenerateResult
	unlock := s.clientLock(clientID)
	defer unlock()
	settings, err := s.canaryQ.GetSettings()
	if err != nil {
		return out, fmt.Errorf("get settings: %w", err)
	}
	if settings.Email == "" {
		return out, ErrCanaryNoAlertEmail
	}
	if err := s.checkEligible(settings, clientID, clientName); err != nil {
		return out, err
	}
	old, err := s.canaryQ.ListByClient(clientID)
	if err != nil {
		return out, fmt.Errorf("list tokens: %w", err)
	}
	res, err := s.generateForClientInner(settings, clientID, clientName, shortID)
	if err != nil {
		return out, err
	}
	out.Created = len(res.Created)
	out.FailedTypes = len(res.FailedTypes)
	if out.Created == 0 {
		return out, ErrCanaryNothingGenerated
	}
	fresh := map[string]bool{}
	for _, t := range res.Created {
		fresh[t.TokenType] = true
	}
	for _, t := range old {
		if !fresh[t.TokenType] {
			continue
		}
		if err := s.DeleteRemoteToken(t.CanaryToken, t.AuthToken); err != nil {
			log.Printf("Warning: kept canary token %s (%s): remote delete failed: %v", t.ID, t.TokenType, err)
			out.DeleteFailed++
			continue
		}
		if err := s.canaryQ.DeleteByID(t.ID); err != nil {
			// The remote token is gone but its row remains: count it and carry on.
			log.Printf("Warning: kept canary token row %s: db delete failed: %v", t.ID, err)
			out.DeleteFailed++
			continue
		}
		out.Replaced++
	}
	return out, nil
}

// discardRemote best-effort deletes a remote token that could not be saved.
func (s *CanaryService) discardRemote(resp *canaryResponse) {
	if err := s.DeleteRemoteToken(resp.Token, resp.AuthToken); err != nil {
		log.Printf("Warning: could not remove unsaved remote canary token %s: %v", resp.Token, err)
	}
}

func (s *CanaryService) generateAPIToken(tokenType *CanaryTokenType, clientID, clientName, email, memo string) (*queries.CanaryToken, error) {
	resp, err := s.createToken(tokenType.Type, email, memo)
	if err != nil {
		log.Printf("Failed to create canary token %s for %s: %v", tokenType.Type, clientName, err)
		return nil, err
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
		s.discardRemote(resp)
		return nil, err
	}
	log.Printf("Created canary token %s for client %s", tokenType.Type, clientName)
	return ct, nil
}

func (s *CanaryService) generateLocalToken(tokenType *CanaryTokenType, clientID, clientName, email, memo string) (*queries.CanaryToken, error) {
	// For local types, we first create a DNS canary token to get a hostname for embedding
	resp, err := s.createToken("dns", email, memo)
	if err != nil {
		log.Printf("Failed to create DNS canary for %s/%s: %v", clientName, tokenType.Type, err)
		return nil, err
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
		s.discardRemote(resp)
		return nil, err
	}
	log.Printf("Created local canary token %s for client %s (DNS: %s)", tokenType.Type, clientName, resp.Hostname)
	return ct, nil
}

const canaryDeleteAPI = "https://canarytokens.org/d3aece8093b71007b5ccfedad91ebb11/delete"
const canaryManageURL = "https://canarytokens.org/manage"

// ManageURL returns the canarytokens.org management URL for a token.
func ManageURL(canaryToken, authToken string) string {
	if canaryToken == "" || authToken == "" {
		return ""
	}
	return canaryManageURL + "?token=" + canaryToken + "&auth=" + authToken
}

// DeleteRemoteToken deletes a token from canarytokens.org.
func (s *CanaryService) DeleteRemoteToken(canaryToken, authToken string) error {
	if canaryToken == "" || authToken == "" {
		return nil // local-only token, nothing to delete remotely
	}

	body, _ := json.Marshal(map[string]string{
		"token": canaryToken,
		"auth":  authToken,
	})

	resp, err := s.client.Post(canaryDeleteAPI, "application/json", strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("delete remote token: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	var result map[string]string
	json.Unmarshal(respBody, &result)

	if result["message"] == "success" {
		return nil
	}
	return fmt.Errorf("delete failed: %s", string(respBody))
}

// ErrCanaryTokenNotFound means no token with that ID exists.
var ErrCanaryTokenNotFound = errors.New("canary token not found")

// ErrCanaryRemoteDeleteFailed means canarytokens.org could not delete the
// token, so its row was kept (a live remote token must stay tracked). The
// remote API does not distinguish "already gone" from other failures, so the
// admin can retry with localOnly to remove the row without a remote call.
var ErrCanaryRemoteDeleteFailed = errors.New("could not delete the token on canarytokens.org; it was kept")

// DeleteToken deletes one token: remote first, then the DB row. With localOnly
// the remote call is skipped (the token is already gone or unreachable).
func (s *CanaryService) DeleteToken(tokenID string, localOnly bool) error {
	token, err := s.canaryQ.GetByID(tokenID)
	if err != nil {
		return ErrCanaryTokenNotFound
	}
	unlock := s.clientLock(token.ClientID)
	defer unlock()
	if !localOnly {
		if err := s.DeleteRemoteToken(token.CanaryToken, token.AuthToken); err != nil {
			log.Printf("Warning: kept canary token %s: remote delete failed: %v", token.ID, err)
			return ErrCanaryRemoteDeleteFailed
		}
	}
	return s.canaryQ.DeleteByID(tokenID)
}

// DeleteClientTokens deletes all canary tokens for a client. Tokens whose
// remote delete fails are kept (unless localOnly) and counted in failed.
func (s *CanaryService) DeleteClientTokens(clientID string, localOnly bool) (deleted, failed int, err error) {
	unlock := s.clientLock(clientID)
	defer unlock()
	tokens, err := s.canaryQ.ListByClient(clientID)
	if err != nil {
		return 0, 0, fmt.Errorf("list tokens: %w", err)
	}
	for _, t := range tokens {
		if !localOnly {
			if rerr := s.DeleteRemoteToken(t.CanaryToken, t.AuthToken); rerr != nil {
				log.Printf("Warning: kept canary token %s: remote delete failed: %v", t.ID, rerr)
				failed++
				continue
			}
		}
		if derr := s.canaryQ.DeleteByID(t.ID); derr != nil {
			return deleted, failed, fmt.Errorf("delete token: %w", derr)
		}
		deleted++
	}
	return deleted, failed, nil
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

func FindCanaryType(name string) *CanaryTokenType {
	return findType(name)
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

// tagEmail adds a +shortid tag to an email address for per-client identification.
// e.g., user@gmail.com + "abc123" → user+abc123@gmail.com
// Works with Gmail, Outlook, and most email providers that support + addressing.
func tagEmail(email, shortID string) string {
	if shortID == "" {
		return email
	}
	at := strings.LastIndex(email, "@")
	if at < 0 {
		return email
	}
	return email[:at] + "+" + shortID + email[at:]
}

func randomBase64Lines(lines int) string {
	var parts []string
	for i := 0; i < lines; i++ {
		parts = append(parts, randomBase64(64))
	}
	return strings.Join(parts, "\n")
}
