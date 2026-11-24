package auth

import (
	"errors"
	"sort"
	"time"
)

const (
	// idleAfter is how long an otherwise-live connection can go unused before
	// it is reported as idle.
	idleAfter = 24 * time.Hour
	// pruneGrace protects a registration that is probably mid-authorization:
	// a freshly registered (or freshly /authorize-d) client has no tokens until
	// the owner finishes typing the passphrase.
	pruneGrace = time.Hour
)

// ErrClientNotFound is returned by RevokeClient for an unknown client ID.
var ErrClientNotFound = errors.New("client not found")

// ConnectionStatus summarises a client's connection for display.
type ConnectionStatus string

const (
	StatusActive   ConnectionStatus = "active"    // has live tokens, used recently or last use unknown
	StatusIdle     ConnectionStatus = "idle"      // has live tokens but unused for more than idleAfter
	StatusExpired  ConnectionStatus = "expired"   // every refresh token has expired
	StatusNoTokens ConnectionStatus = "no-tokens" // registered but never completed authorization
)

// Connection is the read-only view of one registered client that the
// Connected Agents page renders. Zero times mean "unknown" (e.g. tokens
// persisted before the metadata existed).
type Connection struct {
	ClientID     string
	Name         string
	RegisteredAt time.Time
	ConnectedAt  time.Time // earliest original authorization among its tokens
	ActiveTokens int       // refresh tokens that have not expired
	LastUsed     time.Time
	// SessionExpiresAt is the latest refresh-token expiry among live tokens,
	// i.e. when the client must re-authorize if it stops refreshing.
	SessionExpiresAt time.Time
	Status           ConnectionStatus
}

// ListConnections returns one Connection per registered client, most recently
// used first (never-used last), then by name and ID for a stable order. Tokens
// whose client is no longer registered are ignored.
func (s *Server) ListConnections() []Connection {
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	type agg struct {
		total, active int
		latestExpiry  time.Time
		connectedAt   time.Time
		lastUsed      time.Time
	}
	aggs := make(map[string]*agg, len(s.clients))
	for _, rt := range s.refreshTokens {
		a := aggs[rt.ClientID]
		if a == nil {
			a = &agg{}
			aggs[rt.ClientID] = a
		}
		a.total++
		if !now.After(rt.ExpiresAt) {
			a.active++
			if rt.ExpiresAt.After(a.latestExpiry) {
				a.latestExpiry = rt.ExpiresAt
			}
		}
		if !rt.ConnectedAt.IsZero() && (a.connectedAt.IsZero() || rt.ConnectedAt.Before(a.connectedAt)) {
			a.connectedAt = rt.ConnectedAt
		}
		if rt.LastUsedAt.After(a.lastUsed) {
			a.lastUsed = rt.LastUsedAt
		}
	}

	out := make([]Connection, 0, len(s.clients))
	for id, c := range s.clients {
		a := aggs[id]
		if a == nil {
			a = &agg{}
		}
		lastUsed := a.lastUsed
		if seen := s.lastSeen[id]; seen.After(lastUsed) {
			lastUsed = seen
		}

		status := StatusActive
		switch {
		case a.total == 0:
			status = StatusNoTokens
		case a.active == 0:
			status = StatusExpired
		case !lastUsed.IsZero() && now.Sub(lastUsed) > idleAfter:
			status = StatusIdle
		}

		conn := Connection{
			ClientID:     id,
			Name:         c.Name,
			RegisteredAt: c.CreatedAt,
			ConnectedAt:  a.connectedAt,
			ActiveTokens: a.active,
			LastUsed:     lastUsed,
			Status:       status,
		}
		if a.active > 0 {
			conn.SessionExpiresAt = a.latestExpiry
		}
		out = append(out, conn)
	}

	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if !a.LastUsed.Equal(b.LastUsed) {
			return a.LastUsed.After(b.LastUsed) // zero (never used) sorts last
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.ClientID < b.ClientID
	})
	return out
}

// removeClientLocked deletes a client's registration and every credential and
// bookkeeping entry tied to it. The caller must hold s.mu.
func (s *Server) removeClientLocked(id string) {
	delete(s.clients, id)
	for tok, at := range s.tokens {
		if at.ClientID == id {
			delete(s.tokens, tok)
		}
	}
	for hash, rt := range s.refreshTokens {
		if rt.ClientID == id {
			delete(s.refreshTokens, hash)
		}
	}
	for code, ac := range s.codes {
		if ac.ClientID == id {
			delete(s.codes, code)
		}
	}
	delete(s.lastSeen, id)
}

// persistAll writes refresh tokens first, then clients, and reports every
// failure. Both are attempted even if the first fails, so a revocation reaches
// disk as far as possible. If only the tokens land, a restart can bring the
// registration back without tokens, which is harmless: the client still has to
// re-authorize with the owner passphrase.
func (s *Server) persistAll() error {
	return errors.Join(s.persistRefreshTokens(), s.persistClients())
}

// RevokeClient immediately invalidates everything the client holds (access and
// refresh tokens, pending authorization codes) and removes its registration,
// then persists. In-flight requests that already passed RequireBearer finish
// normally; every later request is rejected. It returns ErrClientNotFound for
// an unknown ID, and a persistence error if the revocation did not reach disk
// (the in-memory revocation has still happened).
//
// Removing the registration means a client that cannot recover from an unknown
// client_id may need to be removed and re-added by the user.
func (s *Server) RevokeClient(id string) error {
	s.mu.Lock()
	if _, ok := s.clients[id]; !ok {
		s.mu.Unlock()
		return ErrClientNotFound
	}
	s.removeClientLocked(id)
	s.mu.Unlock()

	// Persist after releasing mu: the persist functions take it themselves and
	// sync.Mutex is not reentrant.
	return s.persistAll()
}

// PruneStale removes registrations that are dead weight: no unexpired access
// or refresh token, no pending authorization code, and neither registered nor
// last seen at /authorize within pruneGrace (so a client mid-authorization is
// never removed). It returns how many were removed, persisting only if any
// were.
func (s *Server) PruneStale() (int, error) {
	now := time.Now()

	s.mu.Lock()
	live := make(map[string]bool)
	for _, at := range s.tokens {
		if !now.After(at.ExpiresAt) {
			live[at.ClientID] = true
		}
	}
	for _, rt := range s.refreshTokens {
		if !now.After(rt.ExpiresAt) {
			live[rt.ClientID] = true
		}
	}
	for _, ac := range s.codes {
		if !now.After(ac.ExpiresAt) {
			live[ac.ClientID] = true
		}
	}

	var stale []string
	for id, c := range s.clients {
		if live[id] {
			continue
		}
		recent := c.CreatedAt
		if c.LastAuthorizeAt.After(recent) {
			recent = c.LastAuthorizeAt
		}
		if now.Sub(recent) <= pruneGrace {
			continue
		}
		stale = append(stale, id)
	}
	for _, id := range stale {
		s.removeClientLocked(id) // also drops the client's expired tokens
	}
	s.mu.Unlock()

	if len(stale) == 0 {
		return 0, nil
	}
	return len(stale), s.persistAll()
}
