package web

import (
	"encoding/json"
	"net/http"

	"github.com/trivial-corp/workmux/internal/mcp"
	"github.com/trivial-corp/workmux/internal/project"
)

func (s *Server) postJSON(w http.ResponseWriter, r *http.Request, into any) bool {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "post only"})
		return false
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(into); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return false
	}
	return true
}

// handleNewWork is the button that starts everything: a worktree, its files, and an
// agent on the task.
func handleNewWork(s *Server, p *project.Project, w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name   string `json:"name"`
		Prompt string `json:"prompt"`
		Base   string `json:"base"`
	}
	if !s.postJSON(w, r, &req) {
		return
	}
	out, err := p.Runner.NewWork(req.Name, req.Prompt, req.Base)
	if err != nil {
		Journal.Note("new work in %s refused: %s", p.Name(), err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	Journal.Note("new work %s/%s off %s, %d file(s) carried over",
		p.Name(), out.Branch, out.Base, out.Copied)
	writeJSON(w, http.StatusOK, out)
}

// handleNewPreview is the name new work would get, computed by the same code that
// would create it, so the dialog can show it while you type.
func handleNewPreview(s *Server, p *project.Project, w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name   string `json:"name"`
		Prompt string `json:"prompt"`
	}
	if !s.postJSON(w, r, &req) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"branch": p.Runner.PreviewName(req.Name, req.Prompt)})
}

// handleSetName names a piece of work, or unnames it with an empty string.
func handleSetName(s *Server, p *project.Project, w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
		Name string `json:"name"`
	}
	if !s.postJSON(w, r, &req) {
		return
	}
	if !p.Owns(req.Path) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "not a worktree of " + p.Name()})
		return
	}
	if err := p.Runner.SetName(req.Path, req.Name); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleStack runs a configured container action.
func handleStack(s *Server, p *project.Project, w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string `json:"action"`
		Slot   string `json:"slot"`
		Path   string `json:"path"`
	}
	if !s.postJSON(w, r, &req) {
		return
	}
	if req.Path != "" && !p.Owns(req.Path) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "not a worktree of " + p.Name()})
		return
	}
	Journal.Note("stack %s %s in %s", req.Action, req.Slot, p.Name())
	out, err := p.Runner.Stack(req.Action, req.Slot, req.Path)
	if err != nil {
		Journal.Note("stack %s failed: %s", req.Action, err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "1", "out": tail(out, 2000)})
}

// handleUpdate merges a worktree's base branch in.
func handleUpdate(s *Server, p *project.Project, w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
		Base string `json:"base"`
	}
	if !s.postJSON(w, r, &req) {
		return
	}
	if !p.Owns(req.Path) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "not a worktree of " + p.Name()})
		return
	}
	out, err := p.Runner.MergeBase(req.Path, req.Base)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"out": tail(out, 800)})
}

// handlePR checks a pull request out into its own worktree.
func handlePR(s *Server, p *project.Project, w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ref string `json:"ref"`
	}
	if !s.postJSON(w, r, &req) {
		return
	}
	out, err := p.Runner.CheckoutPR(req.Ref)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- mcp ----

func handleMCPList(s *Server, p *project.Project, w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": p.MCP.Enabled(),
		"servers": p.MCP.List(r.URL.Query().Get("refresh") == "1"),
	})
}

func handleMCPAdd(s *Server, p *project.Project, w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name      string   `json:"name"`
		Target    string   `json:"target"`
		Transport string   `json:"transport"`
		Scope     string   `json:"scope"`
		Env       []string `json:"env"`
		Headers   []string `json:"headers"`
	}
	if !s.postJSON(w, r, &req) {
		return
	}
	if err := p.MCP.Add(req.Name, req.Target, req.Transport, req.Scope, req.Env, req.Headers); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleMCPAuth hands back a URL to authorize at, rather than driving the agent's
// own UI. Opening a session and typing into it raced the program's startup and lost
// silently; a URL is a fact the browser can act on, and the browser is often not
// even on this machine.
func handleMCPAuth(s *Server, p *project.Project, w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !s.postJSON(w, r, &req) {
		return
	}
	start, err := p.MCP.Auth(req.Name)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	Journal.Note("mcp auth %s in %s → %s", req.Name, p.Name(), tail(start.URL, 60))
	writeJSON(w, http.StatusOK, start)
}

func handleMCPRemove(s *Server, p *project.Project, w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name  string `json:"name"`
		Scope string `json:"scope"`
	}
	if !s.postJSON(w, r, &req) {
		return
	}
	if err := p.MCP.Remove(req.Name, req.Scope); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleLog is this process's own log — session and action lifecycle, agents spawned,
// refused requests. It used to exist only in the terminal you launched it from, which
// is nowhere you'll look.
func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"lines": Journal.Lines()})
}

func tail(s string, n int) string {
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}

var _ = mcp.UserPath // the PATH normalisation is used when sessions are wired
