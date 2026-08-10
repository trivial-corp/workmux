// Package project is one repository as the server holds it — its config and every
// reader and runner that works against it — and the set of them a single server
// serves.
//
// It exists because "which repository is this request about" became a question the
// moment one process served more than one. Everything that used to be a field on
// the server is a field here instead, so a handler either has a project or it is
// about the server itself, and there is no third case where it quietly means the
// only one there was.
package project

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/trivial-corp/workmux/internal/actions"
	"github.com/trivial-corp/workmux/internal/agents"
	"github.com/trivial-corp/workmux/internal/config"
	"github.com/trivial-corp/workmux/internal/gitx"
	"github.com/trivial-corp/workmux/internal/mcp"
	"github.com/trivial-corp/workmux/internal/presets"
	"github.com/trivial-corp/workmux/internal/prs"
	"github.com/trivial-corp/workmux/internal/run"
	"github.com/trivial-corp/workmux/internal/term"
	"github.com/trivial-corp/workmux/internal/work"
)

// Project is one repository and everything that reads or changes it.
type Project struct {
	// ID names this project in a URL. Derived from the name, unique within a set,
	// and stable for as long as the server runs — clients key on it.
	ID      string
	Cfg     *config.Config
	Agents  *agents.Reader
	Builder *work.Builder
	Runner  *actions.Runner
	MCP     *mcp.Reader

	// Log records what happened, when something is worth seeing later. The web
	// layer owns the journal; this keeps the dependency pointing that way and not
	// back. Nil is fine and means nothing is recorded.
	Log func(format string, args ...any)
}

// Open reads a repository and wires everything that works against it.
//
// The root is taken as given: resolving it — through symlinks, to the primary
// checkout — is the caller's job, because whether "." means this directory or the
// checkout it belongs to is a question about the command line, not the repo.
func Open(root string) (*Project, error) {
	if !gitx.IsRepo(root) {
		return nil, fmt.Errorf("%s is not a git repository", root)
	}
	cfg, err := config.Load(root)
	if err != nil {
		return nil, fmt.Errorf("%s: workmux.json is not valid json: %w", root, err)
	}
	p := &Project{Cfg: cfg}
	p.Agents = &agents.Reader{JobsDir: cfg.JobsDir(), Process: cfg.Agent.Process}
	p.Builder = &work.Builder{Cfg: cfg, Agents: p.Agents}
	p.MCP = &mcp.Reader{Cfg: cfg}
	p.Runner = &actions.Runner{Cfg: cfg, Invalidate: func() {
		p.Agents.Invalidate()
		prs.Invalidate(cfg.Root)
	}}
	p.Runner.Spawn = p.spawn
	return p, nil
}

// Name is what to call this project in a sentence.
func (p *Project) Name() string { return p.Cfg.Name }

// Root is the repository's primary checkout.
func (p *Project) Root() string { return p.Cfg.Root }

// Spec turns "a session of this kind, here" into something runnable, and stamps it
// with the project so a session always knows which repository it belongs to.
func (p *Project) Spec(kind term.Kind, cwd, agentID string) (term.Spec, error) {
	d := presets.Deps{Cfg: p.Cfg, SlotFor: p.Builder.SlotFor, NextSlot: p.Builder.NextSlot}
	spec, err := d.Spec(kind, cwd, agentID)
	spec.Project = p.ID
	return spec, err
}

// Where names a directory for a human: "trip1/fix-search", and just "trip1" for
// the base checkout, because "trip1/trip1" reads like a bug.
func (p *Project) Where(dir string) string {
	base := filepath.Base(dir)
	if dir == p.Cfg.Root || base == p.Cfg.Name {
		return p.Cfg.Name
	}
	return p.Cfg.Name + "/" + base
}

// Owns reports whether a path is one of this project's worktrees. This is the
// boundary that keeps a session, a diff or a stack action from reaching a directory
// the request picked: the caller decides where, never the request.
func (p *Project) Owns(path string) bool { return p.Builder.IsWorktree(path) }

// spawn starts an agent on a task, session-less, so it survives the request that
// asked for it.
func (p *Project) spawn(cwd, prompt string) (string, error) {
	cmd := p.Cfg.SpawnCmd(prompt)
	if cmd == "" {
		return "", nil
	}
	res := run.Env(cwd, append(os.Environ(), "PATH="+mcp.UserPath()),
		3*time.Minute, os.Getenv("SHELL"), "-lc", cmd)
	if !res.OK() {
		p.note("agent spawn failed in %s: %s", p.Where(cwd), res.LastLine("no output"))
		return "", fmt.Errorf("%s", res.LastLine("agent spawn failed"))
	}
	p.note("agent started in %s", p.Where(cwd))
	return res.Out, nil
}

func (p *Project) note(format string, args ...any) {
	if p.Log != nil {
		p.Log(format, args...)
	}
}

