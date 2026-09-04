package model

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
)

const RecoveryNonceBytes = 32

func NewRecoveryKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}

func NewRecoveryNonce() ([]byte, error) {
	nonce := make([]byte, RecoveryNonceBytes)
	_, err := rand.Read(nonce)
	return nonce, err
}

func RecoveryProof(privateKey ed25519.PrivateKey, instanceID InstanceID, sessionID SessionID, generation uint64, nonce []byte, protocolMajor, protocolMinor uint16) []byte {
	return ed25519.Sign(privateKey, recoveryMessage(instanceID, sessionID, generation, nonce, protocolMajor, protocolMinor))
}

func VerifyRecoveryProof(publicKey ed25519.PublicKey, instanceID InstanceID, sessionID SessionID, generation uint64, nonce, proof []byte, protocolMajor, protocolMinor uint16) bool {
	if len(publicKey) != ed25519.PublicKeySize || len(nonce) != RecoveryNonceBytes || len(proof) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(publicKey, recoveryMessage(instanceID, sessionID, generation, nonce, protocolMajor, protocolMinor), proof)
}

func recoveryMessage(instanceID InstanceID, sessionID SessionID, generation uint64, nonce []byte, protocolMajor, protocolMinor uint16) []byte {
	message := make([]byte, 0, 64+len(instanceID)+len(sessionID)+len(nonce))
	message = append(message, "ducklion-supervisor-recovery-v1\x00"...)
	message = append(message, instanceID...)
	message = append(message, 0)
	message = append(message, sessionID...)
	var encoded [12]byte
	binary.BigEndian.PutUint64(encoded[:8], generation)
	binary.BigEndian.PutUint16(encoded[8:10], protocolMajor)
	binary.BigEndian.PutUint16(encoded[10:12], protocolMinor)
	message = append(message, encoded[:]...)
	message = append(message, nonce...)
	return message
}
