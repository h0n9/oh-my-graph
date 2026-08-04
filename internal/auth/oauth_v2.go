// Package auth implements the MCP Authorization spec flow: OAuth 2.1
// Authorization Code + PKCE, RFC 7591 Dynamic Client Registration, and
// RFC 9728 / RFC 8414 discovery metadata. It is single-tenant — there is one
// owner, gated by a shared passphrase entered once per client at /authorize.
// DCR registration is unauthenticated by spec, so the passphrase (not
// registration) is the real access control.
//
// Authorization codes and access tokens are in-memory only and do not
// survive a restart -- by design, since they're short-lived (60s and 1h
// respectively) and cheap to reissue. Refresh tokens are longer-lived (90d)
// and are optionally persisted to disk, AES-256-GCM-encrypted under a key
// derived from Config.OwnerPassphrase (Config.RefreshTokensFile), so a
// restarted process can silently reissue an access token via the
// refresh_token grant instead of forcing every connected client through
// interactive re-authentication. DCR client registrations are similarly
// optionally persisted to disk (Config.ClientsFile) since clients treat
// registration as a durable, one-time bootstrap step and have no way to
// recover from their client_id becoming unknown -- unlike token expiry,
// which they're built to handle gracefully.
//
// Because refresh tokens now survive restarts, "restart the server" is no
// longer a way to revoke a leaked refresh token -- delete
// Config.RefreshTokensFile or rotate Config.OwnerPassphrase to force every
// client to re-authenticate (the latter affects all clients at once; there
// is no per-client revocation).
//
// Refresh tokens are keyed by their SHA-256 hash, not their raw value, in
// both the in-memory map and the persisted file -- defense in depth so a
// leaked file or memory-only leak (core dump, swap, host-level inspection)
// yields hashes, not directly usable credentials. See hashToken's doc
// comment for why no salt/HMAC is needed here.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

const (
	authCodeTTL    = 60 * time.Second
	accessTokenTTL = time.Hour
	refreshTTL     = 90 * 24 * time.Hour

	// sessionTTL is long on purpose: as long as the process stays up, the
	// owner should only ever be asked for the passphrase once. In practice
	// what actually forces re-auth is a process restart (e.g. a Sprites
	// cold-wake), which wipes sessions along with everything else in this
	// package's in-memory stores — not this TTL.
	sessionTTL        = 90 * 24 * time.Hour
	sessionCookieName = "omg_session"
)

type Config struct {
	Issuer            string // public base URL, e.g. https://oh-my-graph-h0n9.sprites.app
	OwnerPassphrase   string
	ClientsFile       string // optional; persists DCR client registrations across restarts
	RefreshTokensFile string // optional; persists refresh tokens (AES-256-GCM encrypted) across restarts
}

type client struct {
	ID                      string
	Secret                  string // empty for public clients (token_endpoint_auth_method=none)
	Name                    string
	RedirectURIs            []string
	TokenEndpointAuthMethod string
	CreatedAt               time.Time
}

type authCode struct {
	ClientID            string
	RedirectURI         string
	CodeChallenge       string
	CodeChallengeMethod string
	Scope               string
	ExpiresAt           time.Time
}

type accessToken struct {
	ClientID  string
	Scope     string
	ExpiresAt time.Time
}

type refreshToken struct {
	ClientID  string
	Scope     string
	ExpiresAt time.Time
}

type Server struct {
	issuer            string
	ownerPassphrase   string
	clientsFile       string
	refreshTokensFile string
	authorizeTmpl     *template.Template
	loginTmpl         *template.Template

	mu            sync.Mutex
	clients       map[string]*client
	codes         map[string]*authCode
	tokens        map[string]*accessToken
	refreshTokens map[string]*refreshToken
	sessions      map[string]time.Time // session ID -> expiry

	// encKey and encSalt are derived once (in loadRefreshTokens, called from
	// NewServer) and never mutated afterward, so they're safe to read from
	// persistRefreshTokens without holding mu.
	encKey  []byte
	encSalt []byte
}

func NewServer(cfg Config) *Server {
	s := &Server{
		issuer:            strings.TrimRight(cfg.Issuer, "/"),
		ownerPassphrase:   cfg.OwnerPassphrase,
		clientsFile:       cfg.ClientsFile,
		refreshTokensFile: cfg.RefreshTokensFile,
		authorizeTmpl:     template.Must(template.New("authorize").Parse(authorizeHTML)),
		loginTmpl:         template.Must(template.New("login").Parse(loginHTML)),
		clients:           make(map[string]*client),
		codes:             make(map[string]*authCode),
		tokens:            make(map[string]*accessToken),
		refreshTokens:     make(map[string]*refreshToken),
		sessions:          make(map[string]time.Time),
	}
	s.loadClients()
	s.loadRefreshTokens()
	return s
}

