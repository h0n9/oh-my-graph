package viz

import (
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/h0n9/oh-my-graph/internal/auth"
)

var agentsTmpl = template.Must(template.ParseFS(staticFS, "static/agents.html", "static/shared.html"))

// AgentManager is the slice of the auth server the Connected Agents page
// needs. It is only available when the server runs with --auth.
type AgentManager interface {
	ListConnections() []auth.Connection
	RevokeClient(id string) error
	PruneStale() (int, error)
}

// Option configures a Handler.
type Option func(*Handler)

// WithAgents enables the Connected Agents page (/agents, /agents/revoke,
// /agents/prune) and the link to it from the topics page. Without it those
// routes return 404.
func WithAgents(m AgentManager) Option {
	return func(h *Handler) { h.agents = m }
}

type agentView struct {
	ClientID    string
	Name        string
	StatusLabel string
	StatusClass string
	Connected   string
	LastUsed    string
	Session     string
	Removable   bool // no live session: offered as "Remove" rather than "Revoke"
	Inactive    bool
}

type agentsView struct {
	Error        string
	Notice       string
	Live         []agentView
	Inactive     []agentView
	InactiveOpen bool
}

// inactiveOpenMax is how many agents in total the page shows with the inactive
// group expanded; beyond it the group is collapsed.
const inactiveOpenMax = 6

func (h *Handler) buildAgentsView(errMsg, notice string) agentsView {
	view := agentsView{Error: errMsg, Notice: notice}
	conns := h.agents.ListConnections()
	now := h.now()
	for _, c := range conns {
		a := toAgentView(c, now)
		if a.Inactive {
			view.Inactive = append(view.Inactive, a)
		} else {
			view.Live = append(view.Live, a)
		}
	}
	view.InactiveOpen = len(conns) <= inactiveOpenMax
	return view
}

func toAgentView(c auth.Connection, now time.Time) agentView {
	a := agentView{
		ClientID:  c.ClientID,
		Name:      c.Name,
		Connected: formatWhen(c.ConnectedAt, now),
		LastUsed:  formatRelative(c.LastUsed, now),
		Session:   "—",
	}
	if a.Name == "" {
		a.Name = "(unnamed client)"
	}
	switch c.Status {
	case auth.StatusActive:
		a.StatusLabel, a.StatusClass = "active", "ok"
	case auth.StatusIdle:
		days := int(now.Sub(c.LastUsed) / (24 * time.Hour))
		a.StatusLabel, a.StatusClass = fmt.Sprintf("idle %dd", days), "warn"
	case auth.StatusExpired:
		a.StatusLabel, a.StatusClass = "expired", "bad"
		a.Session = "expired"
		a.Removable, a.Inactive = true, true
	default:
		a.StatusLabel, a.StatusClass = "no tokens", "bad"
		a.Removable, a.Inactive = true, true
	}
	if !c.SessionExpiresAt.IsZero() {
		a.Session = formatWhen(c.SessionExpiresAt, now)
	}
	return a
}

// formatWhen renders "2026-08-03 · 113 days ago", or "unknown" for a zero time.
func formatWhen(t, now time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.UTC().Format("2006-01-02") + " · " + formatRelative(t, now)
}

func formatRelative(t, now time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := now.Sub(t)
	future := d < 0
	if future {
		d = -d
	}
	var n int
	var unit string
	switch {
	case d < time.Minute:
		if future {
			return "in under a minute"
		}
		return "just now"
	case d < time.Hour:
		n, unit = int(d/time.Minute), "minute"
	case d < 48*time.Hour:
		n, unit = int(d/time.Hour), "hour"
	default:
		n, unit = int(d/(24*time.Hour)), "day"
	}
	if n != 1 {
		unit += "s"
	}
	if future {
		return fmt.Sprintf("in %d %s", n, unit)
	}
	return fmt.Sprintf("%d %s ago", n, unit)
}

// agentCount is the number of agents holding a live session, shown on the
// topics page.
func (h *Handler) agentCount() int {
	n := 0
	for _, c := range h.agents.ListConnections() {
		if c.ActiveTokens > 0 {
			n++
		}
	}
	return n
}

func (h *Handler) serveAgents(w http.ResponseWriter, status int, errMsg, notice string) {
	view := h.buildAgentsView(errMsg, notice)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := agentsTmpl.Execute(w, view); err != nil {
		log.Printf("oh-my-graph: render agents: %v", err)
	}
}

func (h *Handler) agentsPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	notice := ""
	if v := r.URL.Query().Get("pruned"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 1_000_000 {
			switch n {
			case 0:
				notice = "Nothing to prune yet. Inactive agents from the last hour are kept."
			case 1:
				notice = "Pruned 1 inactive agent."
			default:
				notice = fmt.Sprintf("Pruned %d inactive agents.", n)
			}
		}
	}
	h.serveAgents(w, http.StatusOK, "", notice)
}

func (h *Handler) changeAgents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := h.csrf.Check(r); err != nil {
		http.Error(w, "cross-origin agent changes are forbidden", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		h.serveAgents(w, http.StatusBadRequest, "Invalid agent form.", "")
		return
	}

	target := "/agents"
	switch r.URL.Path {
	case "/agents/revoke":
		id := r.PostForm.Get("client_id")
		if id == "" {
			h.serveAgents(w, http.StatusBadRequest, "Invalid agent form.", "")
			return
		}
		if err := h.agents.RevokeClient(id); err != nil {
			if errors.Is(err, auth.ErrClientNotFound) {
				h.serveAgents(w, http.StatusNotFound, "Agent already revoked.", "")
				return
			}
			// The revocation took effect in memory; only saving it failed.
			log.Printf("oh-my-graph: revoke agent: %v", err)
			h.serveAgents(w, http.StatusInternalServerError, "Agent revoked for now, but the change could not be saved; it may return after a restart.", "")
			return
		}
	case "/agents/prune":
		n, err := h.agents.PruneStale()
		if err != nil {
			log.Printf("oh-my-graph: prune agents: %v", err)
			h.serveAgents(w, http.StatusInternalServerError, "Inactive agents were pruned for now, but the change could not be saved; they may return after a restart.", "")
			return
		}
		target = "/agents?pruned=" + strconv.Itoa(n)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}
