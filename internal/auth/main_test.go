package auth

import (
	"bytes"
	"os"
	"sync"
	"testing"
)

// realNewSalt is the production salt source, saved before TestMain pins it.
var realNewSalt func() ([]byte, error)

// TestMain makes server construction cheap in tests. Deriving the file key is
// deliberately slow (600k PBKDF2 iterations), and every test server that has a
// refresh-token file derives one. A fresh server picks a random salt, so a
// plain cache would never hit; pinning the salt and memoizing the derivation
// means the cost is paid once per distinct passphrase.
//
// Only the server's hooks are replaced. Tests that call deriveKey directly
// (crypto_test.go and the old-format file test) still run the real derivation.
// The fixed salt hides salt-handling bugs from every other test, so a test
// about salts must write its own random salt to the file, as the old-format
// test does.
func TestMain(m *testing.M) {
	realNewSalt = newSalt
	fixedSalt := []byte("0123456789abcdef")
	newSalt = func() ([]byte, error) { return append([]byte(nil), fixedSalt...), nil }

	type cacheKey struct{ passphrase, salt string }
	var (
		mu    sync.Mutex
		cache = map[cacheKey][]byte{}
	)
	deriveKeyFunc = func(passphrase string, salt []byte) ([]byte, error) {
		k := cacheKey{passphrase, string(salt)}
		mu.Lock()
		defer mu.Unlock()
		if key, ok := cache[k]; ok {
			return key, nil
		}
		key, err := deriveKey(passphrase, salt)
		if err != nil {
			return nil, err
		}
		cache[k] = key
		return key, nil
	}
	os.Exit(m.Run())
}

func TestRealNewSalt(t *testing.T) {
	a, err := realNewSalt()
	if err != nil {
		t.Fatal(err)
	}
	b, err := realNewSalt()
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != saltSize || len(b) != saltSize {
		t.Fatalf("salt sizes = %d, %d, want %d", len(a), len(b), saltSize)
	}
	if bytes.Equal(a, b) {
		t.Fatal("two salts are identical")
	}
}