// loadClients best-effort restores DCR client registrations from disk so a
// restart doesn't orphan already-connected clients (see package doc). A
// missing or unreadable file just starts with an empty registry -- this is a
// convenience cache, not a source of truth clients can't function without.
func (s *Server) loadClients() {
	if s.clientsFile == "" {
		return
	}
	data, err := os.ReadFile(s.clientsFile)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("oh-my-graph/auth: loadClients: %v", err)
		}
		return
	}
	var clients map[string]*client
	if err := json.Unmarshal(data, &clients); err != nil {
		log.Printf("oh-my-graph/auth: loadClients: parse %s: %v", s.clientsFile, err)
		return
	}
	s.clients = clients
	log.Printf("oh-my-graph/auth: loaded %d client registration(s) from %s", len(clients), s.clientsFile)
}

// persistClients writes the current client registry to disk. Best-effort:
// logs on failure but never fails the caller's request over it, since the
// in-memory registry remains authoritative for the life of this process.
func (s *Server) persistClients() {
	if s.clientsFile == "" {
		return
	}
	s.mu.Lock()
	data, err := json.MarshalIndent(s.clients, "", "  ")
	s.mu.Unlock()
	if err != nil {
		log.Printf("oh-my-graph/auth: persistClients: marshal: %v", err)
		return
	}

	tmp := s.clientsFile + ".tmp"
	if err := os.MkdirAll(filepath.Dir(s.clientsFile), 0o700); err != nil {
		log.Printf("oh-my-graph/auth: persistClients: mkdir: %v", err)
		return
	}
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		log.Printf("oh-my-graph/auth: persistClients: write: %v", err)
		return
	}
	if err := os.Rename(tmp, s.clientsFile); err != nil {
		log.Printf("oh-my-graph/auth: persistClients: rename: %v", err)
	}
}

// refreshTokensAAD binds the encrypted refresh-tokens file to its purpose
// and format version, so it can never be silently misinterpreted as some
// other ciphertext (and gives a natural place to bump on a future format
// change: change the string, old files simply fail to decrypt like any
// other corruption).
var refreshTokensAAD = []byte("oh-my-graph/oauth-refresh-tokens/v1")

// loadRefreshTokens best-effort restores persisted refresh tokens and
// establishes s.encKey/s.encSalt for this process's lifetime -- derived
// once here (expensive, by design) and reused by every later
// persistRefreshTokens call rather than re-derived per write. A missing
// file, corrupt file, or wrong/rotated passphrase (GCM auth-tag failure)
// all just log and leave the server with an empty refreshTokens map and a
// freshly generated salt/key for future writes -- this is a durability
// optimization, not a source of truth the server can't run without.
// Already-expired entries are dropped on load.
func (s *Server) loadRefreshTokens() {
	if s.refreshTokensFile == "" {
		return
	}

	salt := make([]byte, saltSize)
	data, err := os.ReadFile(s.refreshTokensFile)
	switch {
	case err != nil && !os.IsNotExist(err):
		log.Printf("oh-my-graph/auth: loadRefreshTokens: %v", err)
		fallthrough
	case err != nil:
		if _, rerr := rand.Read(salt); rerr != nil {
			log.Printf("oh-my-graph/auth: loadRefreshTokens: generate salt: %v", rerr)
			return
		}
	case len(data) < saltSize:
		log.Printf("oh-my-graph/auth: loadRefreshTokens: %s: file too short, starting empty", s.refreshTokensFile)
		if _, rerr := rand.Read(salt); rerr != nil {
			log.Printf("oh-my-graph/auth: loadRefreshTokens: generate salt: %v", rerr)
			return
		}
	default:
		copy(salt, data[:saltSize])
	}

	key, err := deriveKey(s.ownerPassphrase, salt)
	if err != nil {
		log.Printf("oh-my-graph/auth: loadRefreshTokens: derive key: %v", err)
		return
	}
	s.encSalt = salt
	s.encKey = key

	if len(data) <= saltSize {
		return // missing/too-short file, already logged above -- start empty
	}

	plaintext, err := decryptBlob(key, refreshTokensAAD, data[saltSize:])
	if err != nil {
		// Never log err's details beyond this generic line -- it carries no
		// key material, but keep it that way deliberately (see package doc).
		log.Printf("oh-my-graph/auth: loadRefreshTokens: %s: could not decrypt (passphrase changed or file corrupted?), starting empty", s.refreshTokensFile)
		return
	}

	var tokens map[string]*refreshToken
	if err := json.Unmarshal(plaintext, &tokens); err != nil {
		log.Printf("oh-my-graph/auth: loadRefreshTokens: parse: %v", err)
		return
	}

	now := time.Now()
	kept := make(map[string]*refreshToken, len(tokens))
	for tok, rt := range tokens {
		if now.After(rt.ExpiresAt) {
			continue
		}
		kept[tok] = rt
	}
	s.refreshTokens = kept
	log.Printf("oh-my-graph/auth: loaded %d refresh token(s) from %s", len(kept), s.refreshTokensFile)
}

