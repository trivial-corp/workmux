package web

import (
	"net/http"

	"github.com/trivial-corp/workmux/internal/changes"
	"github.com/trivial-corp/workmux/internal/gitx"
	"github.com/trivial-corp/workmux/internal/project"
)

// handleChanges answers "what has this work done": status plus the commits its base
// doesn't have.
func handleChanges(s *Server, p *project.Project, w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if !p.Owns(path) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "not a worktree of " + p.Name()})
		return
	}
	base := r.URL.Query().Get("base")
	if base == "" {
		base = gitx.DefaultBranch(p.Root())
	}
	writeJSON(w, http.StatusOK, changes.Read(path, base))
}

// handleDiff is one file, or one commit.
func handleDiff(s *Server, p *project.Project, w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	path := q.Get("path")
	if !p.Owns(path) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "not a worktree of " + p.Name()})
		return
	}
	if rev := q.Get("rev"); rev != "" {
		diff := changes.CommitDiff(path, rev)
		if diff == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no such revision"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"diff": diff, "rev": rev})
		return
	}
	file := q.Get("file")
	if file == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "which file?"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"diff": changes.FileDiff(path, file, q.Get("staged") == "1"),
		"file": file,
	})
}
