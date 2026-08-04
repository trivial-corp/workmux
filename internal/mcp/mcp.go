// Package mcp surfaces the agent's MCP registry.
//
// The part worth showing isn't which servers are registered — it's whether an agent
// can actually *reach* them. A server can be registered and invisible: its command
// isn't on PATH, or it needs an OAuth round. Both fail silently unless you go
// looking, so the reason belongs next to the name.
package mcp

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/trivial-corp/workmux/internal/config"
	"github.com/trivial-corp/workmux/internal/run"
)

// Server is one registered MCP server and whether it answers.
type Server struct {
	Name    string `json:"name"`
	Target  string `json:"target"`
	State   string `json:"state"` // ok | auth | pending | fail
	Detail  string `json:"detail"`
	Scope   string `json:"scope"`
	Command string `json:"command"`
	Suggest string `json:"suggest"` // where a missing command actually lives
}

// listLine is "name: target - status", which is what the CLI prints.
var listLine = regexp.MustCompile(`^(.+?): (.*?)\s+-\s+([^\s].*)$`)

// enoent pulls the command out of a "no such file" complaint, so a missing binary can
// be located rather than only reported.
var enoent = regexp.MustCompile(`(?:spawn|ENOENT)[: ]+([\w.\-/]+)`)

// Reader lists servers for one project.
type Reader struct {
	Cfg *config.Config

	mu       sync.Mutex
	cached   []Server
	cachedAt time.Time
}

// Enabled reports whether this project has an MCP registry at all.
func (r *Reader) Enabled() bool { return r.Cfg.Agent.MCP != "" }

// List health-checks every server. Cached, because each HTTP server costs a network
// round and the panel is polled.
func (r *Reader) List(force bool) []Server {
	if !r.Enabled() {
		return []Server{}
	}
	r.mu.Lock()
	if !force && r.cached != nil && time.Since(r.cachedAt) < 20*time.Second {
		defer r.mu.Unlock()
		return r.cached
	}
	r.mu.Unlock()

	// Through a login shell with a real PATH: an agent session runs `$SHELL -lc`, so
	// its PATH is what decides reachability. Checking with this server's own PATH
	// reported a different answer than the thing that actually connects.
	env := append(os.Environ(), "PATH="+UserPath())
	res := run.Env(r.Cfg.Root, env, 120*time.Second, loginShell(), "-lc", r.Cfg.Agent.MCP+" list")

	scopes := r.scopes()
	commands := r.commands()
	out := []Server{}
	for _, ln := range strings.Split(stripANSI(res.Out), "\n") {
		m := listLine.FindStringSubmatch(strings.TrimSpace(ln))
		if m == nil {
			continue
		}
		name, target, status := strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), m[3]
		s := Server{
			Name: name, Target: target, Scope: scopes[name],
			State:   classify(status),
			Detail:  strings.TrimSpace(strings.Trim(status, "✓✔✘✗!⏸ ")),
			Command: commands[name],
		}
		if miss := enoent.FindStringSubmatch(status); miss != nil {
			s.Suggest = FindExecutable(miss[1])
		}
		out = append(out, s)
	}
	rank := map[string]int{"auth": 0, "fail": 1, "pending": 2, "ok": 3}
	sort.Slice(out, func(i, j int) bool {
		if rank[out[i].State] != rank[out[j].State] {
			return rank[out[i].State] < rank[out[j].State]
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	r.mu.Lock()
	r.cached, r.cachedAt = out, time.Now()
	r.mu.Unlock()
	return out
}

// classify reads the words, not the glyph. "connected" ships with ✔ (U+2714) in some
// builds and ✓ (U+2713) in others, and matching the glyph put seventeen healthy
// servers in the failed column.
func classify(status string) string {
	low := strings.ToLower(status)
	switch {
	case strings.Contains(low, "authenticat"):
		return "auth"
	case strings.Contains(low, "pending"):
		return "pending"
	case strings.Contains(low, "connected") && !strings.Contains(low, "fail"):
		return "ok"
	default:
		return "fail"
	}
}

var nameOK = regexp.MustCompile(`^[A-Za-z0-9][\w.-]{0,60}$`)

// Add registers a server through the agent CLI, so the CLI stays the one source of
// truth for what is registered.
func (r *Reader) Add(name, target, transport, scope string, env, headers []string) error {
	if !r.Enabled() {
		return errString("this project has no mcp command configured")
	}
	if !nameOK.MatchString(name) {
		return errString("name must be letters, digits, dots or dashes")
	}
	if strings.TrimSpace(target) == "" {
		return errString("give it a URL or a command")
	}
	switch scope {
	case "user", "project", "local":
	default:
		return errString("bad scope")
	}
	argv := append(strings.Fields(r.Cfg.Agent.MCP), "add", "--scope", scope)
	if transport == "http" || transport == "sse" {
		argv = append(argv, "--transport", transport)
	}
	for _, h := range headers {
		if strings.Contains(h, ":") {
			argv = append(argv, "--header", h)
		}
	}
	for _, e := range env {
		if strings.Contains(e, "=") {
			argv = append(argv, "-e", e)
		}
	}
	argv = append(argv, name)
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		argv = append(argv, strings.TrimSpace(target))
	} else {
		argv = append(argv, "--")
		argv = append(argv, strings.Fields(target)...)
	}
	res := run.Env(r.Cfg.Root, append(os.Environ(), "PATH="+UserPath()), 60*time.Second, argv...)
	if !res.OK() {
		return errString(res.LastLine("mcp add failed"))
	}
	r.List(true)
	return nil
}

// Remove unregisters a server.
func (r *Reader) Remove(name, scope string) error {
	if !r.Enabled() {
		return errString("this project has no mcp command configured")
	}
	if !nameOK.MatchString(name) {
		return errString("bad name")
	}
	argv := append(strings.Fields(r.Cfg.Agent.MCP), "remove", name)
	switch scope {
	case "user", "project", "local":
		argv = append(argv, "--scope", scope)
	}
	res := run.Env(r.Cfg.Root, append(os.Environ(), "PATH="+UserPath()), 60*time.Second, argv...)
	if !res.OK() {
		return errString(res.LastLine("mcp remove failed"))
	}
	r.List(true)
	return nil
}

// agentConfig is the shape of the agent CLI's own config file, read for the two
// things its list output doesn't say: which scope a server came from, and what
// command it was declared with.
type agentConfig struct {
	MCPServers map[string]serverDecl `json:"mcpServers"`
	Projects   map[string]struct {
		MCPServers map[string]serverDecl `json:"mcpServers"`
	} `json:"projects"`
}

type serverDecl struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
	URL     string   `json:"url"`
}

