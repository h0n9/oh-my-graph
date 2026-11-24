package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// newBareServer returns a Server with persistence disabled (fast: no key
// derivation).
func newBareServer(t *testing.T) *Server {
	t.Helper()
	return newTestServer(t, Config{Issuer: "http://test.local", OwnerPassphrase: "pw"})
}

func addClient(s *Server, id, name string, created time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clients[id] = &client{ID: id, Name: name, CreatedAt: created}
}

func addRefresh(s *Server, hash, clientID string, connected, lastUsed, expires time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshTokens[hash] = &refreshToken{ClientID: clientID, ConnectedAt: connected, IssuedAt: connected, LastUsedAt: lastUsed, ExpiresAt: expires}
}

func connByID(t *testing.T, conns []Connection, id string) Connection {
	t.Helper()
	for _, c := range conns {
		if c.ClientID == id {
			return c
		}
	}
	t.Fatalf("client %q not in list: %+v", id, conns)
	return Connection{}
}

func TestListConnectionsStatuses(t *testing.T) {
	s := newBareServer(t)
	now := time.Now()
	old := now.Add(-100 * 24 * time.Hour)

	addClient(s, "active", "Claude Code", old)
	addRefresh(s, "a1", "active", old, now.Add(-time.Minute), now.Add(30*24*time.Hour))

	addClient(s, "idle", "ChatGPT", old)
	addRefresh(s, "i1", "idle", old, now.Add(-48*time.Hour), now.Add(10*24*time.Hour))

	addClient(s, "expired", "Codex", old)
	addRefresh(s, "e1", "expired", old, old, now.Add(-time.Hour))

	addClient(s, "none", "Newcomer", now)

	addClient(s, "unknown", "Legacy", old)
	addRefresh(s, "u1", "unknown", time.Time{}, time.Time{}, now.Add(24*time.Hour))

	conns := s.ListConnections()
	want := map[string]ConnectionStatus{
		"active": StatusActive, "idle": StatusIdle, "expired": StatusExpired,
		"none": StatusNoTokens, "unknown": StatusActive,
	}
	if len(conns) != len(want) {
		t.Fatalf("expected %d connections, got %d", len(want), len(conns))
	}
	for id, st := range want {
		if got := connByID(t, conns, id).Status; got != st {
			t.Errorf("%s: status %q, want %q", id, got, st)
		}
	}
	if u := connByID(t, conns, "unknown"); !u.LastUsed.IsZero() || !u.ConnectedAt.IsZero() {
		t.Errorf("legacy token should report unknown (zero) times: %+v", u)
	}
	if e := connByID(t, conns, "expired"); !e.SessionExpiresAt.IsZero() || e.ActiveTokens != 0 {
		t.Errorf("expired client should have no live session: %+v", e)
	}
}

func TestListConnectionsAggregatesTokens(t *testing.T) {
	s := newBareServer(t)
	now := time.Now()
	addClient(s, "c", "Multi", now.Add(-time.Hour*500))
	early, late := now.Add(-200*time.Hour), now.Add(-10*time.Hour)
	addRefresh(s, "t1", "c", early, now.Add(-5*time.Hour), now.Add(5*24*time.Hour))
	addRefresh(s, "t2", "c", late, now.Add(-2*time.Hour), now.Add(60*24*time.Hour))
	addRefresh(s, "t3", "c", late, now.Add(-30*time.Hour), now.Add(-time.Minute)) // expired: not counted active
	s.mu.Lock()
	s.lastSeen["c"] = now.Add(-time.Minute) // newer than any token's LastUsedAt
	s.mu.Unlock()

	c := connByID(t, s.ListConnections(), "c")
	if c.ActiveTokens != 2 {
		t.Errorf("ActiveTokens = %d, want 2", c.ActiveTokens)
	}
	if !c.SessionExpiresAt.Equal(now.Add(60 * 24 * time.Hour)) {
		t.Errorf("SessionExpiresAt = %v, want the LATEST live expiry", c.SessionExpiresAt)
	}
	if !c.ConnectedAt.Equal(early) {
		t.Errorf("ConnectedAt = %v, want the earliest %v", c.ConnectedAt, early)
	}
	if !c.LastUsed.Equal(now.Add(-time.Minute)) {
		t.Errorf("LastUsed = %v, want the lastSeen value", c.LastUsed)
	}
}

