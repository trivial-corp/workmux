package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/trivial-corp/workmux/internal/agents"
	"github.com/trivial-corp/workmux/internal/config"
	"github.com/trivial-corp/workmux/internal/presets"
	"github.com/trivial-corp/workmux/internal/term"
	"github.com/trivial-corp/workmux/internal/testrepo"
	"github.com/trivial-corp/workmux/internal/work"
)

// withSessions builds a server that hands out terminals in one real worktree.
func withSessions(t *testing.T) (*Server, http.Handler, string, *term.Registry) {
	t.Helper()
	r := testrepo.New(t, "proj")
	cfg, err := config.Load(r.Root)
	if err != nil {
		t.Fatal(err)
	}
	reg := term.NewRegistry()
	t.Cleanup(reg.Shutdown)
	p := presets.Deps{Cfg: cfg}
	s := &Server{
		Cfg:     cfg,
		Builder: &work.Builder{Cfg: cfg, Agents: &agents.Reader{}, Terminal: true},
		Origins: []string{"http://127.0.0.1:4315"},
		Sessions: &Sessions{
			Reg:      reg,
			Presets:  p.Spec,
			KnownDir: func(dir string) bool { return dir == r.Root },
		},
	}
	return s, s.Handler(), r.Root, reg
}

func post(t *testing.T, h http.Handler, path string, body any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", path, strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestSessionLifecycle(t *testing.T) {
	_, h, root, reg := withSessions(t)

	code, got := post(t, h, "/api/session/new", map[string]any{
		"kind": "shell", "cwd": root, "cols": 100, "rows": 30})
	if code != 200 {
		t.Fatalf("new = %d %v", code, got)
	}
	id, _ := got["id"].(string)
	if id == "" {
		t.Fatalf("no id: %v", got)
	}
	if got["cols"].(float64) != 100 || got["rows"].(float64) != 30 {
		t.Errorf("the requested size didn't reach the pty: %v", got)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/session/list", nil))
	var list struct {
		Sessions []term.Info `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Sessions) != 1 || list.Sessions[0].ID != id {
		t.Fatalf("list = %+v", list.Sessions)
	}

	if code, got := post(t, h, "/api/session/kill", map[string]any{"id": id}); code != 200 {
		t.Fatalf("kill = %d %v", code, got)
	}
	sess, _ := reg.Get(id)
	if sess != nil && !sess.WaitExit(10e9) {
		t.Error("the session survived being killed")
	}
}

// The one thing this must never do: hand out a shell somewhere that isn't the
// repository's own worktree.
func TestSessionRefusesForeignDirectories(t *testing.T) {
	_, h, _, _ := withSessions(t)
	for _, dir := range []string{"/etc", "/", "", "/tmp"} {
		code, got := post(t, h, "/api/session/new", map[string]any{"kind": "shell", "cwd": dir})
		if code == 200 {
			t.Errorf("%q was accepted: %v", dir, got)
		}
		if msg, _ := got["error"].(string); !strings.Contains(msg, "worktree") {
			t.Errorf("%q: error = %q, want it to say why", dir, msg)
		}
	}
}

// A kind this project can't do returns the reason, and starts nothing.
func TestSessionKindUnavailable(t *testing.T) {
	r := testrepo.New(t, "noagent")
	r.Write("workmux.json", `{"agent": null}`)
	cfg, err := config.Load(r.Root)
	if err != nil {
		t.Fatal(err)
	}
	reg := term.NewRegistry()
	defer reg.Shutdown()
	p := presets.Deps{Cfg: cfg}
	s := &Server{Cfg: cfg, Builder: &work.Builder{Cfg: cfg, Agents: &agents.Reader{}},
		Sessions: &Sessions{Reg: reg, Presets: p.Spec, KnownDir: func(string) bool { return true }}}
	h := s.Handler()

	code, got := post(t, h, "/api/session/new", map[string]any{"kind": "agent", "cwd": r.Root})
	if code == 200 {
		t.Fatalf("an agent session was started for a project with no agent: %v", got)
	}
	if msg, _ := got["error"].(string); !strings.Contains(msg, "agent") {
		t.Errorf("error = %q", msg)
	}
	if len(reg.List()) != 0 {
		t.Error("something was started anyway")
	}
}

// resume is a question answered by the server, so the answer is the same from the
// dock, a keystroke or curl: attach when there's an agent, a fresh session when not.
func TestResumeFallsBackToAFreshSession(t *testing.T) {
	s, h, root, reg := withSessions(t)
	s.Agents = func(string) (string, bool) { return "", false }

	code, got := post(t, h, "/api/session/new", map[string]any{"kind": "resume", "cwd": root})
	if code != 200 {
		t.Fatalf("resume = %d %v", code, got)
	}
	if got["kind"] != string(term.KindAgent) {
		t.Errorf("kind = %v, want a fresh agent session", got["kind"])
	}
	reg.Shutdown()
}

func TestResumeAttachesToTheAgentThatIsThere(t *testing.T) {
	s, h, root, _ := withSessions(t)
	s.Agents = func(string) (string, bool) { return "abc123de", true }

	code, got := post(t, h, "/api/session/new", map[string]any{"kind": "resume", "cwd": root})
	if code != 200 {
		t.Fatalf("resume = %d %v", code, got)
	}
	if got["kind"] != string(term.KindAttach) || got["agent"] != "abc123de" {
		t.Errorf("got %v, want an attach to abc123de", got)
	}

	// Twice must land on the session that exists, not start a second attach.
	_, again := post(t, h, "/api/session/new", map[string]any{"kind": "resume", "cwd": root})
	if again["id"] != got["id"] {
		t.Errorf("second resume started another session: %v then %v", got["id"], again["id"])
	}
}

func TestAttachRejectsABadAgentID(t *testing.T) {
	_, h, root, _ := withSessions(t)
	for _, id := range []string{"", "no", "../../etc/passwd", "abc; rm -rf /"} {
		code, got := post(t, h, "/api/session/new",
			map[string]any{"kind": "attach", "cwd": root, "agent": id})
		if code == 200 {
			t.Errorf("agent id %q was accepted: %v", id, got)
		}
	}
}

// CORS does not cover WebSockets, so the upgrade needs its own check — otherwise
// any page you had open could connect to localhost and get a shell.
func TestSocketRefusesAForeignOrigin(t *testing.T) {
	_, h, root, _ := withSessions(t)
	_, got := post(t, h, "/api/session/new", map[string]any{"kind": "shell", "cwd": root})
	id := got["id"].(string)

	req := httptest.NewRequest("GET", "/api/session/socket/"+id, nil)
	req.Header.Set("Origin", "http://evil.example")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("code = %d, want 403", rec.Code)
	}
}

func TestSocketUnknownSession(t *testing.T) {
	_, h, _, _ := withSessions(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/session/socket/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("code = %d, want 404", rec.Code)
	}
}

// --no-terminal must remove the routes, not answer "disabled": an absent capability
// shouldn't look like a broken one.
func TestNoTerminalHasNoSessionRoutes(t *testing.T) {
	r := testrepo.New(t, "proj")
	cfg, _ := config.Load(r.Root)
	s := &Server{Cfg: cfg, Builder: &work.Builder{Cfg: cfg, Agents: &agents.Reader{}, Terminal: false}}
	h := s.Handler()

	// /api/* that doesn't exist is a 404, and the work view says terminals are off.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/session/list", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("code = %d, want 404", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/work", nil))
	var view work.View
	_ = json.Unmarshal(rec.Body.Bytes(), &view)
	if view.Terminal {
		t.Error("the work view should report terminals off")
	}
}

func TestSessionMethodGuards(t *testing.T) {
	_, h, _, _ := withSessions(t)
	for _, path := range []string{"/api/session/new", "/api/session/kill"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s GET = %d, want 405", path, rec.Code)
		}
	}
}
