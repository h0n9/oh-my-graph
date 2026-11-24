package viz

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/h0n9/oh-my-graph/internal/auth"
	"github.com/h0n9/oh-my-graph/internal/graph"
)

type fakeAgents struct {
	conns     []auth.Connection
	revoked   []string
	revokeErr error
	pruned    int
	pruneErr  error
	pruneN    int
}

func (f *fakeAgents) ListConnections() []auth.Connection { return f.conns }

func (f *fakeAgents) RevokeClient(id string) error {
	if f.revokeErr != nil {
		return f.revokeErr
	}
	for i, c := range f.conns {
		if c.ClientID == id {
			f.conns = append(f.conns[:i], f.conns[i+1:]...)
			f.revoked = append(f.revoked, id)
			return nil
		}
	}
	return auth.ErrClientNotFound
}

func (f *fakeAgents) PruneStale() (int, error) {
	f.pruned++
	return f.pruneN, f.pruneErr
}

var testNow = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

func sampleConns() []auth.Connection {
	return []auth.Connection{
		{ClientID: "c-active", Name: "Claude Code", ConnectedAt: testNow.Add(-113 * 24 * time.Hour), ActiveTokens: 2,
			LastUsed: testNow.Add(-2 * time.Minute), SessionExpiresAt: testNow.Add(89 * 24 * time.Hour), Status: auth.StatusActive},
		{ClientID: "c-idle", Name: "ChatGPT", ConnectedAt: testNow.Add(-113 * 24 * time.Hour), ActiveTokens: 1,
			LastUsed: testNow.Add(-34 * 24 * time.Hour), SessionExpiresAt: testNow.Add(33 * 24 * time.Hour), Status: auth.StatusIdle},
		{ClientID: "c-old", Name: "Codex", Status: auth.StatusExpired},
		{ClientID: "c-new", Name: "", Status: auth.StatusNoTokens},
	}
}

func newAgentsHandler(t *testing.T, f *fakeAgents) (*Handler, func(method, path, body, fetchSite string, want int) *httptest.ResponseRecorder) {
	t.Helper()
	mgr := graph.NewManager(t.TempDir())
	t.Cleanup(func() { mgr.Close() })
	h := NewHandler(mgr, WithAgents(f))
	h.now = func() time.Time { return testNow }
	request := func(method, path, body, fetchSite string, want int) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if fetchSite != "" {
			req.Header.Set("Sec-Fetch-Site", fetchSite)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("%s %s = %d, want %d: %s", method, path, rec.Code, want, rec.Body)
		}
		return rec
	}
	return h, request
}

func TestAgentsPageListsConnections(t *testing.T) {
	_, request := newAgentsHandler(t, &fakeAgents{conns: sampleConns()})
	rec := request("GET", "/agents", "", "", 200)
	body := rec.Body.String()

	for _, want := range []string{
		"Claude Code", "active", "ChatGPT", "idle 34d", "Codex", "expired", "(unnamed client)", "no tokens",
		"113 days ago", "2 minutes ago", "in 89 days", "unknown",
		`data-revoke="c-active"`, `data-removable="false"`, `data-revoke="c-old"`, `data-removable="true"`,
		`action="/agents/prune"`, `action="/agents/revoke"`, "2 inactive",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("agents page missing %q", want)
		}
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

func TestAgentsPageEmptyState(t *testing.T) {
	_, request := newAgentsHandler(t, &fakeAgents{})
	body := request("GET", "/agents", "", "", 200).Body.String()
	if !strings.Contains(body, "No connected agents.") {
		t.Fatal("empty state missing")
	}
	if strings.Contains(body, `action="/agents/prune"`) {
		t.Fatal("prune button shown with nothing to prune")
	}
}

func TestAgentsPageCollapsesInactiveWhenMany(t *testing.T) {
	var conns []auth.Connection
	for i := 0; i < 8; i++ {
		conns = append(conns, auth.Connection{ClientID: "x" + string(rune('a'+i)), Name: "n", Status: auth.StatusNoTokens})
	}
	_, request := newAgentsHandler(t, &fakeAgents{conns: conns})
	if body := request("GET", "/agents", "", "", 200).Body.String(); strings.Contains(body, "<details open>") {
		t.Fatal("inactive group should be collapsed with many agents")
	}
	_, request = newAgentsHandler(t, &fakeAgents{conns: sampleConns()})
	if body := request("GET", "/agents", "", "", 200).Body.String(); !strings.Contains(body, "<details open>") {
		t.Fatal("inactive group should be open with few agents")
	}
}

func TestAgentsPageEscapesClientName(t *testing.T) {
	conns := []auth.Connection{{ClientID: "c", Name: `<script>alert(1)</script>"x`, Status: auth.StatusActive, ActiveTokens: 1}}
	_, request := newAgentsHandler(t, &fakeAgents{conns: conns})
	body := request("GET", "/agents", "", "", 200).Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Fatal("client_name was not escaped")
	}
}

