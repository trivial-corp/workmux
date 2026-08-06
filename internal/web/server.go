// Package web is the HTTP surface: the API the browser and (later) the mobile
// app both read, plus the embedded frontend.
//
// Auth deliberately has one rule with one exception. Terminals hand out a shell,
// so anything not on loopback needs a token — and loopback is exempt, because the
// browser is already on the machine it would be logging into. That keeps local
// use frictionless without pretending a bound-to-the-world port is safe.
package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/trivial-corp/workmux/internal/project"
	"github.com/trivial-corp/workmux/internal/term"
	"github.com/trivial-corp/workmux/internal/work"
)

// ui holds the built frontend. Embedded, so the binary is the whole product:
// nothing to serve from disk, nothing to fetch, works offline.
//
//go:embed all:dist
var ui embed.FS

// build fingerprints the frontend this binary serves. A page keeps running the JS it
// loaded, so after an upgrade or a restart you can be looking at an interface that no
// longer matches the server — and chasing a bug that was fixed an hour ago. The page
// compares this with what it started with and offers to reload.
var build = func() string {
	h := sha256.New()
	_ = fs.WalkDir(ui, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, err := ui.ReadFile(path)
		if err != nil {
			return nil
		}
		h.Write([]byte(path))
		h.Write(b)
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:12]
}()

// Build is the fingerprint of the embedded frontend.
func Build() string { return build }

// Loopback hosts never need a token.
var loopback = map[string]bool{
	"127.0.0.1": true, "::1": true, "localhost": true, "0:0:0:0:0:0:0:1": true,
}

// Server is the dashboard for every repository this process serves.
type Server struct {
	// Projects is what it serves. One is the ordinary case; the routes are the same
	// shape either way, so nothing has to special-case the count.
	Projects *project.Set
	// Terminal says whether this server hands out shells at all.
	Terminal bool
	// Token is required from non-loopback clients. Empty disables the check, which
	// is only sane when something in front is already authenticating.
	Token string
	// Origins that may open a WebSocket. CORS does not cover WebSockets, so
	// without this any page you visited could open one against localhost.
	Origins []string
	// DevDir serves the frontend from disk instead of the embedded copy, so
	// editing the UI is a refresh rather than a rebuild. Empty in a release.
	DevDir string
	// Verbose logs each request. Paired with run.Trace it explains a wrong
	// dashboard: you see the request, then every subprocess it caused.
	Verbose bool
	// Sessions serves terminals. Nil means --no-terminal, and then those routes
	// don't exist at all rather than answering "disabled".
	Sessions *Sessions
}

// agentIDRe bounds what can be passed to an attach command.
var agentIDRe = regexp.MustCompile(`^[0-9a-fA-F-]{6,64}$`)

// resolveResume answers "give me this worktree's agent": attach to the one that's
// there, or start a fresh session rather than making the button a dead end.
func resolveResume(p *project.Project, cwd string) (term.Kind, string) {
	if id, ok := p.Builder.AgentFor(cwd); ok && agentIDRe.MatchString(id) {
		return term.KindAttach, id
	}
	return term.KindAgent, ""
}

// assets is where the frontend is read from: disk in dev, the binary otherwise.
func (s *Server) assets() (fs.FS, error) {
	if s.DevDir != "" {
		if _, err := os.Stat(filepath.Join(s.DevDir, "index.html")); err != nil {
			return nil, fmt.Errorf("--dev: no index.html in %s", s.DevDir)
		}
		return os.DirFS(s.DevDir), nil
	}
	return fs.Sub(ui, "dist")
}

