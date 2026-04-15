package services

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
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
	return strings.Contains(key, placeholderMarker)
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