// persistRefreshTokens serializes the in-memory refreshTokens map and
// atomically writes it to disk, AES-256-GCM-encrypted under s.encKey.
//
// Unlike persistClients, this holds s.mu for the *entire* marshal+encrypt+
// write sequence, not just the marshal. Refresh tokens rotate on every use
// (unlike DCR client registrations, which are rare), so releasing the lock
// before I/O would let two concurrent /token calls' writes land on disk out
// of order and resurrect an already-rotated refresh token. Do not
// "simplify" this back to match persistClients's lock-then-unlock-then-write
// shape -- that reintroduces the race this comment is here to prevent.
func (s *Server) persistRefreshTokens() {
	if s.refreshTokensFile == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.encKey == nil {
		// loadRefreshTokens couldn't establish a key (e.g. salt generation
		// failed) -- nothing safe to encrypt under, skip this write.
		return
	}

	plaintext, err := json.Marshal(s.refreshTokens)
	if err != nil {
		log.Printf("oh-my-graph/auth: persistRefreshTokens: marshal: %v", err)
		return
	}
	ciphertext, err := encryptBlob(s.encKey, refreshTokensAAD, plaintext)
	if err != nil {
		log.Printf("oh-my-graph/auth: persistRefreshTokens: encrypt: %v", err)
		return
	}
	data := make([]byte, 0, len(s.encSalt)+len(ciphertext))
	data = append(data, s.encSalt...)
	data = append(data, ciphertext...)

	tmp := s.refreshTokensFile + ".tmp"
	if err := os.MkdirAll(filepath.Dir(s.refreshTokensFile), 0o700); err != nil {
		log.Printf("oh-my-graph/auth: persistRefreshTokens: mkdir: %v", err)
		return
	}
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		log.Printf("oh-my-graph/auth: persistRefreshTokens: write: %v", err)
		return
	}
	if err := os.Rename(tmp, s.refreshTokensFile); err != nil {
		log.Printf("oh-my-graph/auth: persistRefreshTokens: rename: %v", err)
	}
}

func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/.well-known/oauth-protected-resource", s.handleProtectedResourceMetadata)
	mux.HandleFunc("/.well-known/oauth-authorization-server", s.handleAuthServerMetadata)
	mux.HandleFunc("/register", s.handleRegister)
	mux.HandleFunc("/authorize", s.handleAuthorize)
	mux.HandleFunc("/token", s.handleToken)
	mux.HandleFunc("/login", s.handleLogin)
}

// RequireBearer wraps next so that requests must carry a valid access token
// issued by this server. On failure it replies 401 with a WWW-Authenticate
// challenge pointing at the protected-resource metadata (RFC 9728), which is
// how MCP clients discover this authorization server in the first place.
func (s *Server) RequireBearer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		authz := r.Header.Get("Authorization")
		if !strings.HasPrefix(authz, prefix) {
			log.Printf("oh-my-graph/auth: bearer: rejected %s %s: missing/malformed Authorization header (got %q)", r.Method, r.URL.Path, authz)
			s.unauthorized(w)
			return
		}

		tok := strings.TrimPrefix(authz, prefix)
		s.mu.Lock()
		at, ok := s.tokens[tok]
		s.mu.Unlock()
		if !ok || time.Now().After(at.ExpiresAt) {
			log.Printf("oh-my-graph/auth: bearer: rejected %s %s: token known=%v expired=%v", r.Method, r.URL.Path, ok, ok && time.Now().After(at.ExpiresAt))
			s.unauthorized(w)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (s *Server) unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource"`, s.issuer))
	writeOAuthError(w, http.StatusUnauthorized, "invalid_token", "missing or invalid bearer access token")
}

// RequireOwnerSession gates browser-facing pages (which can't attach a
// Bearer header to a plain navigation) behind a session cookie issued by
// /login. The viz UI has exactly one JS-fetched route (/api/graph); it gets
// a plain 401 so the page's own fetch().catch() handles it, while real page
// navigations get redirected to /login to keep the passphrase prompt as an
// in-app page instead of the browser's native Basic Auth dialog.
func (s *Server) RequireOwnerSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.hasValidSession(r) {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/api/graph" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		q := url.Values{"next": {r.URL.RequestURI()}}
		http.Redirect(w, r, "/login?"+q.Encode(), http.StatusFound)
	})
}

