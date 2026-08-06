package web

import (
	"net/http"
	"path/filepath"
	"strings"

	"github.com/trivial-corp/workmux/internal/project"
)

// handleDropCheck says what dropping a piece of work would take with it.
//
// The confirmation is built from this rather than written into the page, so what you
// are warned about and what the drop refuses on are the same read.
func handleDropCheck(s *Server, p *project.Project, w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	rep, err := p.Runner.DropCheck(path)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	out := map[string]any{
		"path": rep.Path, "branch": rep.Branch, "dirty": rep.Dirty,
		"ahead": rep.Ahead, "merged": rep.Merged,
		"sessions": len(s.sessionsIn(path)),
	}
	// A running stack belongs to the work, not to the directory: removing the worktree
	// would leave its containers up with nothing to point at them.
	for _, item := range p.Builder.Build().Work {
		if item.Stack != nil && samePath(item.Path, path) {
			out["slot"] = item.Stack.Slot
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleDrop removes a worktree, its sessions, and — when asked — its branch.
func handleDrop(s *Server, p *project.Project, w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path   string `json:"path"`
		Branch bool   `json:"branch"`
		Force  bool   `json:"force"`
		Slot   string `json:"slot"`
	}
	if !s.postJSON(w, r, &req) {
		return
	}

	// Check before killing anything: a refused drop must leave the work exactly as it
	// was, including its sessions.
	if _, err := p.Runner.DropCheck(req.Path); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if _, err := p.Runner.Drop(req.Path, req.Branch, req.Force); err != nil {
		Journal.Note("drop %s refused: %s", req.Path, err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	// The directory is gone, so anything still pointed at it is a corpse: a shell whose
	// cwd no longer exists, an agent with nowhere to write.
	killed := 0
	if s.Sessions != nil {
		for _, id := range s.sessionsIn(req.Path) {
			if sess, ok := s.Sessions.Reg.Get(id); ok {
				sess.Kill()
				s.Sessions.Reg.Forget(id)
				killed++
			}
		}
	}
	stopped := false
	if req.Slot != "" {
		if _, err := p.Runner.Stack("stop", req.Slot, ""); err == nil {
			stopped = true
		}
	}
	Journal.Note("dropped %s from %s (branch=%v, %d session(s), stack=%v)",
		filepath.Base(req.Path), p.Name(), req.Branch, killed, stopped)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "sessions": killed, "stack_stopped": stopped,
	})
}

// sessionsIn lists the ids of sessions living in a directory.
func (s *Server) sessionsIn(path string) []string {
	var out []string
	if s.Sessions == nil || path == "" {
		return out
	}
	for _, info := range s.Sessions.Reg.List() {
		if samePath(info.CWD, path) {
			out = append(out, info.ID)
		}
	}
	return out
}

// samePath compares two paths as directories, resolving symlinks — on macOS /var is
// /private/var, and a comparison that misses that thinks nothing lives here.
func samePath(a, b string) bool {
	if a == b {
		return true
	}
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		ra = a
	}
	rb, err := filepath.EvalSymlinks(b)
	if err != nil {
		rb = b
	}
	return strings.TrimSuffix(ra, "/") == strings.TrimSuffix(rb, "/")
}
