package model

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"fmt"
)

const recoveryTokenBytes = 32

func NewRecoveryToken() ([]byte, error) {
	token := make([]byte, recoveryTokenBytes)
	if _, err := rand.Read(token); err != nil {
		return nil, fmt.Errorf("generate recovery token: %w", err)
	}
	return token, nil
}

func RecoveryVerifier(sessionID SessionID, generation uint64, token []byte) []byte {
	h := sha256.New()
	_, _ = h.Write([]byte("ducklion-supervisor-recovery-v1\x00"))
	_, _ = h.Write([]byte(sessionID))
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], generation)
	_, _ = h.Write(encoded[:])
	_, _ = h.Write(token)
	return h.Sum(nil)
}

func VerifyRecoveryToken(sessionID SessionID, generation uint64, token, expectedVerifier []byte) bool {
	actual := RecoveryVerifier(sessionID, generation, token)
	return len(actual) == len(expectedVerifier) && subtle.ConstantTimeCompare(actual, expectedVerifier) == 1
}