func TestListConnectionsSortAndOrphanTokens(t *testing.T) {
	s := newBareServer(t)
	now := time.Now()
	addClient(s, "b", "B", now)
	addClient(s, "a", "A", now)
	addClient(s, "never", "Z", now)
	addRefresh(s, "b1", "b", now, now.Add(-time.Hour), now.Add(time.Hour))
	addRefresh(s, "a1", "a", now, now.Add(-time.Minute), now.Add(time.Hour))
	addRefresh(s, "orphan", "gone", now, now, now.Add(time.Hour)) // client not registered

	conns := s.ListConnections()
	if len(conns) != 3 {
		t.Fatalf("orphan token's client must not be listed, got %d entries", len(conns))
	}
	order := []string{conns[0].ClientID, conns[1].ClientID, conns[2].ClientID}
	if order[0] != "a" || order[1] != "b" || order[2] != "never" {
		t.Fatalf("expected most-recently-used first, never-used last; got %v", order)
	}
}

// seedFullClient gives a client every kind of credential.
func seedFullClient(s *Server, id string) {
	now := time.Now()
	addClient(s, id, "Agent "+id, now.Add(-48*time.Hour))
	addRefresh(s, "r-"+id, id, now.Add(-24*time.Hour), now, now.Add(time.Hour))
	s.mu.Lock()
	s.tokens["at-"+id] = &accessToken{ClientID: id, ExpiresAt: now.Add(time.Hour)}
	s.codes["code-"+id] = &authCode{ClientID: id, ExpiresAt: now.Add(time.Minute)}
	s.lastSeen[id] = now
	s.mu.Unlock()
}

func TestRevokeClientRemovesEverythingForThatClientOnly(t *testing.T) {
	s := newBareServer(t)
	seedFullClient(s, "x")
	seedFullClient(s, "y")

	if err := s.RevokeClient("x"); err != nil {
		t.Fatalf("RevokeClient: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.clients["x"]; ok {
		t.Error("registration for x survived")
	}
	if _, ok := s.tokens["at-x"]; ok {
		t.Error("access token for x survived")
	}
	if _, ok := s.refreshTokens["r-x"]; ok {
		t.Error("refresh token for x survived")
	}
	if _, ok := s.codes["code-x"]; ok {
		t.Error("pending code for x survived")
	}
	if _, ok := s.lastSeen["x"]; ok {
		t.Error("lastSeen for x survived")
	}
	for name, present := range map[string]bool{
		"client":  s.clients["y"] != nil,
		"access":  s.tokens["at-y"] != nil,
		"refresh": s.refreshTokens["r-y"] != nil,
		"code":    s.codes["code-y"] != nil,
	} {
		if !present {
			t.Errorf("revoking x removed y's %s", name)
		}
	}
	if _, ok := s.lastSeen["y"]; !ok {
		t.Error("revoking x removed y's lastSeen")
	}
}

func TestRevokeClientUnknownID(t *testing.T) {
	s := newBareServer(t)
	if err := s.RevokeClient("nope"); !errors.Is(err, ErrClientNotFound) {
		t.Fatalf("expected ErrClientNotFound, got %v", err)
	}
}

func TestRevokedClientTokenIsRejected(t *testing.T) {
	s := newBareServer(t)
	seedFullClient(s, "x")
	h := s.RequireBearer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	if code := bearerRequest(t, h, "at-x"); code != http.StatusOK {
		t.Fatalf("before revoke: %d", code)
	}
	if err := s.RevokeClient("x"); err != nil {
		t.Fatal(err)
	}
	if code := bearerRequest(t, h, "at-x"); code != http.StatusUnauthorized {
		t.Fatalf("after revoke: got %d, want 401", code)
	}
}

func TestRevokeClientPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir, "pw")
	srv, ts := newTestHTTPServerWithConfig(t, cfg)
	clientID, redirectURI := registerClient(t, ts.URL)
	_, refresh := authorizeAndExchange(t, noRedirectClient(), ts.URL, clientID, redirectURI, "pw")

	if err := srv.RevokeClient(clientID); err != nil {
		t.Fatalf("RevokeClient: %v", err)
	}

	srv2 := newTestServer(t, cfg) // restart: only disk state survives
	srv2.mu.Lock()
	defer srv2.mu.Unlock()
	if _, ok := srv2.clients[clientID]; ok {
		t.Error("revoked client's registration came back after restart")
	}
	if _, ok := srv2.refreshTokens[hashToken(refresh)]; ok {
		t.Error("revoked client's refresh token came back after restart")
	}
}

