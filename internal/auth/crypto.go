package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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

// hashToken returns the hex-encoded SHA-256 digest of token, used as the
// refreshTokens map/persisted-file key instead of the raw token value --
// defense in depth so that a leaked persisted file or a memory-only leak
// (core dump, swap, host-level inspection) yields only hashes, not directly
// usable credentials. A plain unkeyed hash is sufficient here (no salt or
// HMAC needed, unlike password hashing): refresh tokens are uniformly
// random 256-bit values from randomToken(32), not low-entropy/guessable
// secrets, so neither dictionary nor rainbow-table attacks apply -- SHA-256
// preimage resistance alone makes reversing the hash infeasible.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
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
