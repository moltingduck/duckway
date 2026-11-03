package handlers

import (
	"encoding/json"
	"strings"
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

// Strings and arrays under any token-ish key stay masked; only numbers,
// booleans and the explicit allow-list are shown.
func TestCCMaskMasksStringsAndArraysUnderTokenKeys(t *testing.T) {
	masked := map[string]interface{}{
		"access_tokens": []interface{}{"a1", "a2"}, "refresh_tokens": []interface{}{"r1"}, "bot_tokens": "b1",
		"tokens": []interface{}{"t1"}, "githubTokens": []interface{}{"g1"}, "endpoint_token": "e1", "usage_token": "u1",
		"token_endpoint": "https://idp.example/token", "input_tokens": "sk-123", "context_tokens": []interface{}{"x"},
		"token": "t", "bot_token": "b",
	}
	visible := map[string]interface{}{
		"max_tokens": "4096", "min_tokens": 1, "maxTokens": 8192, "minTokens": 1, "model_token_limit": "200k", "daily_token_count": 7,
		"token_type": "Bearer", "tokenType": "Bearer", "input_tokens": 12, "tokens_used": float64(3), "include_tokens": true,
		"usage_tokens": 5, "enabled": true, "name": "chan",
	}
	cfg := map[string]interface{}{}
	for k, v := range masked {
		cfg[k] = v
	}
	for k, v := range visible {
		if _, dup := cfg[k]; !dup {
			cfg[k] = v
		}
	}
	raw, _ := json.Marshal(cfg)
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(maskCCConfig(string(raw))), &got); err != nil {
		t.Fatal(err)
	}
	for k := range masked {
		if got[k] != ccSecretMask {
			t.Errorf("%s must be masked, got %v", k, got[k])
		}
	}
	for k, want := range visible {
		if _, dup := masked[k]; dup {
			continue
		}
		wantJSON, _ := json.Marshal(want)
		gotJSON, _ := json.Marshal(got[k])
		if string(wantJSON) != string(gotJSON) {
			t.Errorf("%s must stay visible as %s, got %s", k, wantJSON, gotJSON)
		}
	}
	// A masked value sent back is restored, not stored literally.
	stored := `{"access_tokens":["real1"],"max_tokens":10}`
	restored := restoreCCMaskedConfig(`{"access_tokens":"`+ccSecretMask+`","max_tokens":10}`, stored)
	if strings.Contains(restored, ccSecretMask) {
		t.Errorf("mask leaked into stored config: %s", restored)
	}
}