func TestAgentsRevoke(t *testing.T) {
	f := &fakeAgents{conns: sampleConns()}
	_, request := newAgentsHandler(t, f)

	request("GET", "/agents/revoke", "", "", 405)
	rec := request("PUT", "/agents/revoke", "client_id=c-active", "same-origin", 405)
	if rec.Header().Get("Allow") != "POST" {
		t.Errorf("Allow = %q, want POST", rec.Header().Get("Allow"))
	}
	request("POST", "/agents/revoke", "client_id=c-active", "cross-site", 403)
	request("POST", "/agents/revoke", "client_id=c-active", "same-site", 403)
	if len(f.revoked) != 0 {
		t.Fatal("blocked requests revoked a client")
	}
	request("POST", "/agents/revoke", "", "same-origin", 400)
	request("POST", "/agents/revoke", "client_id=missing", "same-origin", 404)

	rec = request("POST", "/agents/revoke", "client_id=c-active", "same-origin", 303)
	if got := rec.Header().Get("Location"); got != "/agents" {
		t.Fatalf("redirect = %q, want /agents", got)
	}
	if len(f.revoked) != 1 || f.revoked[0] != "c-active" {
		t.Fatalf("revoked = %v", f.revoked)
	}
	// A repeat (double submit) is reported as already revoked.
	body := request("POST", "/agents/revoke", "client_id=c-active", "same-origin", 404).Body.String()
	if !strings.Contains(body, "Agent already revoked.") {
		t.Fatal("404 page should say the agent was already revoked")
	}
}

func TestAgentsRevokePersistFailure(t *testing.T) {
	f := &fakeAgents{conns: sampleConns(), revokeErr: errors.New("disk full")}
	_, request := newAgentsHandler(t, f)
	body := request("POST", "/agents/revoke", "client_id=c-active", "same-origin", 500).Body.String()
	if !strings.Contains(body, "could not be saved") || strings.Contains(body, "disk full") {
		t.Fatalf("expected a safe save-failure message without internals: %s", body)
	}
}

func TestAgentsPrune(t *testing.T) {
	f := &fakeAgents{conns: sampleConns(), pruneN: 3}
	_, request := newAgentsHandler(t, f)

	request("GET", "/agents/prune", "", "", 405)
	request("POST", "/agents/prune", "", "cross-site", 403)
	if f.pruned != 0 {
		t.Fatal("blocked request pruned")
	}
	rec := request("POST", "/agents/prune", "", "same-origin", 303)
	if got := rec.Header().Get("Location"); got != "/agents?pruned=3" {
		t.Fatalf("redirect = %q", got)
	}
	body := request("GET", "/agents?pruned=3", "", "", 200).Body.String()
	if !strings.Contains(body, "Pruned 3 inactive agents.") {
		t.Fatal("prune notice missing")
	}
	if body := request("GET", "/agents?pruned=junk", "", "", 200).Body.String(); strings.Contains(body, "Pruned") || strings.Contains(body, "Nothing to prune") {
		t.Fatal("invalid pruned value must not render a notice")
	}
	if body := request("GET", "/agents?pruned=1", "", "", 200).Body.String(); !strings.Contains(body, "Pruned 1 inactive agent.") {
		t.Fatal("singular prune notice missing")
	}
	if body := request("GET", "/agents?pruned=0", "", "", 200).Body.String(); !strings.Contains(body, "Nothing to prune yet.") {
		t.Fatal("zero prune notice missing")
	}

	f.pruneErr = errors.New("disk full")
	body = request("POST", "/agents/prune", "", "same-origin", 500).Body.String()
	if !strings.Contains(body, "were pruned for now") || strings.Contains(body, "disk full") {
		t.Fatalf("prune failure body = %q", body)
	}
}

func TestAgentsPageNonGetIs405(t *testing.T) {
	_, request := newAgentsHandler(t, &fakeAgents{})
	rec := request("POST", "/agents", "", "same-origin", 405)
	if rec.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("Allow = %q", rec.Header().Get("Allow"))
	}
}