func (s *Server) hasValidSession(r *http.Request) bool {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return false
	}
	s.mu.Lock()
	expiresAt, ok := s.sessions[c.Value]
	s.mu.Unlock()
	return ok && time.Now().Before(expiresAt)
}

// --- discovery metadata ---

func (s *Server) handleProtectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":              s.issuer + "/omg-mcp",
		"authorization_servers": []string{s.issuer},
	})
}

func (s *Server) handleAuthServerMetadata(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                s.issuer,
		"authorization_endpoint":                s.issuer + "/authorize",
		"token_endpoint":                        s.issuer + "/token",
		"registration_endpoint":                 s.issuer + "/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none", "client_secret_basic"},
	})
}

// --- RFC 7591 dynamic client registration ---

type registerRequest struct {
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Printf("oh-my-graph/auth: register: malformed JSON body: %v", err)
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "malformed JSON body")
		return
	}
	log.Printf("oh-my-graph/auth: register: client_name=%q redirect_uris=%q token_endpoint_auth_method=%q", req.ClientName, req.RedirectURIs, req.TokenEndpointAuthMethod)
	if len(req.RedirectURIs) == 0 {
		log.Printf("oh-my-graph/auth: register: rejected: no redirect_uris")
		writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uris is required")
		return
	}
	for _, u := range req.RedirectURIs {
		parsed, err := url.ParseRequestURI(u)
		if err != nil || parsed.Scheme == "" {
			log.Printf("oh-my-graph/auth: register: rejected: invalid redirect_uris entry %q: %v", u, err)
			writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "invalid redirect_uris entry: "+u)
			return
		}
	}

	authMethod := req.TokenEndpointAuthMethod
	if authMethod == "" {
		authMethod = "none"
	}

	clientID, err := randomToken(24)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	c := &client{
		ID:                      clientID,
		Name:                    req.ClientName,
		RedirectURIs:            req.RedirectURIs,
		TokenEndpointAuthMethod: authMethod,
		CreatedAt:               time.Now(),
	}

	resp := map[string]any{
		"client_id":                  clientID,
		"client_id_issued_at":        c.CreatedAt.Unix(),
		"client_name":                c.Name,
		"redirect_uris":              c.RedirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": authMethod,
	}

	if authMethod != "none" {
		secret, err := randomToken(32)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		c.Secret = secret
		resp["client_secret"] = secret
		resp["client_secret_expires_at"] = 0
	}

	s.mu.Lock()
	s.clients[clientID] = c
	s.mu.Unlock()
	s.persistClients()

	log.Printf("oh-my-graph/auth: register: issued client_id=%s auth_method=%s", clientID, authMethod)
	writeJSON(w, http.StatusCreated, resp)
}

// --- /authorize ---

const authorizeHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0, viewport-fit=cover">
<title>Authorize — oh-my-graph</title>
<style>
  :root {
    --bg:     #ffffff;
    --fg:     #000000;
    --muted:  #666666;
    --border: #dddddd;
    --err:    #b00020;
  }
  @media (prefers-color-scheme: dark) {
    :root {
      --bg:     #111111;
      --fg:     #eeeeee;
      --muted:  #999999;
      --border: #333333;
      --err:    #ff6b6b;
    }
  }
  * { box-sizing: border-box; margin: 0; padding: 0; }
  html, body { height: 100%; }
  body {
    background: var(--bg);
    color: var(--fg);
    font-family: ui-monospace, "SFMono-Regular", Menlo, monospace;
    font-size: 14px;
    display: flex;
    align-items: center;
    justify-content: center;
    /* 100vh alone under-covers a standalone iOS PWA's actual visible area
       (Dynamic Island / home-indicator safe areas), which throws off
       centering in the installed app despite it centering correctly in an
       ordinary browser tab. 100dvh (dynamic viewport height) reflects the
       real visible viewport; the plain 100vh line stays first as a
       fallback for browsers that don't support dvh yet. */
    min-height: 100vh;
    min-height: 100dvh;
    padding: max(24px, env(safe-area-inset-top)) max(24px, env(safe-area-inset-right))
             max(24px, env(safe-area-inset-bottom)) max(24px, env(safe-area-inset-left));
  }
  form { width: 100%; max-width: 300px; }
  h1 {
    font-size: 16px;
    font-weight: 600;
    letter-spacing: -0.3px;
    margin-bottom: 8px;
  }
  .desc { color: var(--muted); font-size: 12px; margin-bottom: 24px; line-height: 1.5; }
  input[type=password] {
    width: 100%;
    padding: 10px 12px;
    font: inherit;
    background: var(--bg);
    color: var(--fg);
    border: 1px solid var(--border);
    border-radius: 6px;
    margin-bottom: 12px;
  }
  input[type=password]:focus { outline: none; border-color: var(--fg); }
  button {
    width: 100%;
    padding: 10px 12px;
    font: inherit;
    font-weight: 600;
    background: var(--fg);
    color: var(--bg);
    border: none;
    border-radius: 6px;
    cursor: pointer;
  }
  button:hover { opacity: 0.85; }
  .err { color: var(--err); font-size: 12px; margin-bottom: 12px; }
