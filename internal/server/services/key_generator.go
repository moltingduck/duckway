package services

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const placeholderMarker = "dw_"

// GeneratePlaceholder creates a placeholder key that mimics the real key format.
// It preserves the prefix (e.g., "sk-", "ghp_") and matches the total length,
// inserting "dw_" as a detection marker.
func GeneratePlaceholder(prefix string, totalLength int) (string, error) {
	marker := placeholderMarker
	prefixLen := len(prefix)
	markerLen := len(marker)

	// Remaining chars to fill with random hex
	remainLen := totalLength - prefixLen - markerLen
	if remainLen < 8 {
		remainLen = 8 // minimum randomness
	}

	randomBytes := make([]byte, (remainLen+1)/2)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", fmt.Errorf("generate random bytes: %w", err)
	}

	randomHex := hex.EncodeToString(randomBytes)

	// Build: prefix + dw_ + random, trimmed to totalLength
	result := prefix + marker + randomHex
	if len(result) > totalLength && totalLength > 0 {
		result = result[:totalLength]
	}

	return result, nil
}

// IsPlaceholder checks if a string looks like a Duckway placeholder key.
func IsPlaceholder(key string) bool {
	if strings.Contains(key, placeholderMarker) {
		return true
	}
	parts := strings.Split(key, ".")
	if len(parts) != 3 {
		return false
	}
	payload := string(decodeJWTPart(parts[1]))
	signature := string(decodeJWTPart(parts[2]))
	return strings.Contains(payload, placeholderMarker) || strings.Contains(signature, placeholderMarker)
}

// DetectKeyFormat returns the prefix and total length to use when generating a
// phantom for a known real key. For services that support multiple token formats
// (e.g. GitHub's ghp_*, github_pat_*, gho_*, ghu_*, ghs_*, ghr_* formats)
// this sniffs the actual key so the phantom matches the real one. Falls back
// to the service's static KeyPrefix/KeyLength when no specific variant is
// detected.
func DetectKeyFormat(realKey, servicePrefix string, serviceLength int) (prefix string, length int) {
	if prefix, ok := detectGitHubTokenPrefix(realKey); ok {
		return prefix, len(realKey)
	}
	return servicePrefix, serviceLength
}

func detectGitHubTokenPrefix(realKey string) (string, bool) {
	for _, prefix := range []string{"github_pat_", "ghp_", "gho_", "ghu_", "ghs_", "ghr_"} {
		if strings.HasPrefix(realKey, prefix) {
			return prefix, true
		}
	}
	return "", false
}

// GeneratePlaceholderForRealKey creates a phantom token that follows the real
// credential's broad shape. OAuth/JWT access tokens stay JWT-shaped so tools
// that inspect token format still accept the phantom, while static API keys use
// the provider's prefix/length rules.
func GeneratePlaceholderForRealKey(realKey, servicePrefix string, serviceLength int) (string, error) {
	if looksLikeJWT(realKey) {
		return generateJWTPlaceholder(realKey)
	}
	prefix, length := DetectKeyFormat(realKey, servicePrefix, serviceLength)
	return GeneratePlaceholder(prefix, length)
}

func looksLikeJWT(token string) bool {
	return len(strings.Split(token, ".")) == 3
}

func generateJWTPlaceholder(source string) (string, error) {
	parts := strings.Split(source, ".")
	header := map[string]interface{}{
		"alg": "RS256",
		"typ": "JWT",
		"kid": "duckway-phantom",
	}
	if len(parts) >= 1 {
		if decoded := decodeJWTPart(parts[0]); len(decoded) > 0 {
			_ = json.Unmarshal(decoded, &header)
		}
	}

	now := time.Now().Unix()
	payload := map[string]interface{}{
		"iss": "duckway",
		"sub": "duckway-phantom",
		"iat": now,
		"exp": now + 24*60*60,
	}
	if len(parts) >= 2 {
		if decoded := decodeJWTPart(parts[1]); len(decoded) > 0 {
			_ = json.Unmarshal(decoded, &payload)
		}
	}
	if exp, ok := jwtNumericClaim(payload["exp"]); !ok || exp <= now {
		payload["exp"] = now + 24*60*60
	}
	payload["jti"] = placeholderMarker + placeholderRandomHex(16)
	payload["sub"] = placeholderMarker + "phantom"

	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(headerJSON) + "." +
		base64.RawURLEncoding.EncodeToString(payloadJSON) + "." +
		base64.RawURLEncoding.EncodeToString([]byte(placeholderMarker+placeholderRandomHex(16))), nil
}

func decodeJWTPart(part string) []byte {
	out, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		return nil
	}
	return out
}

func jwtNumericClaim(v interface{}) (int64, bool) {
	switch x := v.(type) {
	case float64:
		return int64(x), true
	case int64:
		return x, true
	case int:
		return int64(x), true
	default:
		return 0, false
	}
}

func placeholderRandomHex(chars int) string {
	if chars <= 0 {
		return ""
	}
	b := make([]byte, (chars+1)/2)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	s := hex.EncodeToString(b)
	if len(s) > chars {
		return s[:chars]
	}
	return s
}

// GenerateShortID generates a 6-char alphanumeric ID (lowercase + digits).
func GenerateShortID() string {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 6)
	rand.Read(b)
	for i := range b {
		b[i] = charset[int(b[i])%len(charset)]
	}
	return string(b)
}

// GeneratePassword generates a random password for first-run admin setup.
func GeneratePassword(length int) (string, error) {
	const charset = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, length)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = charset[int(b[i])%len(charset)]
	}
	return string(b), nil
}
