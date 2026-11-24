package auth

import (
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"
)

// onlyRefreshToken returns the single stored refresh token, failing if there
// is not exactly one.
func onlyRefreshToken(t *testing.T, srv *Server) (string, *refreshToken) {
	t.Helper()
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.refreshTokens) != 1 {
		t.Fatalf("expected exactly 1 refresh token, got %d", len(srv.refreshTokens))
	}
	for k, v := range srv.refreshTokens {
		cp := *v
		return k, &cp
	}
	return "", nil
}

func refreshGrant(t *testing.T, baseURL, clientID, refresh string) {
	t.Helper()
	resp, err := http.PostForm(baseURL+"/token", url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {clientID},
		"refresh_token": {refresh},
	})
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("refresh: status %d", resp.StatusCode)
	}
}

func newTestHTTPServer(t *testing.T, passphrase string) (*Server, *httptest.Server) {
	t.Helper()
	srv := newTestServer(t, testConfig(t.TempDir(), passphrase))
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return srv, ts
}

func TestFirstGrantSetsConnectionMetadata(t *testing.T) {
	srv, ts := newTestHTTPServer(t, "pw")
	before := time.Now()
	clientID, redirectURI := registerClient(t, ts.URL)
	authorizeAndExchange(t, noRedirectClient(), ts.URL, clientID, redirectURI, "pw")

	_, rt := onlyRefreshToken(t, srv)
	if rt.ConnectedAt.Before(before) || rt.IssuedAt.Before(before) || rt.LastUsedAt.Before(before) {
		t.Fatalf("metadata not stamped on first grant: %+v", rt)
	}
	if !rt.ConnectedAt.Equal(rt.IssuedAt) {
		t.Fatalf("first grant: ConnectedAt %v should equal IssuedAt %v", rt.ConnectedAt, rt.IssuedAt)
	}
}

func TestRotationPreservesConnectedAt(t *testing.T) {
	srv, ts := newTestHTTPServer(t, "pw")
	clientID, redirectURI := registerClient(t, ts.URL)
	_, refresh := authorizeAndExchange(t, noRedirectClient(), ts.URL, clientID, redirectURI, "pw")

	_, first := onlyRefreshToken(t, srv)
	beforeRefresh := time.Now()
	refreshGrant(t, ts.URL, clientID, refresh)

	_, second := onlyRefreshToken(t, srv)
	if !second.ConnectedAt.Equal(first.ConnectedAt) {
		t.Fatalf("ConnectedAt changed across rotation: %v -> %v", first.ConnectedAt, second.ConnectedAt)
	}
	if second.IssuedAt.Before(beforeRefresh) || second.LastUsedAt.Before(beforeRefresh) {
		t.Fatalf("IssuedAt/LastUsedAt not refreshed on rotation (grant started %v): %+v", beforeRefresh, second)
	}
}

// Tokens persisted before ConnectedAt existed decode it as zero; rotation must
// fall back to the client's registration time instead of staying unknown.
func TestRotationFallsBackToClientCreatedAt(t *testing.T) {
	srv, ts := newTestHTTPServer(t, "pw")
	clientID, redirectURI := registerClient(t, ts.URL)
	_, refresh := authorizeAndExchange(t, noRedirectClient(), ts.URL, clientID, redirectURI, "pw")

	key, _ := onlyRefreshToken(t, srv)
	srv.mu.Lock()
	srv.refreshTokens[key].ConnectedAt = time.Time{} // simulate a legacy token
	createdAt := srv.clients[clientID].CreatedAt
	srv.mu.Unlock()

	refreshGrant(t, ts.URL, clientID, refresh)
	_, rotated := onlyRefreshToken(t, srv)
	if !rotated.ConnectedAt.Equal(createdAt) {
		t.Fatalf("expected ConnectedAt to fall back to client CreatedAt %v, got %v", createdAt, rotated.ConnectedAt)
	}
}

// A refresh-token file written before the new fields existed must still load,
// with the new fields decoding as zero ("unknown"), without a format bump.
func TestLoadOldFormatRefreshTokenFile(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir, "pw")

	legacy := map[string]map[string]any{
		"legacyhash": {
			"ClientID":  "c1",
			"Scope":     "mcp",
			"ExpiresAt": time.Now().Add(time.Hour).Format(time.RFC3339Nano),
		},
	}
	plaintext, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	salt := make([]byte, saltSize)
	if _, err := rand.Read(salt); err != nil {
		t.Fatal(err)
	}
	key, err := deriveKey("pw", salt)
	if err != nil {
		t.Fatal(err)
	}
	ct, err := encryptBlob(key, refreshTokensAAD, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.RefreshTokensFile, append(salt, ct...), 0o600); err != nil {
		t.Fatal(err)
	}

	srv := newTestServer(t, cfg)
	srv.mu.Lock()
	defer srv.mu.Unlock()
	rt, ok := srv.refreshTokens["legacyhash"]
	if !ok {
		t.Fatal("legacy refresh token did not load")
	}
	if rt.ClientID != "c1" || rt.Scope != "mcp" {
		t.Fatalf("legacy fields lost: %+v", rt)
	}
	if !rt.ConnectedAt.IsZero() || !rt.IssuedAt.IsZero() || !rt.LastUsedAt.IsZero() {
		t.Fatalf("new fields should decode as zero for legacy tokens: %+v", rt)
	}
}

func bearerRequest(t *testing.T, h http.Handler, token string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestRequireBearerRecordsLastSeenOnlyForValidTokens(t *testing.T) {
	srv := newTestServer(t, Config{Issuer: "http://test.local", OwnerPassphrase: "pw"})
	srv.mu.Lock()
	srv.tokens["good"] = &accessToken{ClientID: "c-good", ExpiresAt: time.Now().Add(time.Hour)}
	srv.tokens["old"] = &accessToken{ClientID: "c-old", ExpiresAt: time.Now().Add(-time.Hour)}
	srv.mu.Unlock()

	h := srv.RequireBearer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	before := time.Now()
	if code := bearerRequest(t, h, "good"); code != http.StatusOK {
		t.Fatalf("valid token: got %d", code)
	}
	if code := bearerRequest(t, h, "old"); code != http.StatusUnauthorized {
		t.Fatalf("expired token: got %d", code)
	}
	if code := bearerRequest(t, h, "nope"); code != http.StatusUnauthorized {
		t.Fatalf("unknown token: got %d", code)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if seen, ok := srv.lastSeen["c-good"]; !ok || seen.Before(before) {
		t.Fatalf("lastSeen not recorded for the valid token: %v", seen)
	}
	if _, ok := srv.lastSeen["c-old"]; ok {
		t.Fatal("lastSeen recorded for an expired token")
	}
	if len(srv.lastSeen) != 1 {
		t.Fatalf("unexpected lastSeen entries: %v", srv.lastSeen)
	}
}
