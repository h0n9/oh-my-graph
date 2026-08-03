package auth

import (
	"bytes"
	"testing"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	salt := []byte("0123456789abcdef")
	key, err := deriveKey("correct horse battery staple", salt)
	if err != nil {
		t.Fatalf("deriveKey: %v", err)
	}
	aad := []byte("test-aad")
	plaintext := []byte(`{"hello":"world"}`)

	blob, err := encryptBlob(key, aad, plaintext)
	if err != nil {
		t.Fatalf("encryptBlob: %v", err)
	}
	got, err := decryptBlob(key, aad, blob)
	if err != nil {
		t.Fatalf("decryptBlob: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("round trip mismatch: got %q, want %q", got, plaintext)
	}
}

func TestDecryptWrongKeyFails(t *testing.T) {
	salt := []byte("0123456789abcdef")
	key1, _ := deriveKey("passphrase-one", salt)
	key2, _ := deriveKey("passphrase-two", salt)
	aad := []byte("test-aad")

	blob, err := encryptBlob(key1, aad, []byte("secret"))
	if err != nil {
		t.Fatalf("encryptBlob: %v", err)
	}
	if _, err := decryptBlob(key2, aad, blob); err == nil {
		t.Fatal("decryptBlob: expected error with wrong key, got nil")
	}
}

func TestDecryptWrongAADFails(t *testing.T) {
	salt := []byte("0123456789abcdef")
	key, _ := deriveKey("passphrase", salt)

	blob, err := encryptBlob(key, []byte("aad-v1"), []byte("secret"))
	if err != nil {
		t.Fatalf("encryptBlob: %v", err)
	}
	if _, err := decryptBlob(key, []byte("aad-v2"), blob); err == nil {
		t.Fatal("decryptBlob: expected error with wrong aad, got nil")
	}
}

func TestDecryptCorruptedBlobFails(t *testing.T) {
	salt := []byte("0123456789abcdef")
	key, _ := deriveKey("passphrase", salt)
	aad := []byte("test-aad")

	blob, err := encryptBlob(key, aad, []byte("secret"))
	if err != nil {
		t.Fatalf("encryptBlob: %v", err)
	}

	t.Run("truncated", func(t *testing.T) {
		if _, err := decryptBlob(key, aad, blob[:len(blob)-1]); err == nil {
			t.Fatal("expected error decrypting truncated blob, got nil")
		}
	})

	t.Run("flipped byte", func(t *testing.T) {
		corrupted := append([]byte{}, blob...)
		corrupted[len(corrupted)-1] ^= 0xFF
		if _, err := decryptBlob(key, aad, corrupted); err == nil {
			t.Fatal("expected error decrypting corrupted blob, got nil")
		}
	})

	t.Run("too short for nonce", func(t *testing.T) {
		if _, err := decryptBlob(key, aad, []byte{1, 2, 3}); err == nil {
			t.Fatal("expected error decrypting too-short blob, got nil")
		}
	})
}

func TestDeriveKeyDeterministic(t *testing.T) {
	salt := []byte("0123456789abcdef")
	k1, err := deriveKey("same-passphrase", salt)
	if err != nil {
		t.Fatalf("deriveKey: %v", err)
	}
	k2, err := deriveKey("same-passphrase", salt)
	if err != nil {
		t.Fatalf("deriveKey: %v", err)
	}
	if !bytes.Equal(k1, k2) {
		t.Fatal("deriveKey: same passphrase+salt produced different keys")
	}
}

func TestHashTokenDeterministicAndDistinct(t *testing.T) {
	h1 := hashToken("token-a")
	h2 := hashToken("token-a")
	if h1 != h2 {
		t.Fatal("hashToken: same input produced different hashes")
	}
	if h1 == hashToken("token-b") {
		t.Fatal("hashToken: different inputs produced the same hash")
	}
	if h1 == "token-a" {
		t.Fatal("hashToken: returned the raw input unchanged")
	}
}

func TestDeriveKeyDifferentSaltsDiffer(t *testing.T) {
	k1, err := deriveKey("same-passphrase", []byte("0123456789abcdef"))
	if err != nil {
		t.Fatalf("deriveKey: %v", err)
	}
	k2, err := deriveKey("same-passphrase", []byte("fedcba9876543210"))
	if err != nil {
		t.Fatalf("deriveKey: %v", err)
	}
	if bytes.Equal(k1, k2) {
		t.Fatal("deriveKey: different salts produced the same key")
	}
}
