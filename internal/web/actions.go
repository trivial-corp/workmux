package web

import (
	"encoding/json"
	"net/http"

	"github.com/trivial-corp/workmux/internal/actions"
	"github.com/trivial-corp/workmux/internal/mcp"
)

// Actions and MCP are optional the same way sessions are: nil means this build or
// this project doesn't do them, and the routes don't exist rather than answering
// "disabled".
type Actions struct {
	Runner *actions.Runner
}

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
func (s *Server) handleNewWork(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name   string `json:"name"`
		Prompt string `json:"prompt"`
		Base   string `json:"base"`
	}
	if !s.postJSON(w, r, &req) {
		return
	}
	out, err := s.Actions.Runner.NewWork(req.Name, req.Prompt, req.Base)
	if err != nil {
		Journal.Note("new work refused: %s", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	Journal.Note("new work %s off %s, %d file(s) carried over", out.Branch, out.Base, out.Copied)
	writeJSON(w, http.StatusOK, out)
}

// handleStack runs a configured container action.
func (s *Server) handleStack(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string `json:"action"`
		Slot   string `json:"slot"`
		Path   string `json:"path"`
	}
	if !s.postJSON(w, r, &req) {
		return
	}
	if req.Path != "" && !s.knownWorktree(req.Path) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "not a worktree of this repository"})
		return
	}
	Journal.Note("stack %s %s", req.Action, req.Slot)
	out, err := s.Actions.Runner.Stack(req.Action, req.Slot, req.Path)
	if err != nil {
		Journal.Note("stack %s failed: %s", req.Action, err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "1", "out": tail(out, 2000)})
}

// handleUpdate merges a worktree's base branch in.
func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
		Base string `json:"base"`
	}
	if !s.postJSON(w, r, &req) {
		return
	}
	if !s.knownWorktree(req.Path) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "not a worktree of this repository"})
		return
	}
	out, err := s.Actions.Runner.MergeBase(req.Path, req.Base)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"out": tail(out, 800)})
}

// handlePR checks a pull request out into its own worktree.
func (s *Server) handlePR(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ref string `json:"ref"`
	}
	if !s.postJSON(w, r, &req) {
		return
	}
	out, err := s.Actions.Runner.CheckoutPR(req.Ref)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- mcp ----

func (s *Server) handleMCPList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": s.MCP.Enabled(),
		"servers": s.MCP.List(r.URL.Query().Get("refresh") == "1"),
	})
}

func (s *Server) handleMCPAdd(w http.ResponseWriter, r *http.Request) {
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
	if err := s.MCP.Add(req.Name, req.Target, req.Transport, req.Scope, req.Env, req.Headers); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleMCPRemove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name  string `json:"name"`
		Scope string `json:"scope"`
	}
	if !s.postJSON(w, r, &req) {
		return
	}
	if err := s.MCP.Remove(req.Name, req.Scope); err != nil {
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