func TestRevokeClientReportsPersistFailure(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(dir, "pw")
	s := newTestServer(t, cfg)
	seedFullClient(s, "x")
	if err := s.persistRefreshTokens(); err != nil { // r-x is on disk before the revoke
		t.Fatal(err)
	}
	if srv := newTestServer(t, cfg); srv.refreshTokens["r-x"] == nil {
		t.Fatal("precondition: r-x should be persisted before revoke")
	}

	s.clientsFile = filepath.Join(blocker, "clients.json") // now only the clients write fails
	if err := s.RevokeClient("x"); err == nil {
		t.Fatal("expected a persistence error")
	}
	s.mu.Lock()
	_, stillThere := s.clients["x"]
	s.mu.Unlock()
	if stillThere {
		t.Error("in-memory revocation must still happen when persisting fails")
	}
	// Both writes are attempted: the refresh-token file was rewritten without
	// r-x even though the clients write failed.
	if srv := newTestServer(t, cfg); srv.refreshTokens["r-x"] != nil {
		t.Error("refresh-token write was skipped after the clients write failed")
	}
}

// Grants racing a revoke must never leave tokens behind for the revoked client.
// Like all race tests this is probabilistic: it is meaningful under -race but
// can pass by luck if the guard regresses; the deterministic coverage is
// TestIssueTokenPairRejectsUnregisteredClient.
func TestRevokeClientRacingGrants(t *testing.T) {
	s := newBareServer(t)
	addClient(s, "c1", "Racer", time.Now())

	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.issueTokenPair(httptest.NewRecorder(), "c1", "", time.Time{})
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := s.RevokeClient("c1"); err != nil {
			t.Errorf("RevokeClient: %v", err)
		}
	}()
	wg.Wait()

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, at := range s.tokens {
		if at.ClientID == "c1" {
			t.Fatal("access token survived revocation")
		}
	}
	for _, rt := range s.refreshTokens {
		if rt.ClientID == "c1" {
			t.Fatal("refresh token survived revocation")
		}
	}
}

// RequireBearer must not recreate lastSeen for a client that was just revoked.
// Probabilistic, like TestRevokeClientRacingGrants.
func TestLastSeenNotRecreatedAfterRevoke(t *testing.T) {
	s := newBareServer(t)
	seedFullClient(s, "x")
	h := s.RequireBearer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			bearerRequestNoFatal(h, "at-x")
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := s.RevokeClient("x"); err != nil {
			t.Errorf("RevokeClient: %v", err)
		}
	}()
	wg.Wait()

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.lastSeen["x"]; ok {
		t.Fatal("lastSeen entry exists for a revoked client")
	}
}

func bearerRequestNoFatal(h http.Handler, token string) {
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	h.ServeHTTP(httptest.NewRecorder(), req)
}

func TestPruneStale(t *testing.T) {
	s := newBareServer(t)
	now := time.Now()
	old := now.Add(-3 * time.Hour)

	addClient(s, "stale", "Stale", old) // nothing live, old enough: pruned
	s.mu.Lock()
	s.tokens["at-stale"] = &accessToken{ClientID: "stale", ExpiresAt: now.Add(-time.Hour)} // expired
	s.mu.Unlock()
	addRefresh(s, "r-stale", "stale", old, old, now.Add(-time.Minute)) // expired

	addClient(s, "fresh", "Fresh", now.Add(-10*time.Minute)) // within grace: kept
	addClient(s, "authorizing", "Authorizing", old)          // old but form recently opened: kept
	s.mu.Lock()
	s.clients["authorizing"].LastAuthorizeAt = now.Add(-5 * time.Minute)
	s.mu.Unlock()

	addClient(s, "pending", "Pending", old) // unexpired pending code: kept
	s.mu.Lock()
	s.codes["code"] = &authCode{ClientID: "pending", ExpiresAt: now.Add(time.Minute)}
	s.mu.Unlock()

	addClient(s, "live", "Live", old) // unexpired refresh token: kept
	addRefresh(s, "r-live", "live", old, now, now.Add(time.Hour))

	addClient(s, "access-only", "AccessOnly", old) // unexpired access token only: kept
	s.mu.Lock()
	s.tokens["at-access"] = &accessToken{ClientID: "access-only", ExpiresAt: now.Add(time.Minute)}
	s.mu.Unlock()

	n, err := s.PruneStale()
	if err != nil {
		t.Fatalf("PruneStale: %v", err)
	}
	if n != 1 {
		t.Fatalf("pruned %d clients, want 1", n)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.clients["stale"]; ok {
		t.Error("stale client was not pruned")
	}
	if _, ok := s.tokens["at-stale"]; ok {
		t.Error("pruned client's expired access token was not deleted")
	}
	if _, ok := s.refreshTokens["r-stale"]; ok {
		t.Error("pruned client's expired refresh token was not deleted")
	}
	for _, id := range []string{"fresh", "authorizing", "pending", "live", "access-only"} {
		if _, ok := s.clients[id]; !ok {
			t.Errorf("client %q was wrongly pruned", id)
		}
	}
}

func TestPruneStaleNothingToDoSkipsPersist(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(dir, "pw")
	cfg.ClientsFile = filepath.Join(blocker, "clients.json") // any persist attempt would fail
	s := newTestServer(t, cfg)
	addClient(s, "fresh", "Fresh", time.Now())
	n, err := s.PruneStale()
	if err != nil || n != 0 {
		t.Fatalf("got (%d, %v), want (0, nil): nothing pruned must not persist", n, err)
	}
}

func TestPruneStalePersists(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir, "pw")
	s := newTestServer(t, cfg)
	addClient(s, "stale", "Stale", time.Now().Add(-3*time.Hour))
	if err := s.persistClients(); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PruneStale(); err != nil || n != 1 {
		t.Fatalf("got (%d, %v), want (1, nil)", n, err)
	}
	s2 := newTestServer(t, cfg)
	s2.mu.Lock()
	defer s2.mu.Unlock()
	if _, ok := s2.clients["stale"]; ok {
		t.Fatal("pruned client came back after restart")
	}
}

