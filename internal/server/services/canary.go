package services

import (
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
	Type        string // canarytokens.org type ID
	DisplayName string
	Description string
	DeployPath  string // where to place on client machine
	FormatFn    func(resp canaryResponse) string // generates file content
}

var SupportedCanaryTypes = []CanaryTokenType{
	{
		Type:        "aws_keys",
		DisplayName: "AWS Credentials",
		Description: "Fake AWS access key + secret key in ~/.aws/credentials format",
		DeployPath:  ".aws/credentials",
		FormatFn: func(r canaryResponse) string {
			return fmt.Sprintf("[default]\naws_access_key_id = %s\naws_secret_access_key = %s\n",
				r.AWSAccessKeyID, r.AWSSecretAccessKey)
		},
	},
	{
		Type:        "github",
		DisplayName: "GitHub Token",
		Description: "Fake GitHub personal access token",
		DeployPath:  ".config/gh/canary_token",
		FormatFn: func(r canaryResponse) string {
			return r.TokenValue
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

// GenerateForClient creates canary tokens for a client based on enabled types.
func (s *CanaryService) GenerateForClient(clientID, clientName string) error {
	settings, err := s.canaryQ.GetSettings()
	if err != nil {
		return fmt.Errorf("get settings: %w", err)
	}

	if settings.Email == "" {
		return nil // No email configured, skip
	}

	var enabledTypes []string
	if err := json.Unmarshal([]byte(settings.EnabledTypes), &enabledTypes); err != nil {
		return fmt.Errorf("parse enabled types: %w", err)
	}

	if len(enabledTypes) == 0 {
		return nil
	}

	for _, typeName := range enabledTypes {
		tokenType := findType(typeName)
		if tokenType == nil {
			continue
		}

		memo := fmt.Sprintf("duckway-canary/%s/%s", clientName, typeName)

		resp, err := s.createToken(typeName, settings.Email, memo)
		if err != nil {
			log.Printf("Failed to create canary token %s for %s: %v", typeName, clientName, err)
			continue
		}

		tokenValue := resp.TokenValue
		var secretValue *string
		deployContent := ""

		if typeName == "aws_keys" {
			tokenValue = resp.AWSAccessKeyID
			sv := resp.AWSSecretAccessKey
			secretValue = &sv
		}

		if tokenType.FormatFn != nil {
			deployContent = tokenType.FormatFn(*resp)
		}

		id, _ := GenerateToken(16)
		ct := &queries.CanaryToken{
			ID:            id,
			ClientID:      clientID,
			TokenType:     typeName,
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
			log.Printf("Created canary token %s for client %s", typeName, clientName)
		}
	}

	return nil
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