</style>
</head>
<body>
<form method="POST" action="/authorize">
  <h1>Authorize {{.ClientName}}</h1>
  <p class="desc">Enter the owner passphrase to grant this client access to oh-my-graph.</p>
  {{if .Error}}<p class="err">{{.Error}}</p>{{end}}
  <input type="hidden" name="response_type" value="{{.ResponseType}}">
  <input type="hidden" name="client_id" value="{{.ClientID}}">
  <input type="hidden" name="redirect_uri" value="{{.RedirectURI}}">
  <input type="hidden" name="code_challenge" value="{{.CodeChallenge}}">
  <input type="hidden" name="code_challenge_method" value="{{.CodeChallengeMethod}}">
  <input type="hidden" name="state" value="{{.State}}">
  <input type="hidden" name="scope" value="{{.Scope}}">
  <input type="password" name="passphrase" placeholder="Passphrase" autofocus required>
  <button type="submit">Authorize</button>
</form>
</body>
</html>`

type authorizeParams struct {
	ResponseType        string
	ClientID            string
	RedirectURI         string
	CodeChallenge       string
	CodeChallengeMethod string
	State               string
	Scope               string
}

type authorizeView struct {
	authorizeParams
	ClientName string
	Error      string
}

func parseAuthorizeParams(values url.Values) authorizeParams {
	return authorizeParams{
		ResponseType:        values.Get("response_type"),
		ClientID:            values.Get("client_id"),
		RedirectURI:         values.Get("redirect_uri"),
		CodeChallenge:       values.Get("code_challenge"),
		CodeChallengeMethod: values.Get("code_challenge_method"),
		State:               values.Get("state"),
		Scope:               values.Get("scope"),
	}
}

// validateAuthorizeParams reports whether an error must be shown directly
// (unknown client_id or a redirect_uri that doesn't match registration —
// unsafe to redirect to) versus one that's safe to relay back to the client
// via redirect, per RFC 6749 §4.1.2.1.
func (s *Server) validateAuthorizeParams(p authorizeParams) (c *client, mustNotRedirect bool, errCode, errDesc string) {
	s.mu.Lock()
	cl, ok := s.clients[p.ClientID]
	s.mu.Unlock()
	if !ok {
		return nil, true, "invalid_client", "unknown client_id"
	}

	found := false
	for _, u := range cl.RedirectURIs {
		if u == p.RedirectURI {
			found = true
			break
		}
	}
	if !found {
		return nil, true, "invalid_request", "redirect_uri does not match a registered value"
	}
	if p.ResponseType != "code" {
		return cl, false, "unsupported_response_type", "only response_type=code is supported"
	}
	if p.CodeChallengeMethod != "S256" || p.CodeChallenge == "" {
		return cl, false, "invalid_request", "PKCE with code_challenge_method=S256 is required"
	}
	return cl, false, "", ""
}

func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		p := parseAuthorizeParams(r.URL.Query())
		log.Printf("oh-my-graph/auth: authorize GET: client_id=%q redirect_uri=%q response_type=%q code_challenge_method=%q state=%q", p.ClientID, p.RedirectURI, p.ResponseType, p.CodeChallengeMethod, p.State)
		c, mustNotRedirect, errCode, errDesc := s.validateAuthorizeParams(p)
		if errCode != "" {
			log.Printf("oh-my-graph/auth: authorize GET: rejected client_id=%q: %s (%s) mustNotRedirect=%v", p.ClientID, errCode, errDesc, mustNotRedirect)
			if mustNotRedirect {
				http.Error(w, errDesc, http.StatusBadRequest)
				return
			}
			redirectWithError(w, r, p.RedirectURI, p.State, errCode, errDesc)
			return
		}
		s.renderAuthorizeForm(w, p, c.Name, "")

	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			log.Printf("oh-my-graph/auth: authorize POST: invalid form body: %v", err)
			http.Error(w, "invalid form body", http.StatusBadRequest)
			return
		}
		p := parseAuthorizeParams(r.PostForm)
		log.Printf("oh-my-graph/auth: authorize POST: client_id=%q redirect_uri=%q state=%q", p.ClientID, p.RedirectURI, p.State)
		c, mustNotRedirect, errCode, errDesc := s.validateAuthorizeParams(p)
		if errCode != "" {
			log.Printf("oh-my-graph/auth: authorize POST: rejected client_id=%q: %s (%s) mustNotRedirect=%v", p.ClientID, errCode, errDesc, mustNotRedirect)
			if mustNotRedirect {
				http.Error(w, errDesc, http.StatusBadRequest)
				return
			}
			redirectWithError(w, r, p.RedirectURI, p.State, errCode, errDesc)
			return
		}

		passphrase := r.PostForm.Get("passphrase")
		if subtle.ConstantTimeCompare([]byte(passphrase), []byte(s.ownerPassphrase)) != 1 {
			log.Printf("oh-my-graph/auth: authorize POST: incorrect passphrase for client_id=%s", p.ClientID)
			s.renderAuthorizeForm(w, p, c.Name, "Incorrect passphrase. Try again.")
			return
		}

		code, err := randomToken(24)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		s.mu.Lock()
		s.codes[code] = &authCode{
			ClientID:            p.ClientID,
			RedirectURI:         p.RedirectURI,
			CodeChallenge:       p.CodeChallenge,
			CodeChallengeMethod: p.CodeChallengeMethod,
			Scope:               p.Scope,
			ExpiresAt:           time.Now().Add(authCodeTTL),
		}
		s.mu.Unlock()

		redirectURL, err := url.Parse(p.RedirectURI)
		if err != nil {
			http.Error(w, "invalid redirect_uri", http.StatusBadRequest)
			return
		}
		q := redirectURL.Query()
		q.Set("code", code)
		if p.State != "" {
			q.Set("state", p.State)
		}
		redirectURL.RawQuery = q.Encode()
		log.Printf("oh-my-graph/auth: authorize POST: issued code for client_id=%q (redirect target and code omitted from log)", p.ClientID)
		http.Redirect(w, r, redirectURL.String(), http.StatusFound)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) renderAuthorizeForm(w http.ResponseWriter, p authorizeParams, clientName, formErr string) {
	if clientName == "" {
		clientName = "this client"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	view := authorizeView{authorizeParams: p, ClientName: clientName, Error: formErr}
	if err := s.authorizeTmpl.Execute(w, view); err != nil {
		log.Printf("oh-my-graph: render authorize form: %v", err)
	}
}

func redirectWithError(w http.ResponseWriter, r *http.Request, redirectURI, state, errCode, desc string) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		http.Error(w, "invalid redirect_uri", http.StatusBadRequest)
		return
	}
	q := u.Query()
	q.Set("error", errCode)
	q.Set("error_description", desc)
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// --- /login ---
//
// A styled in-app passphrase page for the viz UI, replacing the browser's
// native HTTP Basic Auth dialog. Unlike authorizeHTML (a generic
// OAuth-consent-screen look for third-party clients), this is part of the
// product's own UI, so it borrows the actual app's dark/light
// prefers-color-scheme palette and monospace font from index.html/graph.html
// instead of introducing a third, inconsistent style.

const loginHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0, viewport-fit=cover">
<title>oh-my-graph</title>
<style>
  :root {
    --bg:     #ffffff;
    --fg:     #000000;
    --muted:  #666666;
    --border: #dddddd;
    --err:    #b00020;
  }
  @media (prefers-color-scheme: dark) {
    :root {
      --bg:     #111111;
      --fg:     #eeeeee;
      --muted:  #999999;
      --border: #333333;
      --err:    #ff6b6b;
    }
  }
  * { box-sizing: border-box; margin: 0; padding: 0; }
  html, body { height: 100%; }
  body {
    background: var(--bg);
    color: var(--fg);
    font-family: ui-monospace, "SFMono-Regular", Menlo, monospace;
    font-size: 14px;
    display: flex;
    align-items: center;
    justify-content: center;
    /* 100vh alone under-covers a standalone iOS PWA's actual visible area
       (Dynamic Island / home-indicator safe areas), which throws off
       centering in the installed app despite it centering correctly in an
       ordinary browser tab. 100dvh (dynamic viewport height) reflects the
       real visible viewport; the plain 100vh line stays first as a
       fallback for browsers that don't support dvh yet. */
    min-height: 100vh;
    min-height: 100dvh;
    padding: max(24px, env(safe-area-inset-top)) max(24px, env(safe-area-inset-right))
             max(24px, env(safe-area-inset-bottom)) max(24px, env(safe-area-inset-left));
  }
  form { width: 100%; max-width: 300px; }
  h1 {
    font-size: 16px;
    font-weight: 600;
    letter-spacing: -0.3px;
    margin-bottom: 24px;
  }
  input[type=password] {
    width: 100%;
    padding: 10px 12px;
    font: inherit;
    background: var(--bg);
    color: var(--fg);
    border: 1px solid var(--border);
    border-radius: 6px;
    margin-bottom: 12px;
  }
  input[type=password]:focus { outline: none; border-color: var(--fg); }
  button {
    width: 100%;
    padding: 10px 12px;
    font: inherit;
    font-weight: 600;
    background: var(--fg);
    color: var(--bg);
    border: none;
    border-radius: 6px;
    cursor: pointer;
  }
  button:hover { opacity: 0.85; }
  .err { color: var(--err); font-size: 12px; margin-bottom: 12px; }
</style>
</head>
<body>
<form method="POST" action="/login">
  <h1>oh-my-graph</h1>
  {{if .Error}}<p class="err">{{.Error}}</p>{{end}}
  <input type="hidden" name="next" value="{{.Next}}">
  <input type="password" name="passphrase" placeholder="Passphrase" autofocus required>
  <button type="submit">Log in</button>
</form>
</body>
</html>`