// Set is the projects one server serves, in the order they arrived.
//
// It changes while the server runs. Starting workmux in a second repository does
// not start a second server — it hands that repository to the one already
// listening, which is the only way "all my work in one page" can also be "just run
// it wherever I am". So every read here takes a lock, and List hands back a copy:
// a poll must never see the set half-changed.
type Set struct {
	mu   sync.RWMutex
	list []*Project
	byID map[string]*Project

	// Log is where a project's own notes go. Read at call time rather than copied
	// into each project, so it can be set after the first projects are open —
	// which it always is, since the journal belongs to the server that hasn't been
	// built yet.
	Log func(format string, args ...any)
	// OnAdd finishes wiring a project the rest of the process cares about: the
	// session list, mostly. It exists because a project can arrive over HTTP hours
	// after startup, and it must come out identical to one that was there from the
	// beginning.
	OnAdd func(*Project)
	// OnChange is told the whole set whenever it gains or loses a project, so it
	// can be written down. Called with the roots rather than the projects: what
	// survives a restart is a list of directories, not live readers.
	OnChange func(roots []string)
}

// New opens every root. One bad root fails the whole set: a dashboard silently
// missing a repository you asked for is worse than a server that won't start and
// says which one.
func New(roots []string) (*Set, error) {
	if len(roots) == 0 {
		return nil, errors.New("no repository to serve")
	}
	s := &Set{byID: map[string]*Project{}}
	for _, root := range roots {
		if _, err := s.Add(root); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Add opens a repository and starts serving it. A root already being served is not
// an error and does not open a second copy — running workmux twice in the same
// place should land you on the page you already have.
func (s *Set) Add(root string) (*Project, error) {
	s.mu.Lock()
	for _, p := range s.list {
		if p.Cfg.Root == root {
			s.mu.Unlock()
			return p, nil
		}
	}
	s.mu.Unlock()

	// Open reads the repository, which is subprocesses — done outside the lock so a
	// slow git can't stall the dashboard everyone else is polling.
	p, err := Open(root)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	// Look again: two invocations can race, and the loser must return the winner's
	// project rather than adding a duplicate under a "-2" id.
	for _, existing := range s.list {
		if existing.Cfg.Root == root {
			s.mu.Unlock()
			return existing, nil
		}
	}
	p.Log = s.note
	p.ID = s.freeID(slug(p.Cfg.Name))
	p.Builder.ID = p.ID // every item it produces has to name the project it came from
	s.list = append(s.list, p)
	s.byID[p.ID] = p
	onAdd := s.OnAdd
	s.mu.Unlock()

	// Outside the lock: a hook that reads the set would otherwise deadlock, and
	// there is no reason for one to be forbidden from doing so.
	if onAdd != nil {
		onAdd(p)
	}
	s.changed()
	return p, nil
}

// Roots is every repository being served, in order — the form that outlives the
// process.
func (s *Set) Roots() []string {
	out := []string{}
	for _, p := range s.List() {
		out = append(out, p.Cfg.Root)
	}
	return out
}

func (s *Set) changed() {
	s.mu.RLock()
	fn := s.OnChange
	s.mu.RUnlock()
	if fn != nil {
		fn(s.Roots())
	}
}

func (s *Set) note(format string, args ...any) {
	s.mu.RLock()
	fn := s.Log
	s.mu.RUnlock()
	if fn != nil {
		fn(format, args...)
	}
}

// Remove stops serving a project. The last one can't go: a server with nothing in
// it is a page that can only tell you it is empty.
func (s *Set) Remove(id string) (*Project, error) {
	s.mu.Lock()
	p, ok := s.byID[id]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("no project %q is being served", id)
	}
	if len(s.list) == 1 {
		s.mu.Unlock()
		return nil, errors.New("this is the only project — stop the server instead")
	}
	delete(s.byID, id)
	out := s.list[:0]
	for _, x := range s.list {
		if x != p {
			out = append(out, x)
		}
	}
	s.list = out
	s.mu.Unlock()

	// Outside the lock, like OnAdd: changed() reads the set back to report it.
	s.changed()
	return p, nil
}

// freeID keeps ids unique. Two checkouts of the same repository — the common way to
// end up with a collision — differ by a suffix rather than one shadowing the other.
// Callers hold the lock.
func (s *Set) freeID(base string) string {
	if _, taken := s.byID[base]; !taken {
		return base
	}
	for n := 2; ; n++ {
		id := base + "-" + strconv.Itoa(n)
		if _, taken := s.byID[id]; !taken {
			return id
		}
	}
}

// List is every project, in the order they arrived.
func (s *Set) List() []*Project {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Project, len(s.list))
	copy(out, s.list)
	return out
}

// Get finds a project by id.
func (s *Set) Get(id string) (*Project, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.byID[id]
	return p, ok
}

// First is the project a client gets when it hasn't said which — the one that
// arrived first, which for a single-repo server is the only one.
func (s *Set) First() *Project {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.list) == 0 {
		return nil
	}
	return s.list[0]
}

// Len is how many repositories this server holds.
func (s *Set) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.list)
}

// Builders is every project's builder, for a merged read of the whole server.
func (s *Set) Builders() []*work.Builder {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*work.Builder, 0, len(s.list))
	for _, p := range s.list {
		out = append(out, p.Builder)
	}
	return out
}

// HasStack reports whether any project has containers, which is what decides
// whether docker is required at all.
func (s *Set) HasStack() bool {
	for _, p := range s.List() {
		if p.Cfg.HasStack() {
			return true
		}
	}
	return false
}

// slug makes a project name safe to put in a URL path, without making it
// unrecognisable: an id you can read is an id you can curl.
func slug(name string) string {
	var b strings.Builder
	lastDash := true
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-._")
	if out == "" {
		return "project"
	}
	if len(out) > 40 {
		out = strings.Trim(out[:40], "-._")
	}
	return out
}