// GET /authorize stamps LastAuthorizeAt in memory but must never write to disk
// (the route is unauthenticated).
func TestAuthorizeGetStampsLastAuthorizeAtWithoutPersisting(t *testing.T) {
	cfg := testConfig(t.TempDir(), "pw")
	srv, ts := newTestHTTPServerWithConfig(t, cfg)
	clientID, redirectURI := registerClient(t, ts.URL)

	before, err := os.ReadFile(cfg.ClientsFile)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(ts.URL + "/authorize?response_type=code&client_id=" + clientID +
		"&redirect_uri=" + redirectURI + "&code_challenge=abc&code_challenge_method=S256")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	srv.mu.Lock()
	stamped := srv.clients[clientID].LastAuthorizeAt
	srv.mu.Unlock()
	if stamped.IsZero() {
		t.Fatal("LastAuthorizeAt was not stamped by GET /authorize")
	}
	after, err := os.ReadFile(cfg.ClientsFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("GET /authorize wrote to the clients file")
	}
}

// An oauth-clients.json written before LastAuthorizeAt existed still loads.
func TestLoadOldFormatClientsFile(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir, "pw")
	legacy := `{"c1":{"ID":"c1","Secret":"","Name":"Old","RedirectURIs":["http://x/cb"],"TokenEndpointAuthMethod":"none","CreatedAt":"2026-01-01T00:00:00Z"}}`
	if err := os.WriteFile(cfg.ClientsFile, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, cfg)
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.clients["c1"]
	if !ok || c.Name != "Old" || !c.LastAuthorizeAt.IsZero() {
		t.Fatalf("legacy client did not load as expected: %+v", c)
	}
}

// A client that is old and has no tokens, but whose owner just opened the
// authorize form, must survive a prune and still complete authorization.
func TestPruneStaleKeepsClientMidAuthorization(t *testing.T) {
	srv, ts := newTestHTTPServer(t, "pw")
	clientID, redirectURI := registerClient(t, ts.URL)
	srv.mu.Lock()
	srv.clients[clientID].CreatedAt = time.Now().Add(-5 * time.Hour) // registered long ago
	srv.mu.Unlock()

	resp, err := http.Get(ts.URL + "/authorize?response_type=code&client_id=" + clientID +
		"&redirect_uri=" + redirectURI + "&code_challenge=abc&code_challenge_method=S256")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if n, err := srv.PruneStale(); err != nil || n != 0 {
		t.Fatalf("prune while the authorize form is open: got (%d, %v), want (0, nil)", n, err)
	}
	// The owner now submits the form; the whole flow still works.
	_, refresh := authorizeAndExchange(t, noRedirectClient(), ts.URL, clientID, redirectURI, "pw")
	if refresh == "" {
		t.Fatal("expected tokens after completing authorization")
	}
}
