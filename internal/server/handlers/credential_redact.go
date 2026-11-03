package handlers

import (
	"encoding/json"
	"strings"
	"unicode"

	"github.com/hackerduck/duckway/internal/models"
)

// Stored upstream secrets never reach the browser. Refreshable credentials keep
// provider metadata (including the OpenID id_token copied from a Codex
// auth.json) in subscription_info; the admin API strips every secret-looking
// field from that JSON and merges the stored values back on update so an
// edit round-trip cannot erase them.

// visibleMetadataFields are provider metadata names (separators removed,
// lower-cased) that look secret-like but never hold a secret.
var visibleMetadataFields = map[string]bool{
	"tokentype": true, "tokenendpoint": true, "tokensremaining": true, "tokenslimit": true,
	"subscriptiontype": true, "ratelimittier": true, "scopes": true, "accountid": true, "clientid": true,
}

// secretWords mark a field secret when they appear as a whole word in a
// snake_case, kebab-case or camelCase name.
var secretWords = map[string]bool{
	"authorization": true, "cookie": true, "cookies": true, "session": true, "bearer": true, "jwt": true,
	"credential": true, "credentials": true, "passphrase": true, "pem": true, "secret": true, "secrets": true,
	"password": true, "passwd": true,
}

// nameWords splits snake, kebab, dotted and camelCase names into lower-case words.
func nameWords(name string) []string {
	var words []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			words = append(words, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	rs := []rune(name)
	for i, r := range rs {
		switch {
		case r == '_' || r == '-' || r == '.' || r == ' ':
			flush()
		case unicode.IsUpper(r):
			prevLower := i > 0 && (unicode.IsLower(rs[i-1]) || unicode.IsDigit(rs[i-1]))
			acronymEnd := i > 0 && unicode.IsUpper(rs[i-1]) && i+1 < len(rs) && unicode.IsLower(rs[i+1])
			if prevLower || acronymEnd {
				flush()
			}
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
	}
	flush()
	return words
}

func isSecretFieldName(name string) bool {
	n := strings.ToLower(name)
	flat := strings.NewReplacer("_", "", "-", "", ".", "", " ", "").Replace(n)
	if visibleMetadataFields[flat] {
		return false
	}
	words := nameWords(name)
	for _, w := range words {
		if secretWords[w] {
			return true
		}
	}
	if len(words) > 0 {
		switch words[len(words)-1] {
		case "token", "tokens", "key", "keys":
			return true
		}
	}
	return strings.HasSuffix(n, "token") ||
		strings.Contains(n, "secret") ||
		strings.Contains(n, "password") ||
		strings.Contains(n, "private_key") ||
		strings.Contains(n, "privatekey") ||
		strings.Contains(n, "api_key") ||
		strings.Contains(n, "apikey")
}

func redactSecretValue(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, child := range t {
			if isSecretFieldName(k) {
				delete(t, k)
				continue
			}
			t[k] = redactSecretValue(child)
		}
		return t
	case []interface{}:
		for i := range t {
			t[i] = redactSecretValue(t[i])
		}
		return t
	default:
		return v
	}
}

// redactSubscriptionInfo returns raw with all secret-looking fields removed.
// Non-JSON input that is not empty is dropped entirely: it cannot be proven
// free of secrets.
func redactSubscriptionInfo(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var parsed interface{}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return ""
	}
	obj, ok := parsed.(map[string]interface{})
	if !ok {
		return ""
	}
	redactSecretValue(obj)
	out, err := json.Marshal(obj)
	if err != nil {
		return ""
	}
	return string(out)
}

// mergeStoredSubscriptionSecrets copies top-level secret fields that exist in
// stored but are absent from incoming into incoming. Values the caller sends
// explicitly win. If either side is unparseable incoming is returned as-is.
func mergeStoredSubscriptionSecrets(stored, incoming string) string {
	var storedObj, incomingObj map[string]interface{}
	if strings.TrimSpace(stored) == "" || json.Unmarshal([]byte(stored), &storedObj) != nil {
		return incoming
	}
	if strings.TrimSpace(incoming) == "" {
		incomingObj = map[string]interface{}{}
	} else if json.Unmarshal([]byte(incoming), &incomingObj) != nil || incomingObj == nil {
		return incoming
	}
	changed := false
	for k, v := range storedObj {
		if !isSecretFieldName(k) {
			continue
		}
		if _, present := incomingObj[k]; present {
			continue
		}
		incomingObj[k] = v
		changed = true
	}
	if !changed {
		return incoming
	}
	out, err := json.Marshal(incomingObj)
	if err != nil {
		return incoming
	}
	return string(out)
}

// sanitizeAPIKeyForAdmin strips every stored secret from a key row before it is
// serialized to the admin browser.
func sanitizeAPIKeyForAdmin(k *models.APIKey) {
	k.KeyEncrypted = ""
	k.RefreshToken = ""
	k.SubscriptionInfo = redactSubscriptionInfo(k.SubscriptionInfo)
}
