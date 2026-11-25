package viz

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/h0n9/oh-my-graph/internal/graph"
)

// formHandler serves both the topic and the agent form routes, so one table
// covers what acceptFormPost does for each of them.
func formHandler(t *testing.T) *Handler {
	t.Helper()
	mgr := graph.NewManager(t.TempDir())
	t.Cleanup(func() { mgr.Close() })
	return NewHandler(mgr, WithAgents(&fakeAgents{conns: sampleConns()}))
}

func formRequest(h *Handler, method, path, body, fetchSite string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if fetchSite != "" {
		req.Header.Set("Sec-Fetch-Site", fetchSite)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestFormRoutesRejectNonPost(t *testing.T) {
	h := formHandler(t)
	for _, path := range []string{"/topics/rename", "/topics/archive", "/topics/restore", "/topics/delete", "/agents/revoke", "/agents/prune"} {
		// A GET with a body must still be a 405, even when it is cross-site and oversized.
		rec := formRequest(h, "GET", path, "topic="+strings.Repeat("a", maxFormBytes+1), "cross-site")
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
			t.Errorf("GET %s = %d Allow=%q, want 405 Allow=POST", path, rec.Code, rec.Header().Get("Allow"))
		}
	}
}

func TestFormRoutesRejectCrossOrigin(t *testing.T) {
	h := formHandler(t)
	for _, tc := range []struct{ path, want string }{
		{"/topics/archive", "cross-origin topic changes are forbidden"},
		{"/topics/delete", "cross-origin topic changes are forbidden"},
		{"/agents/revoke", "cross-origin agent changes are forbidden"},
		{"/agents/prune", "cross-origin agent changes are forbidden"},
	} {
		// The cross-origin check must run before the body is read, so an
		// oversized cross-site body is a 403, not a 400.
		for _, body := range []string{"topic=x", "topic=" + strings.Repeat("a", maxFormBytes+1)} {
			rec := formRequest(h, "POST", tc.path, body, "cross-site")
			if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), tc.want) {
				t.Errorf("POST %s (%d bytes) = %d %q, want 403 %q", tc.path, len(body), rec.Code, rec.Body.String(), tc.want)
			}
		}
	}
}

func TestFormRoutesRejectUnreadableForm(t *testing.T) {
	h := formHandler(t)
	// The oversized body carries valid fields, so without the size limit the
	// request would succeed (or fail differently) instead of returning 400.
	oversized := "topic=x&new_name=y&client_id=c-active&pad=" + strings.Repeat("a", maxFormBytes)
	malformed := "topic=%zz&client_id=c-active" // invalid percent-encoding
	for _, tc := range []struct {
		path, body, want string
	}{
		{"/topics/rename", oversized, "Invalid topic form."},
		{"/topics/rename", malformed, "Invalid topic form."},
		// restore and delete render the Archived tab, so the closure must
		// have captured archived=true.
		{"/topics/restore", oversized, `href="/topics?tab=archived" aria-current="page"`},
		{"/topics/delete", malformed, `href="/topics?tab=archived" aria-current="page"`},
		{"/agents/revoke", oversized, "Invalid agent form."},
		{"/agents/revoke", malformed, "Invalid agent form."},
		{"/agents/prune", oversized, "Invalid agent form."},
	} {
		rec := formRequest(h, "POST", tc.path, tc.body, "same-origin")
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("POST %s = %d, want 400 containing %q: %s", tc.path, rec.Code, tc.want, rec.Body.String())
		}
	}
	// The agents error page is the agents page, not the topics list.
	rec := formRequest(h, "POST", "/agents/revoke", oversized, "same-origin")
	if !strings.Contains(rec.Body.String(), "connected agents") {
		t.Error("agents form error should render the agents page")
	}
}