func (r *Reader) readAgentConfig() agentConfig {
	var cfg agentConfig
	home, err := os.UserHomeDir()
	if err != nil {
		return cfg
	}
	body, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		return cfg
	}
	_ = json.Unmarshal(body, &cfg)
	return cfg
}

// scopes says which config each server came from, which is what decides who sees it.
func (r *Reader) scopes() map[string]string {
	cfg := r.readAgentConfig()
	out := map[string]string{}
	for name := range cfg.MCPServers {
		out[name] = "user"
	}
	for dir, p := range cfg.Projects {
		for name := range p.MCPServers {
			// This checkout's own entry wins: it's the one that applies here.
			if _, seen := out[name]; !seen || dir == r.Cfg.Root {
				out[name] = "local:" + filepath.Base(dir)
			}
		}
	}
	return out
}

// commands is the declared command per server, for showing what a failure was trying
// to run.
func (r *Reader) commands() map[string]string {
	cfg := r.readAgentConfig()
	out := map[string]string{}
	add := func(name string, s serverDecl) {
		if s.Command != "" {
			out[name] = strings.TrimSpace(s.Command + " " + strings.Join(s.Args, " "))
		} else if s.URL != "" {
			out[name] = s.URL
		}
	}
	for name, s := range cfg.MCPServers {
		add(name, s)
	}
	for _, p := range cfg.Projects {
		for name, s := range p.MCPServers {
			if _, seen := out[name]; !seen {
				add(name, s)
			}
		}
	}
	return out
}

// binDirs are where user-installed binaries live.
var binDirs = []string{
	"~/go/bin", "~/.local/bin", "~/bin", "/opt/homebrew/bin", "/usr/local/bin",
	"~/.npm-global/bin", "~/.cargo/bin", "~/.bun/bin",
}

// UserPath is PATH as a terminal session would have it.
//
// A server declared as a bare command is only reachable if that command is on the
// PATH the agent process inherits — and a server started by launchd, systemd or nohup
// has almost nothing on it. That's why "I installed it and it still doesn't appear"
// happens, and normalising the PATH once is the fix rather than a per-server button.
func UserPath() string {
	path := os.Getenv("PATH")
	home, _ := os.UserHomeDir()
	seen := map[string]bool{}
	for _, p := range strings.Split(path, ":") {
		seen[p] = true
	}
	for _, dir := range binDirs {
		full := dir
		if strings.HasPrefix(dir, "~") && home != "" {
			full = filepath.Join(home, strings.TrimPrefix(dir, "~/"))
		}
		if seen[full] {
			continue
		}
		if st, err := os.Stat(full); err == nil && st.IsDir() {
			path += ":" + full
			seen[full] = true
		}
	}
	return path
}

// FindExecutable locates a command that isn't on the current PATH, so a failure can
// say where the thing actually is.
func FindExecutable(name string) string {
	if name == "" || strings.ContainsAny(name, "/ ") {
		return ""
	}
	home, _ := os.UserHomeDir()
	for _, dir := range binDirs {
		full := dir
		if strings.HasPrefix(dir, "~") && home != "" {
			full = filepath.Join(home, strings.TrimPrefix(dir, "~/"))
		}
		cand := filepath.Join(full, name)
		if st, err := os.Stat(cand); err == nil && !st.IsDir() && st.Mode().Perm()&0o111 != 0 {
			return cand
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return ""
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)

func stripANSI(s string) string { return ansi.ReplaceAllString(s, "") }

type errString string

func (e errString) Error() string { return string(e) }

func loginShell() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		if _, err := os.Stat(sh); err == nil {
			return sh
		}
	}
	return "/bin/sh"
}
