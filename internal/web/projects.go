package web

import (
	"net/http"
	"path/filepath"

	"github.com/trivial-corp/workmux/internal/gitx"
)

// The set of repositories a server holds is not fixed at startup.
//
// The way you use this tool is: you're in a repo, you run workmux. Doing that in a
// second repo should not give you a second server on a second port with a second
// page — it should put that repo on the page you already have. So a running server
// accepts new projects, and `workmux` in another checkout is a client that hands
// itself over and exits.

// projectRow is one project as the list endpoint reports it.
type projectRow struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Root string `json:"root"`
}

func (s *Server) rows() []projectRow {
	out := []projectRow{}
	for _, p := range s.Projects.List() {
		out = append(out, projectRow{ID: p.ID, Name: p.Name(), Root: p.Root()})
	}
	return out
}

// handleProjects lists what is being served, adds a repository, or stops serving
// one. GET is the list; POST {root} adds; POST {id, remove:true} removes.
func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]any{"projects": s.rows()})
		return
	}
	var req struct {
		Root   string `json:"root"`
		ID     string `json:"id"`
		Remove bool   `json:"remove"`
	}
	if !s.postJSON(w, r, &req) {
		return
	}
	if req.Remove {
		s.removeProject(w, req.ID)
		return
	}
	s.addProject(w, req.Root)
}

func (s *Server) addProject(w http.ResponseWriter, root string) {
	if root == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "which directory?"})
		return
	}
	// Resolved here, the same way the command line resolves it, so the same repo
	// reached by two different paths is one project and not two. Symlinks first
	// because git prints resolved paths and worktree ownership compares them.
	abs, err := filepath.Abs(root)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	abs = gitx.PrimaryRoot(abs)
	if !gitx.IsRepo(abs) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": abs + " is not a git repository",
		})
		return
	}
	known := s.Projects.Len()
	p, err := s.Projects.Add(abs)
	if err != nil {
		Journal.Note("could not serve %s: %s", abs, err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	added := s.Projects.Len() > known
	if added {
		Journal.Note("now serving %s (%s)", p.Name(), p.Root())
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": p.ID, "name": p.Name(), "root": p.Root(), "added": added,
		"projects": s.rows(),
	})
}

// removeProject stops serving a project and closes its sessions.
//
// Closing them is the honest ending: a session is a shell in a worktree of a repo
// this server no longer knows about, so leaving them running would leave processes
// nobody has a route to. The count comes back so the caller can say what it cost.
func (s *Server) removeProject(w http.ResponseWriter, id string) {
	p, err := s.Projects.Remove(id)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	closed := 0
	if s.Sessions != nil {
		for _, info := range s.Sessions.Reg.List() {
			if info.Project != id {
				continue
			}
			if sess, ok := s.Sessions.Reg.Get(info.ID); ok {
				sess.Kill()
				s.Sessions.Reg.Forget(info.ID)
				closed++
			}
		}
	}
	Journal.Note("stopped serving %s · %d session(s) closed", p.Name(), closed)
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "closed": closed, "projects": s.rows(),
	})
}
