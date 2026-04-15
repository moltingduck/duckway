package services

import (
	"crypto/rand"
	"testing"
)

func TestCryptoRoundtrip(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)

	crypto := NewCrypto(key)

	original := "sk-proj-this-is-a-secret-api-key-12345"
	encrypted, err := crypto.Encrypt(original)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	if encrypted == original {
		t.Error("encrypted == original")
	}

	decrypted, err := crypto.Decrypt(encrypted)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}

	if decrypted != original {
		t.Errorf("decrypted %q != original %q", decrypted, original)
	}
}

func TestCryptoDifferentCiphertexts(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)
	crypto := NewCrypto(key)

	e1, _ := crypto.Encrypt("same-plaintext")
	e2, _ := crypto.Encrypt("same-plaintext")

	if e1 == e2 {
		t.Error("same plaintext produced identical ciphertexts (nonce reuse?)")
	}
}

func TestHashToken(t *testing.T) {
	h1 := HashToken("token-abc")
	h2 := HashToken("token-abc")
	h3 := HashToken("token-xyz")

	if h1 != h2 {
		t.Error("same token produced different hashes")
	}
	if h1 == h3 {
		t.Error("different tokens produced same hash")
	}
}
