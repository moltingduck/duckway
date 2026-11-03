package handlers

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRedactSubscriptionInfoDropsSecretFields(t *testing.T) {
	in := `{"subscriptionType":"max","id_token":"AAA","accessToken":"BBB","client_secret":"CCC","token_type":"Bearer","scopes":["a"],"nested":{"refresh_token":"DDD","ok":1},"list":[{"private_key":"EEE","name":"n"}]}`
	out := redactSubscriptionInfo(in)
	for _, leaked := range []string{"AAA", "BBB", "CCC", "DDD", "EEE"} {
		if strings.Contains(out, leaked) {
			t.Fatalf("redacted JSON still contains %s: %s", leaked, out)
		}
	}
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got["subscriptionType"] != "max" || got["token_type"] != "Bearer" {
		t.Fatalf("non-secret fields lost: %s", out)
	}
	if redactSubscriptionInfo("not json with secret") != "" || redactSubscriptionInfo("") != "" {
		t.Fatal("unparseable input must be dropped")
	}
}

func TestMergeStoredSubscriptionSecrets(t *testing.T) {
	stored := `{"id_token":"KEEP","subscriptionType":"max"}`
	merged := mergeStoredSubscriptionSecrets(stored, `{"subscriptionType":"pro"}`)
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(merged), &got); err != nil {
		t.Fatal(err)
	}
	if got["id_token"] != "KEEP" || got["subscriptionType"] != "pro" {
		t.Fatalf("merge = %s", merged)
	}
	if out := mergeStoredSubscriptionSecrets(stored, `{"id_token":"NEW"}`); !strings.Contains(out, "NEW") || strings.Contains(out, "KEEP") {
		t.Fatalf("explicit value must win: %s", out)
	}
	if out := mergeStoredSubscriptionSecrets("", `{"a":1}`); out != `{"a":1}` {
		t.Fatalf("no stored value must be a no-op: %s", out)
	}
}

func TestSecretFieldNameWordMatching(t *testing.T) {
	secrets := []string{
		"authorization", "Authorization", "cookie", "Set-Cookie", "cookies", "session_key", "sessionKey", "session",
		"bearer", "bearerToken", "jwt", "id_jwt", "credential", "credentials", "passphrase", "pem", "private_pem",
		"access_key", "accessKey", "signing_key", "signingKey", "x-api-key", "X-Api-Key", "tokens", "refresh_tokens",
		"refreshTokens", "access_token", "accessToken", "id_token", "client_secret", "private_key", "apiKey",
	}
	for _, n := range secrets {
		if !isSecretFieldName(n) {
			t.Errorf("%q must be treated as secret", n)
		}
	}
	visible := []string{
		"subscriptionType", "rateLimitTier", "scopes", "account_id", "client_id", "clientId", "token_type",
		"tokenType", "token_endpoint", "displayName", "email", "expires_at", "tokens_remaining", "key_preview", "organization",
	}
	for _, n := range visible {
		if isSecretFieldName(n) {
			t.Errorf("%q must stay visible", n)
		}
	}
}

func TestRedactAndMergeRoundTripNewSecretNames(t *testing.T) {
	stored := `{"subscriptionType":"max","session_key":"S1","x-api-key":"S2","refreshTokens":["S3"],"authorization":"S4","scopes":["a"]}`
	red := redactSubscriptionInfo(stored)
	for _, leaked := range []string{"S1", "S2", "S3", "S4"} {
		if strings.Contains(red, leaked) {
			t.Fatalf("leaked %s: %s", leaked, red)
		}
	}
	if !strings.Contains(red, "scopes") || !strings.Contains(red, "subscriptionType") {
		t.Fatalf("metadata lost: %s", red)
	}
	merged := mergeStoredSubscriptionSecrets(stored, red)
	for _, kept := range []string{"S1", "S2", "S3", "S4"} {
		if !strings.Contains(merged, kept) {
			t.Fatalf("stored secret %s not restored: %s", kept, merged)
		}
	}
}