type loginView struct {
	Next  string
	Error string
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		next := sanitizeNext(r.URL.Query().Get("next"))
		s.renderLoginForm(w, next, "")

	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			log.Printf("oh-my-graph/auth: login POST: invalid form body: %v", err)
			http.Error(w, "invalid form body", http.StatusBadRequest)
			return
		}
		next := sanitizeNext(r.PostForm.Get("next"))

		passphrase := r.PostForm.Get("passphrase")
		if subtle.ConstantTimeCompare([]byte(passphrase), []byte(s.ownerPassphrase)) != 1 {
			log.Printf("oh-my-graph/auth: login POST: incorrect passphrase")
			s.renderLoginForm(w, next, "Incorrect passphrase. Try again.")
			return
		}

		sessionID, err := randomToken(32)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		expiresAt := time.Now().Add(sessionTTL)
		s.mu.Lock()
		s.sessions[sessionID] = expiresAt
		s.mu.Unlock()

		http.SetCookie(w, &http.Cookie{
			Name:     sessionCookieName,
			Value:    sessionID,
			Path:     "/",
			Expires:  expiresAt,
			MaxAge:   int(sessionTTL.Seconds()),
			HttpOnly: true,
			Secure:   strings.HasPrefix(s.issuer, "https://"),
			SameSite: http.SameSiteLaxMode,
		})
		log.Printf("oh-my-graph/auth: login POST: session issued, redirecting to %q", next)
		http.Redirect(w, r, next, http.StatusSeeOther)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) renderLoginForm(w http.ResponseWriter, next, formErr string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	view := loginView{Next: next, Error: formErr}
	if err := s.loginTmpl.Execute(w, view); err != nil {
		log.Printf("oh-my-graph: render login form: %v", err)
	}
}

