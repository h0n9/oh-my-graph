package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
)

const (
	pbkdf2Iterations = 600_000
	saltSize         = 16
	aesKeySize       = 32 // AES-256
)

// deriveKey runs PBKDF2-HMAC-SHA256 over passphrase and salt to produce an
// AES-256 key. This is expensive by design (tens to hundreds of ms) to slow
// down offline brute-force against a stolen file -- callers must derive it
// once per (passphrase, salt) pair and cache the result; never call this
// per-request.
func deriveKey(passphrase string, salt []byte) ([]byte, error) {
	return pbkdf2.Key(sha256.New, passphrase, salt, pbkdf2Iterations, aesKeySize)
}

// encryptBlob encrypts plaintext with AES-256-GCM under key, binding aad,
// with a fresh random nonce. Returns nonce || ciphertext+tag.
func encryptBlob(key, aad, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("read nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, plaintext, aad), nil
}

// decryptBlob reverses encryptBlob; returns an error on auth-tag mismatch
// (wrong key, wrong aad, or corruption). The error is always the opaque
// stdlib AEAD error -- never include key/passphrase material when wrapping
// or logging it.
func decryptBlob(key, aad, blob []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new gcm: %w", err)
	}
	if len(blob) < gcm.NonceSize() {
		return nil, fmt.Errorf("blob too short")
	}
	nonce, ciphertext := blob[:gcm.NonceSize()], blob[gcm.NonceSize():]
	return gcm.Open(nil, nonce, ciphertext, aad)
}
