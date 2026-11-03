package handlers

import (
	"encoding/json"
	"strings"
	"unicode"

	"github.com/hackerduck/duckway/internal/models"
)

// ccSecretMask replaces secret-looking config values in admin API
// responses. The browser never receives the stored value; sending the mask
// back on update keeps the stored value unchanged.
const ccSecretMask = "********"

var ccSecretKeyHints = []string{
	"token", "secret", "password", "passwd", "apikey", "api_key",
	"credential", "authorization", "bearer", "private", "signing",
	"webhook", "cookie", "passphrase", "accesskey", "passcode", "sessionid",
}

// ccSecretKeyWords are short hints matched only as whole word segments, so
// "auth" masks auth/auth_header/authHeader but not "author".
var ccSecretKeyWords = map[string]bool{"auth": true, "dsn": true, "pwd": true, "pin": true, "otp": true, "pass": true}

// ccKeySegments splits a key into lowercase words on non-alphanumerics and
// camelCase boundaries.
func ccKeySegments(key string) []string {
	var segs []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			segs = append(segs, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	rs := []rune(key)
	for i, r := range rs {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if unicode.IsUpper(r) && i > 0 && unicode.IsLower(rs[i-1]) {
				flush()
			}
			cur = append(cur, r)
		default:
			flush()
		}
	}
	flush()
	return segs
}

// ccPlainTokenKeys are the only token-ish keys that stay visible even when the
// value is not a number or boolean: descriptive settings, not credentials.
var ccPlainTokenKeys = map[string]bool{
	"max_tokens": true, "maxtokens": true, "min_tokens": true, "mintokens": true, "token_type": true, "tokentype": true,
}

// ccIsPlainTokenKey reports whether key is on the explicit allow-list:
// max_tokens, min_tokens, token_type and *_token_limit / *_token_count.
func ccIsPlainTokenKey(key string) bool {
	k := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
	if ccPlainTokenKeys[k] {
		return true
	}
	return strings.HasSuffix(k, "_token_limit") || strings.HasSuffix(k, "_token_count") ||
		strings.HasSuffix(k, "tokenlimit") || strings.HasSuffix(k, "tokencount")
}

var ccCountWords = map[string]bool{
	"max": true, "min": true, "limit": true, "limits": true, "count": true, "budget": true,
	"ttl": true, "total": true, "remaining": true, "used": true,
}

// ccIsCountStyleKey reports whether a key that mentions "token" names a token
// QUANTITY (input_tokens, max_tokens, token_budget, tokenLimit, tokens_used),
// as opposed to a credential (access_token, usage_token, endpoint_token).
func ccIsCountStyleKey(key string) bool {
	segs := ccKeySegments(key)
	hasToken := false
	for _, s := range segs {
		if s == "tokens" || s == "maxtokens" || s == "mintokens" {
			return true
		}
		if s == "token" {
			hasToken = true
		}
	}
	if !hasToken {
		return false
	}
	for _, s := range segs {
		if ccCountWords[s] {
			return true
		}
	}
	return false
}

// ccSecretBesidesToken reports whether the key looks secret for a reason other
// than the word "token" (password, secret, pin, otp, webhook, key ...).
func ccSecretBesidesToken(key string) bool {
	k := strings.ToLower(key)
	for _, hint := range ccSecretKeyHints {
		if hint != "token" && strings.Contains(k, hint) {
			return true
		}
	}
	for _, seg := range ccKeySegments(key) {
		if ccSecretKeyWords[seg] {
			return true
		}
	}
	return strings.HasSuffix(k, "_key") || k == "key"
}

// ccShouldMask decides whether the value under key is replaced by the mask.
// A secret-looking key (anything mentioning token, secret, password, ...) is
// masked, except: a number or boolean (a token count, never a credential) and
// the explicit allow-list above. Strings, arrays and objects under any
// token-ish key stay masked (access_tokens, refresh_tokens, bot_tokens, tokens,
// githubTokens, endpoint_token, usage_token ...).
func ccShouldMask(key string, val interface{}) bool {
	if val == nil || !ccIsSecretKey(key) {
		return false
	}
	switch val.(type) {
	case float64, bool, json.Number, int, int64:
		// A number or boolean stays visible only for a token-QUANTITY key. Under
		// password, secret, pin, otp, webhook, key, access_token ... a numeric
		// value is still a credential (a numeric PIN or OTP) and stays masked.
		return (!ccIsCountStyleKey(key) || ccSecretBesidesToken(key)) && !ccIsPlainTokenKey(key)
	}
	if s, ok := val.(string); ok && s == "" {
		return false
	}
	return !ccIsPlainTokenKey(key)
}

func ccIsSecretKey(key string) bool {
	k := strings.ToLower(key)
	for _, hint := range ccSecretKeyHints {
		if !strings.Contains(k, hint) {
			continue
		}
		return true
	}
	for _, seg := range ccKeySegments(key) {
		if ccSecretKeyWords[seg] {
			return true
		}
	}
	return strings.HasSuffix(k, "_key") || k == "key"
}

func maskCCValue(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, val := range t {
			if ccIsSecretKey(k) {
				if ccShouldMask(k, val) {
					t[k] = ccSecretMask
				} else if _, nested := val.(map[string]interface{}); nested {
					t[k] = maskCCValue(val)
				}
				continue
			}
			t[k] = maskCCValue(val)
		}
	case []interface{}:
		for i, val := range t {
			t[i] = maskCCValue(val)
		}
	}
	return v
}

// maskCCConfig returns the config JSON with secret-looking values masked.
// Unparseable config is returned as "{}" rather than leaked verbatim.
func maskCCConfig(config string) string {
	if strings.TrimSpace(config) == "" {
		return "{}"
	}
	var cfg interface{}
	if err := json.Unmarshal([]byte(config), &cfg); err != nil {
		return "{}"
	}
	out, err := json.Marshal(maskCCValue(cfg))
	if err != nil {
		return "{}"
	}
	return string(out)
}

// maskCC masks the config of a control channel in place.
func maskCC(c *models.ControlChannel) {
	if c != nil {
		c.Config = maskCCConfig(c.Config)
	}
}

// restoreCCMasked puts stored secrets back in place of mask placeholders.
// Limitation: objects are matched by key, but arrays are matched by index, so
// if a client reorders or removes array elements that contain masked secrets
// the restored values follow the new positions (a mask with no stored value at
// that index is dropped, never sent through as a literal secret).
func restoreCCMasked(incoming, stored interface{}) interface{} {
	switch in := incoming.(type) {
	case map[string]interface{}:
		st, _ := stored.(map[string]interface{})
		for k, val := range in {
			old, has := st[k]
			if s, ok := val.(string); ok && s == ccSecretMask && ccIsSecretKey(k) {
				if has {
					in[k] = old
				} else {
					delete(in, k)
				}
				continue
			}
			in[k] = restoreCCMasked(val, old)
		}
	case []interface{}:
		st, _ := stored.([]interface{})
		for i, val := range in {
			var old interface{}
			if i < len(st) {
				old = st[i]
			}
			in[i] = restoreCCMasked(val, old)
		}
	}
	return incoming
}

// restoreCCMaskedConfig replaces mask placeholders in an incoming config
// with the stored values at the same path.
func restoreCCMaskedConfig(incoming, stored string) string {
	var in, st interface{}
	if err := json.Unmarshal([]byte(incoming), &in); err != nil {
		return incoming
	}
	_ = json.Unmarshal([]byte(stored), &st)
	out, err := json.Marshal(restoreCCMasked(in, st))
	if err != nil {
		return incoming
	}
	return string(out)
}
