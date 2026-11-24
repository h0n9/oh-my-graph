package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestIssueTokenPairRejectsUnregisteredClient guards the revoke-vs-grant
// race: a client removed after the grant handler authenticated it must not
// receive (or persist) a fresh token pair.
func TestIssueTokenPairRejectsUnregisteredClient(t *testing.T) {
	srv := newTestServer(t, testConfig(t.TempDir(), "pw"))
	srv.mu.Lock()
	srv.clients["c1"] = &client{ID: "c1", Name: "n", CreatedAt: time.Now()}
	srv.mu.Unlock()

	// Simulate a revoke landing between authenticateClient and issueTokenPair.
	srv.mu.Lock()
	delete(srv.clients, "c1")
	srv.mu.Unlock()

	rec := httptest.NewRecorder()
	srv.issueTokenPair(rec, "c1", "", time.Time{})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 invalid_client, got %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["error"] != "invalid_client" {
		t.Fatalf("expected error=invalid_client, got %v", body["error"])
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.tokens) != 0 || len(srv.refreshTokens) != 0 {
		t.Fatalf("tokens minted for an unregistered client: access=%d refresh=%d", len(srv.tokens), len(srv.refreshTokens))
	}
}

// TestIssueTokenPairConcurrentWithClientRemoval is a -race stress test:
// grants racing a client removal must not corrupt state, and every grant that
// starts after the removal has finished must be rejected and mint nothing.
func TestIssueTokenPairConcurrentWithClientRemoval(t *testing.T) {
	srv := newTestServer(t, testConfig(t.TempDir(), "pw"))
	srv.mu.Lock()
	srv.clients["c1"] = &client{ID: "c1", CreatedAt: time.Now()}
	srv.mu.Unlock()

	removed := make(chan struct{})
	var wg sync.WaitGroup

	// Grants racing the removal: either outcome is legitimate.
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			srv.issueTokenPair(httptest.NewRecorder(), "c1", "", time.Time{})
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		srv.mu.Lock()
		delete(srv.clients, "c1")
		srv.mu.Unlock()
		close(removed)
	}()

	// Snapshot token counts once the removal is done, then let late grants
	// run: none may succeed or add a token.
	<-removed
	srv.mu.Lock()
	accessBefore, refreshBefore := len(srv.tokens), len(srv.refreshTokens)
	srv.mu.Unlock()
	late := make(chan int, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			srv.issueTokenPair(rec, "c1", "", time.Time{})
			late <- rec.Code
		}()
	}
	wg.Wait()
	close(late)

	for code := range late {
		if code != http.StatusUnauthorized {
			t.Fatalf("grant after removal should be rejected with 401, got %d", code)
		}
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	// issueTokenPair re-checks the client under the insert lock, so once the
	// removal has happened no grant (racing or late) can add a token: the
	// counts taken right after the removal must be final.
	if len(srv.tokens) != accessBefore || len(srv.refreshTokens) != refreshBefore {
		t.Fatalf("tokens minted after the client was removed: access %d->%d refresh %d->%d",
			accessBefore, len(srv.tokens), refreshBefore, len(srv.refreshTokens))
	}
}

// TestRefreshGrantForRemovedClientIsRejected is an end-to-end regression test
// for the real /token refresh path: a client that is no longer registered must
// get a 401 and no new tokens may be minted. Note it is rejected earlier, in
// authenticateClient; the narrower window between that check and the insert is
// covered by TestIssueTokenPairRejectsUnregisteredClient.
func TestRefreshGrantForRemovedClientIsRejected(t *testing.T) {
	dir := t.TempDir()
	const passphrase = "pw"
	srv := newTestServer(t, testConfig(dir, passphrase))
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	clientID, redirectURI := registerClient(t, ts.URL)
	_, refresh := authorizeAndExchange(t, noRedirectClient(), ts.URL, clientID, redirectURI, passphrase)

	srv.mu.Lock()
	delete(srv.clients, clientID)
	accessBefore, refreshBefore := len(srv.tokens), len(srv.refreshTokens)
	srv.mu.Unlock()

	resp, err := http.PostForm(ts.URL+"/token", url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {clientID},
		"refresh_token": {refresh},
	})
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for a removed client, got %d", resp.StatusCode)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.tokens) != accessBefore || len(srv.refreshTokens) != refreshBefore {
		t.Fatalf("tokens changed for a removed client: access %d->%d refresh %d->%d",
			accessBefore, len(srv.tokens), refreshBefore, len(srv.refreshTokens))
	}
}

// TestPersistRefreshTokensWithoutKeyReportsError: when a file is configured
// but no key exists, nothing is written and the caller must be told.
func TestPersistRefreshTokensWithoutKeyReportsError(t *testing.T) {
	srv := newTestServer(t, testConfig(t.TempDir(), "pw"))
	srv.encKey = nil
	if err := srv.persistRefreshTokens(); !errors.Is(err, errNoEncryptionKey) {
		t.Fatalf("expected errNoEncryptionKey, got %v", err)
	}
}

// TestWriteFileAtomicCleansUpTempOnFailure: a failed rename must not leave the
// temp file behind.
func TestWriteFileAtomicCleansUpTempOnFailure(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o700); err != nil { // renaming a file over a directory fails
		t.Fatal(err)
	}
	if err := writeFileAtomic(target, []byte("x")); err == nil {
		t.Fatal("expected an error renaming over a directory")
	}
	if _, err := os.Stat(target + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind after failure: %v", err)
	}
}

// TestPersistReturnsError checks both persist functions report failures
// instead of swallowing them, so revoke/prune can surface a revocation that
// did not reach disk.
func TestPersistReturnsError(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A path *under* a regular file can never be created.
	cfg := testConfig(dir, "pw")
	cfg.ClientsFile = filepath.Join(blocker, "clients.json")
	cfg.RefreshTokensFile = filepath.Join(blocker, "refresh.enc")
	srv := newTestServer(t, cfg)

	if err := srv.persistClients(); err == nil {
		t.Error("persistClients: expected an error for an unwritable path")
	}
	if err := srv.persistRefreshTokens(); err == nil {
		t.Error("persistRefreshTokens: expected an error for an unwritable path")
	}
}

func TestPersistDisabledReturnsNil(t *testing.T) {
	srv := newTestServer(t, Config{Issuer: "http://test.local", OwnerPassphrase: "pw"})
	if err := srv.persistClients(); err != nil {
		t.Errorf("persistClients with no file: %v", err)
	}
	if err := srv.persistRefreshTokens(); err != nil {
		t.Errorf("persistRefreshTokens with no file: %v", err)
	}
}

// TestConcurrentPersistClientsKeepsLatestSnapshot guards persistClients
// holding s.mu for the whole write: concurrent writers must never leave an
// older snapshot on disk (which could resurrect a removed client).
func TestConcurrentPersistClientsKeepsLatestSnapshot(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir, "pw")
	srv := newTestServer(t, cfg)

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("c-%d", i)
			srv.mu.Lock()
			srv.clients[id] = &client{ID: id, CreatedAt: time.Now()}
			srv.mu.Unlock()
			if err := srv.persistClients(); err != nil {
				t.Errorf("persistClients: %v", err)
			}
		}(i)
	}
	wg.Wait()

	srv2 := newTestServer(t, cfg)
	if got := len(srv2.clients); got != n {
		t.Fatalf("expected %d persisted clients after concurrent writes, got %d", n, got)
	}
}
