package handlers

import (
	"encoding/json"
	"testing"
)

func TestCCMaskKeepsTokenCountsVisibleButMasksRealSecrets(t *testing.T) {
	visible := []string{"max_tokens", "maxTokens", "min_tokens", "token_limit", "tokenLimit", "token_count", "input_tokens",
		"output_tokens", "context_tokens", "max_output_tokens", "token_budget", "tokenType", "token_type", "token_endpoint", "tokens"}
	secrets := []string{"token", "bot_token", "botToken", "access_token", "refresh_token", "api_token", "auth_token", "discord_bot_token",
		"webhook_secret", "password", "api_key", "authorization"}
	cfg := map[string]interface{}{}
	for _, k := range visible {
		cfg[k] = 4096
	}
	for _, k := range secrets {
		cfg[k] = "real-secret-value"
	}
	raw, _ := json.Marshal(cfg)
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(maskCCConfig(string(raw))), &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range visible {
		if got[k] != float64(4096) {
			t.Errorf("%s must stay visible, got %v", k, got[k])
		}
	}
	for _, k := range secrets {
		if got[k] != ccSecretMask {
			t.Errorf("%s must be masked, got %v", k, got[k])
		}
	}
}
