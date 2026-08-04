package auth

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestSanitizeNext(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", "/"},
		{"plain path", "/foo", "/foo"},
		{"path with query", "/foo?x=1", "/foo?x=1"},
		{"protocol relative", "//evil.com", "/"},
		{"triple slash still protocol relative", "///evil.com", "/"},
		{"absolute url", "https://evil.com", "/"},
		{"bare host no leading slash", "evil.com", "/"},
		// Regression for the backslash bypass: some browsers normalize a
		// leading "\" to "/" when resolving a relative URL, so "/\evil.com"
		// would otherwise behave like "//evil.com" despite starting with a
		// single "/".
		{"backslash bypass", `/\evil.com`, "/"},
		{"double backslash, no leading slash", `\\evil.com`, "/"},
		// A backslash later in the path (not leading) isn't a browser
		// normalization vector -- only a *leading* "\"/"/" sequence gets
		// treated as protocol-relative, so this is a safe on-site path and
		// should pass through unchanged.
		{"backslash later in path is not a redirect vector", `/a/\evil.com`, `/a/\evil.com`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sanitizeNext(c.in); got != c.want {
				t.Errorf("sanitizeNext(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestHasValidSession(t *testing.T) {
	srv := newTestServer(t, testConfig(t.TempDir(), "secret"))

	t.Run("no cookie", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/graph", nil)
		if srv.hasValidSession(r) {
			t.Fatal("expected hasValidSession to be false with no cookie")
		}
	})

	t.Run("unknown session id", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/graph", nil)
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "not-a-real-session"})
		if srv.hasValidSession(r) {
			t.Fatal("expected hasValidSession to be false for unknown session id")
		}
	})

	t.Run("valid unexpired session", func(t *testing.T) {
		srv.mu.Lock()
		srv.sessions["valid-session"] = time.Now().Add(time.Hour)
		srv.mu.Unlock()

		r := httptest.NewRequest(http.MethodGet, "/graph", nil)
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "valid-session"})
		if !srv.hasValidSession(r) {
			t.Fatal("expected hasValidSession to be true for valid unexpired session")
		}
	})

	t.Run("expired session", func(t *testing.T) {
		srv.mu.Lock()
		srv.sessions["expired-session"] = time.Now().Add(-time.Hour)
		srv.mu.Unlock()

		r := httptest.NewRequest(http.MethodGet, "/graph", nil)
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "expired-session"})
		if srv.hasValidSession(r) {
			t.Fatal("expected hasValidSession to be false for expired session")
		}
	})
}

func TestRequireOwnerSession(t *testing.T) {
	srv := newTestServer(t, testConfig(t.TempDir(), "secret"))

	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	sentinel := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		w.Write([]byte("reached next handler"))
	})
	mux.Handle("/graph", srv.RequireOwnerSession(sentinel))
	mux.Handle("/api/graph", srv.RequireOwnerSession(sentinel))

	ts := httptest.NewServer(mux)
	defer ts.Close()
	client := noRedirectClient()

	t.Run("no cookie, normal path redirects to login", func(t *testing.T) {
		resp, err := client.Get(ts.URL + "/graph?topic=finance")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusFound)
		}
		loc := resp.Header.Get("Location")
		if !strings.HasPrefix(loc, "/login?next=") {
			t.Fatalf("Location = %q, want prefix /login?next=", loc)
		}
		u, err := url.Parse(loc)
		if err != nil {
			t.Fatal(err)
		}
		if got := u.Query().Get("next"); got != "/graph?topic=finance" {
			t.Errorf("next param = %q, want /graph?topic=finance", got)
		}
	})

	t.Run("no cookie, /api/graph gets plain 401", func(t *testing.T) {
		resp, err := client.Get(ts.URL + "/api/graph")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
		}
	})

	t.Run("valid session reaches next handler", func(t *testing.T) {
		srv.mu.Lock()
		srv.sessions["good-session"] = time.Now().Add(time.Hour)
		srv.mu.Unlock()

		req, err := http.NewRequest(http.MethodGet, ts.URL+"/graph", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "good-session"})
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusTeapot {
			t.Fatalf("status = %d, want %d (next handler reached)", resp.StatusCode, http.StatusTeapot)
		}
	})

	t.Run("expired session treated like no session", func(t *testing.T) {
		srv.mu.Lock()
		srv.sessions["stale-session"] = time.Now().Add(-time.Hour)
		srv.mu.Unlock()

		req, err := http.NewRequest(http.MethodGet, ts.URL+"/graph", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "stale-session"})
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusFound)
		}
	})
}

