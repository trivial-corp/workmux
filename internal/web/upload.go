package web

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Pasting a screenshot is a real part of working with an agent — "here's what it
// looks like, fix it" — and a terminal can't carry an image. What an agent CLI does
// accept is a *path*, so a pasted image is written to a file and the path is typed
// in for you. That's the whole feature: upload, then paste text.
const (
	maxUpload = 12 << 20 // a screenshot, not a video
	uploadTTL = 24 * time.Hour
)

// imageTypes maps a sniffed content type to an extension. Sniffed, not trusted: the
// header comes from the page and the extension ends up in a command line.
var imageTypes = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

// handleUpload stores one pasted image and returns where it landed.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "post only"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(http.MaxBytesReader(w, r.Body, maxUpload+1), maxUpload+1))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "could not read the upload"})
		return
	}
	if len(body) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "nothing to upload"})
		return
	}
	if len(body) > maxUpload {
		writeJSON(w, http.StatusRequestEntityTooLarge,
			map[string]string{"error": "that image is too big (12MB max)"})
		return
	}
	// http.DetectContentType reads the magic bytes, so a .png that isn't one, or a
	// header claiming otherwise, doesn't decide what gets written.
	ext, ok := imageTypes[strings.Split(http.DetectContentType(body), ";")[0]]
	if !ok {
		writeJSON(w, http.StatusUnsupportedMediaType,
			map[string]string{"error": "only images can be pasted (png, jpeg, gif, webp)"})
		return
	}

	dir, err := s.uploadDir()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	// The name is ours, never the client's: it goes into a command line.
	name := fmt.Sprintf("paste-%d%s", time.Now().UnixNano(), ext)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": path, "bytes": len(body)})
}

// uploadDir is a per-user directory outside any repository — a pasted screenshot is
// not a change to the project, and dropping files into a worktree would show up in
// its diff.
func (s *Server) uploadDir() (string, error) {
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("workmux-pastes-%d", os.Getuid()))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("could not make a place for pasted images: %w", err)
	}
	s.sweepUploads(dir)
	return dir, nil
}

// sweepUploads deletes yesterday's pastes. Nobody would ever clean this up by hand,
// and a directory of screenshots growing forever is its own small bug.
func (s *Server) sweepUploads(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < uploadTTL {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}
