// Package auth implements the MCP Authorization spec flow: OAuth 2.1
// Authorization Code + PKCE, RFC 7591 Dynamic Client Registration, and
// RFC 9728 / RFC 8414 discovery metadata. It is single-tenant — there is one
// owner, gated by a shared passphrase entered once per client at /authorize.
// DCR registration is unauthenticated by spec, so the passphrase (not
// registration) is the real access control.
//
// All state is in-memory: a process restart wipes every client, code, and
// token, forcing connected clients to silently re-authenticate.
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
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

const (
	authCodeTTL    = 60 * time.Second
	accessTokenTTL = time.Hour
	refreshTTL     = 90 * 24 * time.Hour
)

type Config struct {
	Issuer          string // public base URL, e.g. https://oh-my-graph-h0n9.sprites.app
	OwnerPassphrase string
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
	issuer          string
	ownerPassphrase string
	authorizeTmpl   *template.Template

	mu            sync.Mutex
	clients       map[string]*client
	codes         map[string]*authCode
	tokens        map[string]*accessToken
	refreshTokens map[string]*refreshToken
}

func NewServer(cfg Config) *Server {
	return &Server{
		issuer:          strings.TrimRight(cfg.Issuer, "/"),
		ownerPassphrase: cfg.OwnerPassphrase,
		authorizeTmpl:   template.Must(template.New("authorize").Parse(authorizeHTML)),
		clients:         make(map[string]*client),
		codes:           make(map[string]*authCode),
		tokens:          make(map[string]*accessToken),
		refreshTokens:   make(map[string]*refreshToken),
	}
}

func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/.well-known/oauth-protected-resource", s.handleProtectedResourceMetadata)
	mux.HandleFunc("/.well-known/oauth-authorization-server", s.handleAuthServerMetadata)
	mux.HandleFunc("/register", s.handleRegister)
	mux.HandleFunc("/authorize", s.handleAuthorize)
	mux.HandleFunc("/token", s.handleToken)
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
			s.unauthorized(w)
			return
		}

		tok := strings.TrimPrefix(authz, prefix)
		s.mu.Lock()
		at, ok := s.tokens[tok]
		s.mu.Unlock()
		if !ok || time.Now().After(at.ExpiresAt) {
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

// RequireOwnerBasicAuth gates browser-facing pages (which can't attach a
// Bearer header to a plain navigation) behind the same owner passphrase via
// HTTP Basic Auth, so browsers get a native login prompt.
func (s *Server) RequireOwnerBasicAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pass, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(pass), []byte(s.ownerPassphrase)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="oh-my-graph"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
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
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "malformed JSON body")
		return
	}
	if len(req.RedirectURIs) == 0 {
		writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uris is required")
		return
	}
	for _, u := range req.RedirectURIs {
		parsed, err := url.ParseRequestURI(u)
		if err != nil || parsed.Scheme == "" {
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

	writeJSON(w, http.StatusCreated, resp)
}

// --- /authorize ---

const authorizeHTML = `<!DOCTYPE html>
<html>
<head><meta charset="utf-8"><title>Authorize oh-my-graph</title>
<style>
body{font-family:system-ui,sans-serif;max-width:420px;margin:80px auto;padding:0 16px;color:#1a1a1a}
h1{font-size:1.1rem}
input[type=password]{width:100%;padding:8px;font-size:1rem;margin:12px 0;box-sizing:border-box}
button{padding:8px 16px;font-size:1rem;cursor:pointer}
.err{color:#b00020;margin-bottom:8px}
</style>
</head>
<body>
<h1>Authorize {{.ClientName}}</h1>
<p>Enter the owner passphrase to grant this client access to oh-my-graph.</p>
{{if .Error}}<p class="err">{{.Error}}</p>{{end}}
<form method="POST" action="/authorize">
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
		c, mustNotRedirect, errCode, errDesc := s.validateAuthorizeParams(p)
		if errCode != "" {
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
			http.Error(w, "invalid form body", http.StatusBadRequest)
			return
		}
		p := parseAuthorizeParams(r.PostForm)
		c, mustNotRedirect, errCode, errDesc := s.validateAuthorizeParams(p)
		if errCode != "" {
			if mustNotRedirect {
				http.Error(w, errDesc, http.StatusBadRequest)
				return
			}
			redirectWithError(w, r, p.RedirectURI, p.State, errCode, errDesc)
			return
		}

		passphrase := r.PostForm.Get("passphrase")
		if subtle.ConstantTimeCompare([]byte(passphrase), []byte(s.ownerPassphrase)) != 1 {
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

// --- /token ---

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "malformed form body")
		return
	}

	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		s.handleAuthorizationCodeGrant(w, r.PostForm)
	case "refresh_token":
		s.handleRefreshTokenGrant(w, r.PostForm)
	default:
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "supported grant types: authorization_code, refresh_token")
	}
}

func (s *Server) authenticateClient(form url.Values) (*client, string, string) {
	c, ok := s.lookupClient(form.Get("client_id"))
	if !ok {
		return nil, "invalid_client", "unknown client_id"
	}
	if c.TokenEndpointAuthMethod != "none" {
		if subtle.ConstantTimeCompare([]byte(form.Get("client_secret")), []byte(c.Secret)) != 1 {
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

func (s *Server) handleAuthorizationCodeGrant(w http.ResponseWriter, form url.Values) {
	c, errCode, desc := s.authenticateClient(form)
	if errCode != "" {
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
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "authorization code is invalid or expired")
		return
	}
	if ac.ClientID != c.ID || ac.RedirectURI != form.Get("redirect_uri") {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "client_id or redirect_uri mismatch")
		return
	}

	verifier := form.Get("code_verifier")
	if verifier == "" || oauth2.S256ChallengeFromVerifier(verifier) != ac.CodeChallenge {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
		return
	}

	s.issueTokenPair(w, c.ID, ac.Scope)
}

func (s *Server) handleRefreshTokenGrant(w http.ResponseWriter, form url.Values) {
	c, errCode, desc := s.authenticateClient(form)
	if errCode != "" {
		writeOAuthError(w, http.StatusUnauthorized, errCode, desc)
		return
	}

	rt := form.Get("refresh_token")
	s.mu.Lock()
	old, ok := s.refreshTokens[rt]
	if ok {
		delete(s.refreshTokens, rt) // rotate on every use per OAuth 2.1 guidance for public clients
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
	s.refreshTokens[refresh] = &refreshToken{ClientID: clientID, Scope: scope, ExpiresAt: time.Now().Add(refreshTTL)}
	s.mu.Unlock()

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