// TestHandleLogin also covers renderLoginForm, which has no independent
// behavior worth isolating beyond what these GET/error-path assertions
// already exercise.
func TestHandleLogin(t *testing.T) {
	newServer := func(t *testing.T) (*Server, string) {
		t.Helper()
		srv := newTestServer(t, testConfig(t.TempDir(), "correct-passphrase"))
		mux := http.NewServeMux()
		srv.RegisterRoutes(mux)
		ts := httptest.NewServer(mux)
		t.Cleanup(ts.Close)
		return srv, ts.URL
	}

	t.Run("GET with no next renders form with default next", func(t *testing.T) {
		_, base := newServer(t)
		resp, err := http.Get(base + "/login")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		body := readBody(t, resp)
		if !strings.Contains(body, `name="next" value="/"`) {
			t.Errorf("body missing default next field, got: %s", body)
		}
	})

	t.Run("GET with next echoes sanitized value", func(t *testing.T) {
		_, base := newServer(t)
		resp, err := http.Get(base + "/login?next=" + url.QueryEscape("/foo"))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body := readBody(t, resp)
		if !strings.Contains(body, `name="next" value="/foo"`) {
			t.Errorf("body missing echoed next field, got: %s", body)
		}
	})

	t.Run("GET with malicious next is sanitized before render", func(t *testing.T) {
		_, base := newServer(t)
		resp, err := http.Get(base + "/login?next=" + url.QueryEscape("//evil.com"))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body := readBody(t, resp)
		if !strings.Contains(body, `name="next" value="/"`) {
			t.Errorf("expected sanitized next=/ in body, got: %s", body)
		}
		if strings.Contains(body, "evil.com") {
			t.Errorf("body should not contain evil.com, got: %s", body)
		}
	})

	t.Run("POST wrong passphrase re-renders form without setting cookie", func(t *testing.T) {
		_, base := newServer(t)
		resp, err := http.PostForm(base+"/login", url.Values{"passphrase": {"wrong"}})
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if len(resp.Cookies()) != 0 {
			t.Errorf("expected no cookies set on failed login, got %v", resp.Cookies())
		}
		body := readBody(t, resp)
		if !strings.Contains(body, "Incorrect passphrase") {
			t.Errorf("expected error message in body, got: %s", body)
		}
	})

	t.Run("POST correct passphrase sets session cookie and redirects", func(t *testing.T) {
		_, base := newServer(t)
		client := noRedirectClient()
		resp, err := client.PostForm(base+"/login", url.Values{"passphrase": {"correct-passphrase"}})
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusSeeOther)
		}
		if got := resp.Header.Get("Location"); got != "/" {
			t.Errorf("Location = %q, want /", got)
		}
		cookies := resp.Cookies()
		if len(cookies) != 1 {
			t.Fatalf("expected exactly 1 cookie, got %d", len(cookies))
		}
		c := cookies[0]
		if c.Name != sessionCookieName {
			t.Errorf("cookie name = %q, want %q", c.Name, sessionCookieName)
		}
		if !c.HttpOnly {
			t.Error("expected cookie to be HttpOnly")
		}
		if c.SameSite != http.SameSiteLaxMode {
			t.Errorf("SameSite = %v, want Lax", c.SameSite)
		}
		if c.Secure {
			t.Error("expected Secure=false for http:// test issuer")
		}
	})

	t.Run("POST correct passphrase redirects to sanitized next", func(t *testing.T) {
		_, base := newServer(t)
		client := noRedirectClient()
		resp, err := client.PostForm(base+"/login", url.Values{
			"passphrase": {"correct-passphrase"},
			"next":       {"/foo"},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if got := resp.Header.Get("Location"); got != "/foo" {
			t.Errorf("Location = %q, want /foo", got)
		}
	})

	t.Run("POST correct passphrase with backslash-bypass next redirects safely", func(t *testing.T) {
		_, base := newServer(t)
		client := noRedirectClient()
		resp, err := client.PostForm(base+"/login", url.Values{
			"passphrase": {"correct-passphrase"},
			"next":       {`/\evil.com`},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if got := resp.Header.Get("Location"); got != "/" {
			t.Errorf("Location = %q, want / (open redirect must be blocked)", got)
		}
	})

	t.Run("POST with malformed form body returns 400", func(t *testing.T) {
		_, base := newServer(t)
		// Invalid percent-encoding makes url.ParseQuery (called internally by
		// ParseForm) fail.
		req, err := http.NewRequest(http.MethodPost, base+"/login", strings.NewReader("passphrase=%zz"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
		}
	})

	t.Run("PUT is not allowed", func(t *testing.T) {
		_, base := newServer(t)
		req, err := http.NewRequest(http.MethodPut, base+"/login", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
		}
	})
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
