package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/trivial-corp/workmux/internal/project"
	"github.com/trivial-corp/workmux/internal/term"
	"github.com/trivial-corp/workmux/internal/testrepo"
	"github.com/trivial-corp/workmux/internal/work"
)

func get(t *testing.T, h http.Handler, path string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// The whole point: two repositories, one page, one ordered list.
func TestOverviewMergesEveryProject(t *testing.T) {
	_, h, _ := serving(t, "", "alpha", "beta")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/work", nil))
	var v work.Overview
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Projects) != 2 {
		t.Fatalf("projects = %+v, want two", v.Projects)
	}
	if v.Projects[0].ID != "alpha" || v.Projects[1].ID != "beta" {
		t.Errorf("ids = %q, %q — the order they were given", v.Projects[0].ID, v.Projects[1].ID)
	}
	// Every project's base checkout is in the one list, and each says which project
	// it is: with two repos a branch name no longer identifies anything on its own.
	if len(v.Work) != 2 {
		t.Fatalf("work = %+v, want one item per repo", v.Work)
	}
	seen := map[string]bool{}
	for _, w := range v.Work {
		seen[w.Project] = true
	}
	if !seen["alpha"] || !seen["beta"] {
		t.Errorf("work items name projects %v", seen)
	}
}

// A worktree of one project must not be reachable through another's endpoints.
// This is the boundary the whole session and diff surface rests on, and with one
// project it could not be tested at all.
func TestProjectsCannotReachEachOther(t *testing.T) {
	_, h, roots := serving(t, "", "alpha", "beta")
	betaRoot := roots[1]

	code, got := get(t, h, "/api/p/alpha/changes?path="+betaRoot)
	if code == 200 {
		t.Fatalf("alpha read beta's worktree: %v", got)
	}
	if msg, _ := got["error"].(string); !strings.Contains(msg, "alpha") {
		t.Errorf("error = %q, want it to name the project", msg)
	}

	// And through its own, it works.
	if code, got = get(t, h, "/api/p/beta/changes?path="+betaRoot); code != 200 {
		t.Errorf("beta could not read its own worktree: %d %v", code, got)
	}
}

// A typo in a project id is the likeliest thing to go wrong once there are
// several, so the answer says what does exist.
func TestUnknownProjectIsA404(t *testing.T) {
	_, h, _ := serving(t, "", "alpha", "beta")
	code, got := get(t, h, "/api/p/gamma/work")
	if code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", code)
	}
	known, _ := got["known"].([]any)
	if len(known) != 2 {
		t.Errorf("known = %v, want both projects listed", got["known"])
	}
}

// Running workmux in a second repo hands it to the server already up. That is one
// POST, and it has to produce a project indistinguishable from a startup one.
func TestAddingAProjectWhileRunning(t *testing.T) {
	s, h, _ := serving(t, "", "alpha")
	later := testrepo.New(t, "later")

	code, got := post(t, h, "/api/projects", map[string]any{"root": later.Root})
	if code != 200 {
		t.Fatalf("add = %d %v", code, got)
	}
	if got["id"] != "later" || got["added"] != true {
		t.Errorf("add = %v", got)
	}
	if s.Projects.Len() != 2 {
		t.Fatalf("set holds %d projects", s.Projects.Len())
	}

	// It answers on its own routes immediately…
	if code, got = get(t, h, "/api/p/later/work"); code != 200 {
		t.Fatalf("the new project's route = %d %v", code, got)
	}
	// …and it is in the merged list.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/work", nil))
	var v work.Overview
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	if len(v.Projects) != 2 {
		t.Errorf("overview projects = %+v", v.Projects)
	}

	// The same root again is the same project, not a second one under "later-2":
	// running workmux twice where you already are should land you where you are.
	if code, got = post(t, h, "/api/projects", map[string]any{"root": later.Root}); code != 200 {
		t.Fatalf("re-add = %d %v", code, got)
	}
	if got["added"] != false || got["id"] != "later" {
		t.Errorf("re-add = %v, want the project that is already there", got)
	}
	if s.Projects.Len() != 2 {
		t.Errorf("re-adding created a duplicate: %d projects", s.Projects.Len())
	}
}

func TestAddingSomethingThatIsNotARepo(t *testing.T) {
	_, h, _ := serving(t, "", "alpha")
	code, got := post(t, h, "/api/projects", map[string]any{"root": t.TempDir()})
	if code == 200 {
		t.Fatalf("a bare directory was accepted: %v", got)
	}
	if msg, _ := got["error"].(string); !strings.Contains(msg, "git repository") {
		t.Errorf("error = %q", msg)
	}
}

func TestRemovingAProject(t *testing.T) {
	s, h, _ := serving(t, "", "alpha", "beta")

	code, got := post(t, h, "/api/projects", map[string]any{"id": "beta", "remove": true})
	if code != 200 {
		t.Fatalf("remove = %d %v", code, got)
	}
	if s.Projects.Len() != 1 {
		t.Errorf("set holds %d projects", s.Projects.Len())
	}
	if code, _ = get(t, h, "/api/p/beta/work"); code != http.StatusNotFound {
		t.Errorf("beta still answers: %d", code)
	}

	// The last one can't go: a server with nothing in it is a page that can only
	// tell you it is empty.
	code, got = post(t, h, "/api/projects", map[string]any{"id": "alpha", "remove": true})
	if code == 200 {
		t.Fatalf("the last project was removed: %v", got)
	}
	if s.Projects.Len() != 1 {
		t.Errorf("set holds %d projects", s.Projects.Len())
	}
}

// Health is what a second workmux probes to decide whether to hand its repository
// over, so it has to say what it is — something else answering 200 on that port
// must not be mistaken for a workmux.
func TestHealthIdentifiesItself(t *testing.T) {
	_, h, _ := serving(t, "", "alpha", "beta")
	code, got := get(t, h, "/api/health")
	if code != 200 {
		t.Fatalf("code = %d", code)
	}
	if got["server"] != "workmux" || got["ok"] != true {
		t.Errorf("health = %v", got)
	}
	if ids, _ := got["projects"].([]any); len(ids) != 2 {
		t.Errorf("projects = %v", got["projects"])
	}
}

// Sessions are the server's, not a project's: a client holding an id shouldn't
// have to remember which repository issued it. But the session says.
func TestSessionsCarryTheirProject(t *testing.T) {
	r := testrepo.New(t, "alpha")
	set, err := project.New([]string{r.Root})
	if err != nil {
		t.Fatal(err)
	}
	reg := newRegistry(t)
	s := &Server{Projects: set, Terminal: true, Sessions: &Sessions{Reg: reg}}
	h := s.Handler()

	code, got := post(t, h, "/api/p/alpha/session/new",
		map[string]any{"kind": "shell", "cwd": r.Root})
	if code != 200 {
		t.Fatalf("new = %d %v", code, got)
	}
	if got["project"] != "alpha" {
		t.Errorf("session project = %v, want alpha", got["project"])
	}
}

func newRegistry(t *testing.T) *term.Registry {
	t.Helper()
	reg := term.NewRegistry()
	t.Cleanup(reg.Shutdown)
	return reg
}