func TestIndexShowsAgentsLinkOnlyWhenEnabled(t *testing.T) {
	f := &fakeAgents{conns: sampleConns()} // 2 connections have live tokens
	_, request := newAgentsHandler(t, f)
	body := request("GET", "/", "", "", 200).Body.String()
	if !strings.Contains(body, `href="/agents"`) || !strings.Contains(body, "2 agents") {
		t.Fatalf("topics page should link to /agents with the live count: %s", body)
	}

	mgr := graph.NewManager(t.TempDir())
	defer mgr.Close()
	h := NewHandler(mgr) // no provider
	for _, path := range []string{"/agents", "/agents/revoke", "/agents/prune"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 404 {
			t.Errorf("%s without a provider = %d, want 404", path, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if strings.Contains(rec.Body.String(), "/agents") {
		t.Fatal("topics page links to /agents without a provider")
	}
}

// Moving the shared styles into shared.html must not blank or unstyle the
// existing topics page.
func TestTopicsPageStillRendersAfterSharedTemplateRefactor(t *testing.T) {
	mgr := graph.NewManager(t.TempDir())
	defer mgr.Close()
	_, release, err := mgr.OpenTopic("t1", true)
	if err != nil {
		t.Fatal(err)
	}
	release()
	rec := httptest.NewRecorder()
	NewHandler(mgr).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	body := rec.Body.String()
	if len(body) == 0 {
		t.Fatal("topics page body is empty")
	}
	for _, want := range []string{"icon-button", `aria-label="Rename t1"`, `aria-label="Archive t1"`, `id="archive-dialog"`, "--bg:", "manifest.json", "prefers-color-scheme: dark"} {
		if !strings.Contains(body, want) {
			t.Errorf("topics page missing %q", want)
		}
	}
}

func TestFormatRelative(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "just now"},
		{time.Minute, "1 minute ago"},
		{2 * time.Minute, "2 minutes ago"},
		{5 * time.Hour, "5 hours ago"},
		{47 * time.Hour, "47 hours ago"},
		{72 * time.Hour, "3 days ago"},
		{-30 * time.Second, "in under a minute"},
		{-89 * 24 * time.Hour, "in 89 days"},
	}
	for _, c := range cases {
		if got := formatRelative(testNow.Add(-c.d), testNow); got != c.want {
			t.Errorf("formatRelative(%v) = %q, want %q", c.d, got, c.want)
		}
	}
	if formatRelative(time.Time{}, testNow) != "unknown" || formatWhen(time.Time{}, testNow) != "unknown" {
		t.Error("zero time should render as unknown")
	}
}

func TestAgentsPageHeadAndLabels(t *testing.T) {
	_, request := newAgentsHandler(t, &fakeAgents{conns: sampleConns()})
	if rec := request("HEAD", "/agents", "", "", 200); rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("HEAD /agents should still send Cache-Control: no-store")
	}
	body := request("GET", "/agents", "", "", 200).Body.String()
	// Active/idle agents are offered Revoke; agents without a live session Remove.
	if strings.Count(body, ">Revoke<") != 2 || strings.Count(body, ">Remove<") != 2 {
		t.Errorf("expected 2 Revoke and 2 Remove buttons, body: %s", body)
	}
	if !strings.Contains(body, "Dates are UTC.") {
		t.Error("page should say dates are UTC")
	}
	if !strings.Contains(body, "Prune inactive agents") || !strings.Contains(body, "Prune removes inactive agents older than an hour.") {
		t.Error("with inactive agents, the page should offer pruning and explain it")
	}

	// No inactive agents: no prune link, and no sentence about a link that isn't there.
	_, request = newAgentsHandler(t, &fakeAgents{conns: sampleConns()[:2]})
	body = request("GET", "/agents", "", "", 200).Body.String()
	if strings.Contains(body, "Prune inactive agents") || strings.Contains(body, "Prune removes") {
		t.Errorf("no inactive agents: page should not offer or explain pruning, body: %s", body)
	}
}

// CrossOriginProtection lets requests with neither Sec-Fetch-Site nor Origin
// through (non-browser clients have no ambient session cookie to abuse); the
// owner-session wrapper in front of the handler still applies.
func TestAgentsRevokeAllowsRequestsWithoutBrowserHeaders(t *testing.T) {
	f := &fakeAgents{conns: sampleConns()}
	_, request := newAgentsHandler(t, f)
	request("POST", "/agents/revoke", "client_id=c-active", "", 303)
	if len(f.revoked) != 1 {
		t.Fatalf("revoked = %v", f.revoked)
	}
}
