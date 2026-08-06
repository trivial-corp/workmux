package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/trivial-corp/workmux/internal/project"
	"github.com/trivial-corp/workmux/internal/testrepo"
	"github.com/trivial-corp/workmux/internal/work"
)

// serving builds a server over real repositories, one project per name.
func serving(t *testing.T, token string, names ...string) (*Server, http.Handler, []string) {
	t.Helper()
	var roots []string
	for _, name := range names {
		roots = append(roots, testrepo.New(t, name).Root)
	}
	set, err := project.New(roots)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		Projects: set,
		Terminal: true,
		Token:    token,
		Origins:  []string{"http://127.0.0.1:4315"},
	}
	return s, s.Handler(), roots
}

func server(t *testing.T, token string) (*Server, http.Handler) {
	t.Helper()
	s, h, _ := serving(t, token, "proj")
	return s, h
}

func TestWorkEndpoint(t *testing.T) {
	_, h := server(t, "")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/work", nil))

	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q", ct)
	}
	var v work.Overview
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("body is not the overview: %v", err)
	}
	if len(v.Projects) != 1 || v.Projects[0].Name != "proj" || len(v.Work) != 1 {
		t.Errorf("overview = %+v", v)
	}
	if v.Work[0].Project != v.Projects[0].ID {
		t.Errorf("work[0].project = %q, want %q", v.Work[0].Project, v.Projects[0].ID)
	}
	// Absent things must serialise as empty lists, not null: the frontend and the
	// mobile app both iterate these without checking.
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	item := raw["work"].([]any)[0].(map[string]any)
	for _, key := range []string{"agents", "sessions", "prs"} {
		if item[key] == nil {
			t.Errorf("work[0].%s is null, want []", key)
		}
	}
}

func TestServesTheEmbeddedUI(t *testing.T) {
	_, h := server(t, "")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("content-type = %q", ct)
	}
	if body := rec.Body.String(); len(body) < 200 || !contains(body, "workmux") {
		t.Errorf("body looks wrong (%d bytes)", len(body))
	}
	// index.html must never be cached, or a deploy is invisible until a hard reload.
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("cache-control = %q, want no-store", cc)
	}
}

// An unknown /api/ path is a mistake worth reporting as one; anything else is a
// client-side route and gets the app.
func TestUnknownPaths(t *testing.T) {
	_, h := server(t, "")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/nope", nil))
	if rec.Code != 404 {
		t.Errorf("api 404 = %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/some/deep/route", nil))
	if rec.Code != 200 {
		t.Errorf("client route = %d, want the app", rec.Code)
	}
}

// The one auth rule with its one exception.
func TestTokenIsRequiredOffLoopbackOnly(t *testing.T) {
	s, h := server(t, "sekret")

	req := httptest.NewRequest("GET", "/api/work", nil)
	req.RemoteAddr = "192.168.1.50:5000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("off-box without a token = %d, want 401", rec.Code)
	}

	// Loopback stays exempt: the browser is already on the machine.
	req = httptest.NewRequest("GET", "/api/work", nil)
	req.RemoteAddr = "127.0.0.1:5000"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("loopback = %d, want 200", rec.Code)
	}

	// Every way a client can present it.
	for _, tc := range []struct {
		name  string
		apply func(*http.Request)
	}{
		{"header", func(r *http.Request) { r.Header.Set("X-Workmux-Token", s.Token) }},
		{"bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+s.Token) }},
		{"cookie", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "workmux", Value: s.Token}) }},
	} {
		req = httptest.NewRequest("GET", "/api/work", nil)
		req.RemoteAddr = "192.168.1.50:5000"
		tc.apply(req)
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Errorf("%s = %d, want 200", tc.name, rec.Code)
		}
	}

	// A wrong token is not "no token".
	req = httptest.NewRequest("GET", "/api/work", nil)
	req.RemoteAddr = "192.168.1.50:5000"
	req.Header.Set("X-Workmux-Token", "wrong")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token = %d, want 401", rec.Code)
	}
}

// ?t=… is how a phone gets in; it must become a cookie so the token stops
// travelling in URLs (and referrers) after the first load.
func TestQueryTokenBecomesACookie(t *testing.T) {
	s, h := server(t, "sekret")
	req := httptest.NewRequest("GET", "/?t="+s.Token, nil)
	req.RemoteAddr = "192.168.1.50:5000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("got %d cookies, want 1", len(cookies))
	}
	c := cookies[0]
	if c.Name != "workmux" || c.Value != s.Token {
		t.Errorf("cookie = %+v", c)
	}
	if !c.HttpOnly {
		t.Error("the cookie must be HttpOnly — scripts have no business reading it")
	}
}

// An empty token disables the check, for when something in front authenticates.
func TestEmptyTokenMeansNoCheck(t *testing.T) {
	_, h := server(t, "")
	req := httptest.NewRequest("GET", "/api/work", nil)
	req.RemoteAddr = "8.8.8.8:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("code = %d, want 200", rec.Code)
	}
}

// CORS does not cover WebSockets, so the upgrade needs its own origin rule:
// without it, any page you visited could open one against localhost.
func TestOriginAllowlist(t *testing.T) {
	s, _ := server(t, "")
	if !s.OriginOK("http://127.0.0.1:4315") {
		t.Error("our own origin must be allowed")
	}
	if s.OriginOK("http://evil.example") {
		t.Error("a foreign origin must be refused")
	}
	// A non-browser client (curl, the mobile app) sends no Origin at all.
	if !s.OriginOK("") {
		t.Error("an absent origin is not a browser and must be allowed")
	}
}

// Health is deliberately outside the token check, so a proxy can probe it.
func TestHealthNeedsNoToken(t *testing.T) {
	_, h := server(t, "sekret")
	req := httptest.NewRequest("GET", "/api/health", nil)
	req.RemoteAddr = "192.168.1.50:5000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("code = %d, want 200", rec.Code)
	}
}

func TestConfigEndpoint(t *testing.T) {
	_, h := server(t, "")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/p/proj/config", nil))
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["name"] != "proj" {
		t.Errorf("config = %+v", got)
	}
	if _, ok := got["stack"]; !ok {
		t.Error("stack must be present (as null) so a client can tell")
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