// sanitizeNext guards against open redirects: only an on-site relative path
// is allowed through (a leading "/" that isn't "//", which browsers treat
// as protocol-relative to an attacker-controlled host); anything else falls
// back to "/". Backslashes are normalized to "/" before the check, since
// some browsers treat a leading "\" like "/" when resolving a relative URL
// (e.g. "/\evil.com" behaving like "//evil.com"), which would otherwise
// slip past a naive single-slash check.
func sanitizeNext(next string) string {
	normalized := strings.ReplaceAll(next, "\\", "/")
	if normalized == "" || normalized[0] != '/' || strings.HasPrefix(normalized, "//") {
		return "/"
	}
	return next
}

// --- /token ---

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		log.Printf("oh-my-graph/auth: token: malformed form body: %v", err)
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "malformed form body")
		return
	}

	grantType := r.PostForm.Get("grant_type")
	log.Printf("oh-my-graph/auth: token: grant_type=%q client_id=%q", grantType, r.PostForm.Get("client_id"))
	switch grantType {
	case "authorization_code":
		s.handleAuthorizationCodeGrant(w, r)
	case "refresh_token":
		s.handleRefreshTokenGrant(w, r)
	default:
		log.Printf("oh-my-graph/auth: token: unsupported grant_type=%q", grantType)
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "supported grant types: authorization_code, refresh_token")
	}
}

