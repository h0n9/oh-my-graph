package auth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func testConfig(dir, passphrase string) Config {
	return Config{
		Issuer:            "http://test.local",
		OwnerPassphrase:   passphrase,
		ClientsFile:       filepath.Join(dir, "oauth-clients.json"),
		RefreshTokensFile: filepath.Join(dir, "oauth-refresh-tokens.enc"),
	}
}

func newTestServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	return NewServer(cfg)
}

// noRedirectClient stops at the first redirect so tests can inspect the
// Location header (the authorization code) instead of following it to a
// nonexistent client callback URL.
func noRedirectClient() *http.Client {
	return &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

func registerClient(t *testing.T, baseURL string) (clientID, redirectURI string) {
	t.Helper()
	redirectURI = "http://client.example/callback"
	body, err := json.Marshal(map[string]any{
		"client_name":                "test-client",
		"redirect_uris":              []string{redirectURI},
		"token_endpoint_auth_method": "none",
	})
	if err != nil {
		t.Fatalf("marshal register request: %v", err)
	}
	resp, err := http.Post(baseURL+"/register", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("register: status %d: %s", resp.StatusCode, b)
	}
	var out struct {
		ClientID string `json:"client_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("register: decode response: %v", err)
	}
	return out.ClientID, redirectURI
}

// authorizeAndExchange drives a full authorization_code + PKCE flow against
// a running *Server and returns the resulting access and refresh tokens.
func authorizeAndExchange(t *testing.T, client *http.Client, baseURL, clientID, redirectURI, passphrase string) (accessToken, refreshToken string) {
	t.Helper()
	verifier := oauth2.GenerateVerifier()
	challenge := oauth2.S256ChallengeFromVerifier(verifier)

	form := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {"xyz"},
		"passphrase":            {passphrase},
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+"/authorize", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("build authorize request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("authorize: expected 302, got %d: %s", resp.StatusCode, b)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("authorize: parse Location: %v", err)
	}
	code := loc.Query().Get("code")
	if code == "" {
		t.Fatalf("authorize: no code in redirect: %s", loc)
	}

	tresp, err := http.PostForm(baseURL+"/token", url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {clientID},
		"redirect_uri":  {redirectURI},
		"code":          {code},
		"code_verifier": {verifier},
	})
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	defer tresp.Body.Close()
	if tresp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(tresp.Body)
		t.Fatalf("token: status %d: %s", tresp.StatusCode, b)
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(tresp.Body).Decode(&out); err != nil {
		t.Fatalf("token: decode response: %v", err)
	}
	return out.AccessToken, out.RefreshToken
}

// TestRefreshTokenSurvivesRestart is the regression test for
// blocker-sprites-connector-session-not-persisted: a refresh token issued
// by one Server instance must be redeemable by a brand-new Server instance
// pointed at the same on-disk files, simulating a process restart.
func TestRefreshTokenSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	const passphrase = "correct horse battery staple"
	cfg := testConfig(dir, passphrase)
	client := noRedirectClient()

	srvA := newTestServer(t, cfg)
	muxA := http.NewServeMux()
	srvA.RegisterRoutes(muxA)
	tsA := httptest.NewServer(muxA)

	clientID, redirectURI := registerClient(t, tsA.URL)
	_, refreshToken := authorizeAndExchange(t, client, tsA.URL, clientID, redirectURI, passphrase)
	if refreshToken == "" {
		t.Fatal("expected a non-empty refresh token")
	}
	tsA.Close() // the process is gone; only what's on disk matters now

	srvB := newTestServer(t, cfg) // simulated restart: fresh instance, same files
	muxB := http.NewServeMux()
	srvB.RegisterRoutes(muxB)
	tsB := httptest.NewServer(muxB)
	defer tsB.Close()

	resp, err := http.PostForm(tsB.URL+"/token", url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {clientID},
		"refresh_token": {refreshToken},
	})
	if err != nil {
		t.Fatalf("refresh after restart: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("refresh after restart: status %d: %s", resp.StatusCode, b)
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.AccessToken == "" {
		t.Fatal("expected a new access token after restart")
	}
}

func TestLoadRefreshTokensMissingFileStartsEmpty(t *testing.T) {
	dir := t.TempDir()
	srv := newTestServer(t, testConfig(dir, "pw")) // RefreshTokensFile doesn't exist yet
	if len(srv.refreshTokens) != 0 {
		t.Fatalf("expected empty refreshTokens, got %d", len(srv.refreshTokens))
	}
}

func TestLoadRefreshTokensCorruptFileStartsEmpty(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir, "pw")
	if err := os.WriteFile(cfg.RefreshTokensFile, []byte("not a valid encrypted blob"), 0o600); err != nil {
		t.Fatalf("write corrupt file: %v", err)
	}
	srv := newTestServer(t, cfg) // must not panic
	if len(srv.refreshTokens) != 0 {
		t.Fatalf("expected empty refreshTokens after corrupt load, got %d", len(srv.refreshTokens))
	}
}

func TestLoadRefreshTokensWrongPassphraseStartsEmpty(t *testing.T) {
	dir := t.TempDir()
	srvA := newTestServer(t, testConfig(dir, "passphrase-A"))
	srvA.mu.Lock()
	srvA.refreshTokens["tok-a"] = &refreshToken{ClientID: "c1", ExpiresAt: time.Now().Add(time.Hour)}
	srvA.mu.Unlock()
	srvA.persistRefreshTokens()

	srvB := newTestServer(t, testConfig(dir, "passphrase-B")) // wrong passphrase, same files
	if len(srvB.refreshTokens) != 0 {
		t.Fatalf("expected empty refreshTokens with wrong passphrase, got %d", len(srvB.refreshTokens))
	}
}

func TestLoadRefreshTokensDropsExpired(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir, "pw")
	srv := newTestServer(t, cfg)
	srv.mu.Lock()
	srv.refreshTokens["expired"] = &refreshToken{ClientID: "c1", ExpiresAt: time.Now().Add(-time.Hour)}
	srv.refreshTokens["valid"] = &refreshToken{ClientID: "c1", ExpiresAt: time.Now().Add(time.Hour)}
	srv.mu.Unlock()
	srv.persistRefreshTokens()

	srv2 := newTestServer(t, cfg)
	if _, ok := srv2.refreshTokens["expired"]; ok {
		t.Fatal("expected expired token to be dropped on load")
	}
	if _, ok := srv2.refreshTokens["valid"]; !ok {
		t.Fatal("expected valid token to survive load")
	}
}

func TestPersistedFileIsEncrypted(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir, "pw")
	srv := newTestServer(t, cfg)
	const secretToken = "super-secret-refresh-token-value-xyz"
	srv.mu.Lock()
	srv.refreshTokens[secretToken] = &refreshToken{ClientID: "c1", ExpiresAt: time.Now().Add(time.Hour)}
	srv.mu.Unlock()
	srv.persistRefreshTokens()

	raw, err := os.ReadFile(cfg.RefreshTokensFile)
	if err != nil {
		t.Fatalf("read persisted file: %v", err)
	}
	if bytes.Contains(raw, []byte(secretToken)) {
		t.Fatal("persisted file contains the plaintext refresh token -- it is not encrypted")
	}
}

// TestConcurrentPersistDoesNotLoseTokens guards the race persistRefreshTokens
// deliberately avoids by holding s.mu for the entire marshal+encrypt+write
// (unlike persistClients, which only holds it for the marshal). If that lock
// were released before the file write, concurrent writers could land on disk
// out of order and this test would flake by losing tokens.
func TestConcurrentPersistDoesNotLoseTokens(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir, "pw")
	srv := newTestServer(t, cfg)

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tok := fmt.Sprintf("tok-%d", i)
			srv.mu.Lock()
			srv.refreshTokens[tok] = &refreshToken{ClientID: "c1", ExpiresAt: time.Now().Add(time.Hour)}
			srv.mu.Unlock()
			srv.persistRefreshTokens()
		}(i)
	}
	wg.Wait()

	srv2 := newTestServer(t, cfg) // fresh load from whatever ended up on disk
	if got := len(srv2.refreshTokens); got != n {
		t.Fatalf("expected %d persisted refresh tokens after concurrent writes, got %d", n, got)
	}
}
