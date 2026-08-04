// Package web is the HTTP surface: the API the browser and (later) the mobile
// app both read, plus the embedded frontend.
//
// Auth deliberately has one rule with one exception. Terminals hand out a shell,
// so anything not on loopback needs a token — and loopback is exempt, because the
// browser is already on the machine it would be logging into. That keeps local
// use frictionless without pretending a bound-to-the-world port is safe.
package web

import (
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/trivial-corp/workmux/internal/config"
	"github.com/trivial-corp/workmux/internal/work"
)

// ui holds the built frontend. Embedded, so the binary is the whole product:
// nothing to serve from disk, nothing to fetch, works offline.
//
//go:embed all:dist
var ui embed.FS

// Loopback hosts never need a token.
var loopback = map[string]bool{
	"127.0.0.1": true, "::1": true, "localhost": true, "0:0:0:0:0:0:0:1": true,
}

// Server is one repository's dashboard.
type Server struct {
	Cfg     *config.Config
	Builder *work.Builder
	// Token is required from non-loopback clients. Empty disables the check, which
	// is only sane when something in front is already authenticating.
	Token string
	// Origins that may open a WebSocket. CORS does not cover WebSockets, so
	// without this any page you visited could open one against localhost.
	Origins []string
}

// Handler wires the routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/work", s.guard(s.handleWork))
	mux.HandleFunc("/api/config", s.guard(s.handleConfig))
	mux.HandleFunc("/api/health", s.handleHealth) // unguarded: for a proxy probe
	mux.HandleFunc("/", s.guard(s.handleUI))
	return mux
}

// guard applies the token rule and hands a token in the query straight into a
// cookie, so a URL pasted on a phone doesn't leave the token in every referrer.
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="workmux"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error": "a token is required from off-box clients",
			})
			return
		}
		if tok := r.URL.Query().Get("t"); tok != "" && tok == s.Token {
			http.SetCookie(w, &http.Cookie{
				Name: "workmux", Value: tok, Path: "/",
				HttpOnly: true, SameSite: http.SameSiteLaxMode,
			})
		}
		next(w, r)
	}
}

func (s *Server) authorized(r *http.Request) bool {
	if s.Token == "" {
		return true
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil && loopback[host] {
		return true
	}
	if c, err := r.Cookie("workmux"); err == nil && c.Value == s.Token {
		return true
	}
	if r.Header.Get("X-Workmux-Token") == s.Token {
		return true
	}
	if strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") == s.Token {
		return true
	}
	return r.URL.Query().Get("t") == s.Token
}

// OriginOK decides whether a WebSocket upgrade may proceed.
func (s *Server) OriginOK(origin string) bool {
	if origin == "" {
		return true // a non-browser client (curl, the mobile app) sends none
	}
	for _, ok := range s.Origins {
		if origin == ok {
			return true
		}
	}
	return false
}

func (s *Server) handleWork(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Builder.Build())
}

// handleConfig is what the project looks like, for a client that wants to render
// the right affordances before it has any work to show.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Cfg)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": s.Cfg.Name})
}

func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	dist, err := fs.Sub(ui, "dist")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "no frontend embedded"})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/")
	if path == "" {
		path = "index.html"
	}
	if _, err := fs.Stat(dist, path); err != nil {
		// Unknown /api/* is a mistake worth reporting as one; anything else is a
		// client-side route, so hand back the app.
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		path = "index.html"
	}
	b, err := fs.ReadFile(dist, path)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	w.Header().Set("Content-Type", contentType(path))
	if path != "index.html" {
		// Hashed asset names make these immutable; index.html must never be.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-store")
	}
	_, _ = w.Write(b)
}

func contentType(path string) string {
	switch {
	case strings.HasSuffix(path, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(path, ".js"):
		return "text/javascript; charset=utf-8"
	case strings.HasSuffix(path, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(path, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(path, ".json"):
		return "application/json"
	default:
		return "application/octet-stream"
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write: %v", err)
	}
}

// Listen serves until the process is told to stop. Timeouts are deliberately
// absent on writes: sessions stream for hours.
func (s *Server) Listen(addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 20 * time.Second,
	}
	return srv.ListenAndServe()
}
