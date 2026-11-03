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
