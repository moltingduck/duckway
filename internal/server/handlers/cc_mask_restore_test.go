package handlers

import "testing"

func TestRestoreCCMaskedConfigUnparseableStoredDropsMask(t *testing.T) {
	got := restoreCCMaskedConfig(`{"bot_token":"********","name":"x"}`, `not json`)
	if got != `{"name":"x"}` {
		t.Fatalf("got %s, want mask dropped and name kept", got)
	}
}

func TestRestoreCCMaskedConfigArrayIsIndexBased(t *testing.T) {
	got := restoreCCMaskedConfig(`{"hooks":[{"secret":"********"}]}`, `{"hooks":[{"secret":"s0"}]}`)
	if got != `{"hooks":[{"secret":"s0"}]}` {
		t.Fatalf("got %s", got)
	}
}

func TestCCIsSecretKeyExtendedHints(t *testing.T) {
	for _, k := range []string{"webhook_url", "dsn", "DSN", "cookie", "Set-Cookie", "auth", "authHeader", "x_auth_token", "passphrase", "pwd", "db_pwd", "accessKey", "access_key", "bot_token"} {
		if !ccIsSecretKey(k) {
			t.Errorf("%q should be secret", k)
		}
	}
	for _, k := range []string{"author", "authority", "guild_id", "category_id", "name", "keyboard", "dsnx"} {
		if ccIsSecretKey(k) {
			t.Errorf("%q should not be secret", k)
		}
	}
}
