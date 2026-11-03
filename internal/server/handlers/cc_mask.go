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
	"webhook", "cookie", "passphrase", "accesskey",
}

// ccSecretKeyWords are short hints matched only as whole word segments, so
// "auth" masks auth/auth_header/authHeader but not "author".
var ccSecretKeyWords = map[string]bool{"auth": true, "dsn": true, "pwd": true}

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

func ccIsSecretKey(key string) bool {
	k := strings.ToLower(key)
	for _, hint := range ccSecretKeyHints {
		if strings.Contains(k, hint) {
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

func maskCCValue(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, val := range t {
			if ccIsSecretKey(k) {
				if s, ok := val.(string); ok && s == "" {
					continue
				}
				if val != nil {
					t[k] = ccSecretMask
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
