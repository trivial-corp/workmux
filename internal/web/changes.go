package web

import (
	"net/http"

	"github.com/trivial-corp/workmux/internal/changes"
	"github.com/trivial-corp/workmux/internal/gitx"
)

// handleChanges answers "what has this work done": status plus the commits its base
// doesn't have.
func (s *Server) handleChanges(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if !s.knownWorktree(path) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "not a worktree of this repository"})
		return
	}
	base := r.URL.Query().Get("base")
	if base == "" {
		base = gitx.DefaultBranch(s.Cfg.Root)
	}
	writeJSON(w, http.StatusOK, changes.Read(path, base))
}

// handleDiff is one file, or one commit.
func (s *Server) handleDiff(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	path := q.Get("path")
	if !s.knownWorktree(path) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "not a worktree of this repository"})
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

// knownWorktree is the same boundary sessions use: a path from a request is only
// ever one of this repository's own worktrees.
func (s *Server) knownWorktree(path string) bool {
	if path == "" {
		return false
	}
	if s.Sessions != nil && s.Sessions.KnownDir != nil {
		return s.Sessions.KnownDir(path)
	}
	for _, w := range gitx.Worktrees(s.Cfg.Root) {
		if w.Path == path {
			return true
		}
	}
	return false
}