// Handler wires the routes.
//
// Two shapes, and which one a route has says what it is about. /api/… is about the
// server: the whole dashboard, its log, its sessions. /api/p/{project}/… is about
// one repository, and the project is in the path rather than in a body or a header
// so that every such request is legible in a log and repeatable with curl.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/work", s.guard(s.handleOverview))
	mux.HandleFunc("/api/health", s.handleHealth) // unguarded: for a proxy probe
	mux.HandleFunc("/api/log", s.guard(s.handleLog))
	mux.HandleFunc("/api/upload", s.guard(s.handleUpload))
	mux.HandleFunc("/api/projects", s.guard(s.handleProjects))

	mux.HandleFunc("/api/p/{project}/work", s.scoped(handleWork))
	mux.HandleFunc("/api/p/{project}/config", s.scoped(handleConfig))
	mux.HandleFunc("/api/p/{project}/changes", s.scoped(handleChanges))
	mux.HandleFunc("/api/p/{project}/diff", s.scoped(handleDiff))
	mux.HandleFunc("/api/p/{project}/stacks", s.scoped(handleStacks))
	mux.HandleFunc("/api/p/{project}/mcp", s.scoped(handleMCPList))
	mux.HandleFunc("/api/p/{project}/new", s.scoped(handleNewWork))
	mux.HandleFunc("/api/p/{project}/stack", s.scoped(handleStack))
	mux.HandleFunc("/api/p/{project}/update", s.scoped(handleUpdate))
	mux.HandleFunc("/api/p/{project}/pr", s.scoped(handlePR))
	mux.HandleFunc("/api/p/{project}/mcp/add", s.scoped(handleMCPAdd))
	mux.HandleFunc("/api/p/{project}/mcp/remove", s.scoped(handleMCPRemove))
	if s.Sessions != nil {
		// A session belongs to a worktree, so starting one is about a project. Once
		// it exists its id is the server's, and listing, killing and streaming it
		// are not — a client holding an id shouldn't have to remember where it came
		// from.
		mux.HandleFunc("/api/p/{project}/session/new", s.scoped(handleSessionNew))
		mux.HandleFunc("/api/session/list", s.guard(s.handleSessionList))
		mux.HandleFunc("/api/session/kill", s.guard(s.handleSessionKill))
		mux.HandleFunc("/api/session/socket/", s.guard(s.handleSessionSocket))
	}
	mux.HandleFunc("/", s.guard(s.handleUI))
	return mux
}

// handler is a request about one repository.
type handler func(*Server, *project.Project, http.ResponseWriter, *http.Request)

// scoped resolves {project} before the handler runs, so no handler has to wonder
// whether it has one. An unknown id is a 404 naming what does exist: with several
// projects served, a typo in an id is the likeliest thing to go wrong.
func (s *Server) scoped(next handler) http.HandlerFunc {
	return s.guard(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("project")
		p, ok := s.Projects.Get(id)
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]any{
				"error": fmt.Sprintf("no project %q is being served", id),
				"known": s.projectIDs(),
			})
			return
		}
		next(s, p, w, r)
	})
}

func (s *Server) projectIDs() []string {
	out := []string{}
	for _, p := range s.Projects.List() {
		out = append(out, p.ID)
	}
	return out
}

// guard applies the token rule and hands a token in the query straight into a
// cookie, so a URL pasted on a phone doesn't leave the token in every referrer.
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Verbose {
			started := time.Now()
			defer func() { log.Printf("%s %s  %s", r.Method, r.URL.Path, time.Since(started).Round(time.Millisecond)) }()
		}
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

// handleOverview is the dashboard: every project, and all of their work in one
// ordered list.
func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, work.Merge(s.Projects.Builders(), s.Terminal, build))
}

// handleWork is one project's own view, for a client that wants a single
// repository rather than the whole server.
func handleWork(s *Server, p *project.Project, w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, p.Builder.Build())
}

// handleConfig is what the project looks like, for a client that wants to render
// the right affordances before it has any work to show.
func handleConfig(s *Server, p *project.Project, w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, p.Cfg)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	names := []string{}
	for _, p := range s.Projects.List() {
		names = append(names, p.Name())
	}
	// "server" is an identity check, not decoration: a second workmux probes this
	// port to decide whether to hand its repository over, and something else
	// answering 200 must not be mistaken for one.
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "server": "workmux", "projects": s.projectIDs(), "names": names,
	})
}

func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	dist, err := s.assets()
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
	if path != "index.html" && s.DevDir == "" {
		// Hashed asset names make these immutable; index.html must never be, and
		// in dev nothing is — you'd be debugging a file the browser kept.
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