// authenticateClient supports both confidential-client auth styles advertised
// in discovery metadata: client_secret_basic (RFC 6749 §2.3.1 -- client_id and
// client_secret as HTTP Basic Auth credentials on the request, NOT form
// fields) and client_secret_post (client_secret as a form field). Basic Auth
// takes precedence when present.
func (s *Server) authenticateClient(r *http.Request, form url.Values) (*client, string, string) {
	clientID := form.Get("client_id")
	var secret string
	var haveSecret bool
	if basicUser, basicPass, ok := r.BasicAuth(); ok {
		clientID = basicUser
		secret = basicPass
		haveSecret = true
	} else if v := form.Get("client_secret"); v != "" {
		secret = v
		haveSecret = true
	}

	c, ok := s.lookupClient(clientID)
	if !ok {
		return nil, "invalid_client", "unknown client_id"
	}
	if c.TokenEndpointAuthMethod != "none" {
		if !haveSecret || subtle.ConstantTimeCompare([]byte(secret), []byte(c.Secret)) != 1 {
			return nil, "invalid_client", "client authentication failed"
		}
	}
	return c, "", ""
}

func (s *Server) lookupClient(id string) (*client, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.clients[id]
	return c, ok
}

func (s *Server) handleAuthorizationCodeGrant(w http.ResponseWriter, r *http.Request) {
	form := r.PostForm
	c, errCode, desc := s.authenticateClient(r, form)
	if errCode != "" {
		log.Printf("oh-my-graph/auth: token authorization_code: client auth failed for client_id=%q: %s", form.Get("client_id"), desc)
		writeOAuthError(w, http.StatusUnauthorized, errCode, desc)
		return
	}

	code := form.Get("code")
	s.mu.Lock()
	ac, ok := s.codes[code]
	if ok {
		delete(s.codes, code) // single-use: consume on first presentation regardless of outcome
	}
	s.mu.Unlock()

	if !ok || time.Now().After(ac.ExpiresAt) {
		log.Printf("oh-my-graph/auth: token authorization_code: code invalid or expired for client_id=%s (known=%v)", c.ID, ok)
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "authorization code is invalid or expired")
		return
	}
	if ac.ClientID != c.ID || ac.RedirectURI != form.Get("redirect_uri") {
		log.Printf("oh-my-graph/auth: token authorization_code: mismatch: code.client_id=%s got=%s code.redirect_uri=%q got=%q", ac.ClientID, c.ID, ac.RedirectURI, form.Get("redirect_uri"))
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "client_id or redirect_uri mismatch")
		return
	}

	verifier := form.Get("code_verifier")
	if verifier == "" || oauth2.S256ChallengeFromVerifier(verifier) != ac.CodeChallenge {
		log.Printf("oh-my-graph/auth: token authorization_code: PKCE verification failed for client_id=%s (verifier present=%v)", c.ID, verifier != "")
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
		return
	}

	log.Printf("oh-my-graph/auth: token authorization_code: success for client_id=%s", c.ID)
	s.issueTokenPair(w, c.ID, ac.Scope)
}

func (s *Server) handleRefreshTokenGrant(w http.ResponseWriter, r *http.Request) {
	form := r.PostForm
	c, errCode, desc := s.authenticateClient(r, form)
	if errCode != "" {
		writeOAuthError(w, http.StatusUnauthorized, errCode, desc)
		return
	}

	rt := form.Get("refresh_token")
	rtHash := hashToken(rt)
	s.mu.Lock()
	old, ok := s.refreshTokens[rtHash]
	if ok {
		delete(s.refreshTokens, rtHash) // rotate on every use per OAuth 2.1 guidance for public clients
	}
	s.mu.Unlock()

	if !ok || old.ClientID != c.ID || time.Now().After(old.ExpiresAt) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "refresh_token is invalid, expired, or was issued to a different client")
		return
	}

	s.issueTokenPair(w, c.ID, old.Scope)
}

func (s *Server) issueTokenPair(w http.ResponseWriter, clientID, scope string) {
	access, err := randomToken(32)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	refresh, err := randomToken(32)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	s.mu.Lock()
	s.tokens[access] = &accessToken{ClientID: clientID, Scope: scope, ExpiresAt: time.Now().Add(accessTokenTTL)}
	// Store the token hashed, not raw -- see hashToken's doc comment. Only
	// this response body ever carries the raw value; from here on the
	// server (memory and disk alike) only ever holds its SHA-256 digest.
	s.refreshTokens[hashToken(refresh)] = &refreshToken{ClientID: clientID, Scope: scope, ExpiresAt: time.Now().Add(refreshTTL)}
	s.mu.Unlock()
	s.persistRefreshTokens()

	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    int(accessTokenTTL.Seconds()),
		"refresh_token": refresh,
		"scope":         scope,
	})
}

// --- helpers ---

func randomToken(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("oh-my-graph: encode auth response: %v", err)
	}
}

func writeOAuthError(w http.ResponseWriter, status int, code, desc string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
}
