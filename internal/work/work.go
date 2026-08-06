// Package work assembles the one thing this tool is about.
//
// A piece of work is a worktree + branch, every PR it has produced, the agents
// living in it, the sessions open on it, and — only if this change needs the app
// running — a stack. Containers are an attachment to a task, not the frame around
// it: most edits never need them, and starting one costs a minute you don't spend
// unless the change earns it.
//
// The list is ordered by what wants you: needs-input first, then working, then
// whatever has containers up, then the rest, newest first inside each group. With
// several changes in flight, that ordering *is* the dashboard.
package work

import (
	"sort"
	"sync"
	"time"

	"github.com/trivial-corp/workmux/internal/agents"
	"github.com/trivial-corp/workmux/internal/config"
	"github.com/trivial-corp/workmux/internal/gitx"
	"github.com/trivial-corp/workmux/internal/prs"
	"github.com/trivial-corp/workmux/internal/stack"
)

// Session is a live terminal session, as the work list needs it. The terminal
// package owns the real thing; this is the shape the dashboard reads.
type Session struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Title string `json:"title"`
	CWD   string `json:"cwd"`
	Agent string `json:"agent"`
	Alive bool   `json:"alive"`
}

// PRRef is the PR attached to a worktree's branch.
type PRRef struct {
	Number int    `json:"number"`
	State  string `json:"state"`
	Base   string `json:"base"`
	Title  string `json:"title"`
	Draft  bool   `json:"draft"`
}

// StackRef is a running stack, flattened for the UI.
type StackRef struct {
	Slot     string   `json:"slot"`
	Dir      string   `json:"dir"`
	Path     string   `json:"path"`
	Branch   string   `json:"branch"`
	URL      string   `json:"url"`
	Profiles []string `json:"profiles"`
	stack.State
}

// Item is one piece of work. Project says which repository it belongs to, because
// one server serves several and a merged list is otherwise ambiguous — two repos
// can easily have a branch of the same name.
type Item struct {
	Project   string         `json:"project"`
	Path      string         `json:"path"`
	Dir       string         `json:"dir"`
	Branch    string         `json:"branch"`
	IsDefault bool           `json:"is_default"`
	PR        *PRRef         `json:"pr"`
	PRs       []int          `json:"prs"`
	Base      string         `json:"base"`
	Activity  int64          `json:"activity"`
	Live      bool           `json:"live"`
	Behind    int            `json:"behind"`
	Ahead     int            `json:"ahead"`
	Stack     *StackRef      `json:"stack"`
	Agents    []agents.Agent `json:"agents"`
	Sessions  []Session      `json:"sessions"`
	Tempo     string         `json:"tempo"`
	Rank      int            `json:"rank"`
}

// AgentCaps tells the UI what this project's agent can do, so it can offer what
// exists rather than buttons that answer "not configured".
type AgentCaps struct {
	Name   string `json:"name"`
	Run    bool   `json:"run"`
	Spawn  bool   `json:"spawn"`
	Attach bool   `json:"attach"`
	Jobs   bool   `json:"jobs"`
	MCP    bool   `json:"mcp"`
}

// Project is one repository, without its work: everything a client needs to render
// the right affordances for it. Held apart from the items so a merged list can
// carry many projects' work and still say what each one can do.
type Project struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Root         string    `json:"root"`
	Base         string    `json:"base"`
	StackEnabled bool      `json:"stack_enabled"`
	RepoURL      string    `json:"repo_url"`
	OpenPRs      []prs.PR  `json:"open_prs"`
	NextSlot     string    `json:"next_slot"`
	Profiles     string    `json:"profiles"`
	Agent        AgentCaps `json:"agent"`
}

// View is one project and its work.
type View struct {
	Project
	Work []Item `json:"work"`
}

// Overview is every project this server holds, with all of their work in one list.
//
// One list rather than one per project on purpose: the ordering is what the
// dashboard is for, and "what wants me right now" doesn't stop at a repository
// boundary. Each item names its project, so a client can group or filter.
type Overview struct {
	Projects []Project `json:"projects"`
	Work     []Item    `json:"work"`
	Terminal bool      `json:"terminal"`
	// Build fingerprints the frontend this server serves, so a page can notice it is
	// older than the server and stop you debugging a bug that's already fixed.
	Build string `json:"build"`
}

// Builder assembles one project's view. It holds the readers so their caches
// survive polls.
type Builder struct {
	// ID is the project this builder is for; every item it produces carries it.
	ID     string
	Cfg    *config.Config
	Agents *agents.Reader
	// Sessions returns the live sessions of the whole server. Items keep only the
	// ones opened in their own worktree, and worktree paths are unique across
	// projects, so an unfiltered list is the right input.
	Sessions func() []Session
}

// Build reads the current state of everything. Every reader degrades to empty
// rather than failing, so a missing gh or a stopped docker costs you that column
// and nothing else.
func (b *Builder) Build() View {
	cfg := b.Cfg
	root := cfg.Root
	base := gitx.DefaultBranch(root)
	gitx.KickFetch(root, base) // behind-counts come from local refs; keep them fresh

	trees := gitx.Worktrees(root)
	paths := make([]string, 0, len(trees))
	for _, w := range trees {
		paths = append(paths, w.Path)
	}

	byBranch, openPRs := prs.Data(root)
	known := map[int]bool{}
	for _, p := range byBranch {
		known[p.Number] = true
	}

	// Reading a stack is a docker round trip each; with several up they have no
	// reason to wait for one another.
	running := stack.Running(cfg)
	stackByPath := map[string]*StackRef{}
	if len(running) > 0 {
		refs := make([]*StackRef, len(running))
		var wg sync.WaitGroup
		for i, p := range running {
			wg.Add(1)
			go func(i int, p stack.Project) {
				defer wg.Done()
				refs[i] = &StackRef{
					Slot: p.Slot, Dir: baseName(p.Dir), Path: p.Dir,
					URL: cfg.StackURL(p.Slot), Profiles: splitProfiles(cfg.Profiles()),
					State: stack.Read(p, cfg),
				}
			}(i, p)
		}
		wg.Wait()
		for i, ref := range refs {
			stackByPath[running[i].Dir] = ref
		}
	}

	all := b.Agents.Snapshot(paths)
	byHome := map[string][]agents.Agent{}
	for _, a := range all {
		byHome[a.Home] = append(byHome[a.Home], a)
	}
	liveOwners := b.Agents.LiveDirs(paths)

	var sessions []Session
	if b.Sessions != nil {
		sessions = b.Sessions()
	}

	// Each worktree's behind/ahead is a `git rev-list` — 43 of them here, and
	// sequentially that *is* the response time. They're independent reads, so run
	// them at once, bounded so a repo with a hundred worktrees doesn't fork a
	// hundred gits.
	drift := driftAll(trees, byBranch, base)

	items := make([]Item, 0, len(trees))
	for _, w := range trees {
		branch := w.Branch
		if branch == "" {
			branch = "(detached)"
		}
		mine := byHome[w.Path]
		tempo := ""
		if len(mine) > 0 {
			tempo = mine[0].Tempo // Snapshot already ordered by what wants attention
		}
		// A live process owned by this worktree means work is happening here,
		// whatever the last state write claimed.
		live := liveOwners[w.Path]

		var pr *PRRef
		prNums := []int{}
		if p, ok := byBranch[branch]; ok {
			pr = &PRRef{Number: p.Number, State: p.State, Base: p.Base, Title: p.Title, Draft: p.Draft}
			prNums = append(prNums, p.Number)
		}
		for _, a := range mine {
			for _, n := range a.PRs {
				// Agent notes mention other repos' PRs too. Only keep numbers that
				// are PRs *here*, since that's where the links point.
				if !contains(prNums, n) && (len(known) == 0 || known[n]) {
					prNums = append(prNums, n)
				}
			}
		}

		// Behind *what*: a PR's own base branch, not an assumption about main. A
		// branch cut from another branch was measured against the wrong thing,
		// which made the merge button lie.
		itemBase := base
		if pr != nil && pr.Base != "" {
			itemBase = pr.Base
		}
		behind, ahead := drift[w.Path].behind, drift[w.Path].ahead

		st := stackByPath[w.Path]
		if st != nil {
			st.Branch = branch
		}

		var mySessions []Session
		for _, s := range sessions {
			if s.CWD == w.Path && s.Alive {
				mySessions = append(mySessions, s)
			}
		}
		if mySessions == nil {
			mySessions = []Session{}
		}
		if mine == nil {
			mine = []agents.Agent{}
		}

		items = append(items, Item{
			Project: b.ID,
			Path:    w.Path, Dir: w.Dir, Branch: branch,
			IsDefault: w.Path == root,
			PR:        pr, PRs: prNums, Base: itemBase,
			Activity: lastActivity(w.Path, mine), Live: live,
			Behind: behind, Ahead: ahead, Stack: st,
			Agents: mine, Sessions: mySessions, Tempo: tempo,
			Rank: rank(w.Path == root, tempo, live, st != nil, len(mine) > 0),
		})
	}

	// Newest first inside each group: with 35 worktrees, "what did I touch" is how
	// you find things again.
	sort.SliceStable(items, func(i, j int) bool { return items[i].Activity > items[j].Activity })
	sort.SliceStable(items, func(i, j int) bool { return items[i].Rank < items[j].Rank })

	// An open PR whose branch is already checked out somewhere isn't news.
	var unchecked []prs.PR
	for _, p := range openPRs {
		if !hasBranch(items, p.Branch) {
			unchecked = append(unchecked, p)
		}
		if len(unchecked) >= 12 {
			break
		}
	}

	a := cfg.Agent
	return View{
		Project: Project{
			ID: b.ID, Name: cfg.Name, Root: root, Base: base,
			StackEnabled: cfg.HasStack(), RepoURL: gitx.WebURL(root),
			OpenPRs: unchecked, NextSlot: stack.NextFreeSlot(cfg, running),
			Profiles: cfg.Profiles(),
			Agent: AgentCaps{
				Name: nameOr(a.Name, "agent"), Run: a.Command != "", Spawn: a.Spawn != "",
				Attach: a.Attach != "", Jobs: cfg.JobsDir() != "", MCP: a.MCP != "",
			},
		},
		Work: items,
	}
}

// Merge reads every project at once and puts their work in a single ordered list.
//
// Concurrently, because each project is a fistful of git and docker round trips and
// they have no reason to wait for one another: three repos should cost what the
// slowest one costs, not the sum. The ordering is then applied across the merged
// list, so a blocked agent in one repo outranks a quiet worktree in another —
// which is the whole reason for serving them together.
func Merge(builders []*Builder, terminal bool, build string) Overview {
	out := Overview{Projects: []Project{}, Work: []Item{}, Terminal: terminal, Build: build}
	views := make([]View, len(builders))
	var wg sync.WaitGroup
	for i, b := range builders {
		wg.Add(1)
		go func(i int, b *Builder) {
			defer wg.Done()
			views[i] = b.Build()
		}(i, b)
	}
	wg.Wait()
	for _, v := range views {
		out.Projects = append(out.Projects, v.Project)
		out.Work = append(out.Work, v.Work...)
	}
	sort.SliceStable(out.Work, func(i, j int) bool { return out.Work[i].Activity > out.Work[j].Activity })
	sort.SliceStable(out.Work, func(i, j int) bool { return out.Work[i].Rank < out.Work[j].Rank })
	return out
}

type drift struct{ behind, ahead int }

// driftAll counts behind/ahead for every worktree concurrently, each against its
// own base — a PR's baseRefName when it has one, not an assumption about main.
func driftAll(trees []gitx.Worktree, byBranch map[string]prs.PR, base string) map[string]drift {
	out := make(map[string]drift, len(trees))
	var mu sync.Mutex
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for _, w := range trees {
		itemBase := base
		if p, ok := byBranch[w.Branch]; ok && p.Base != "" {
			itemBase = p.Base
		}
		wg.Add(1)
		go func(path, b string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			behind, ahead := gitx.BehindAhead(path, b)
			mu.Lock()
			out[path] = drift{behind, ahead}
			mu.Unlock()
		}(w.Path, itemBase)
	}
	wg.Wait()
	return out
}

// rank is the ordering the whole dashboard leans on.
//
// The base checkout goes last however busy it looks: it's where you happen to
// stand, not a change in flight, and ranking it by activity kept it pinned to the
// top pretending to be work.
func rank(isDefault bool, tempo string, live, hasStack, hasAgents bool) int {
	switch {
	case isDefault:
		return 6
	case tempo == "blocked":
		return 0
	case live || tempo == "active":
		return 1
	case hasStack:
		return 2
	case hasAgents:
		return 3
	default:
		return 5
	}
}

// lastActivity is when this work was last touched: the newest of its agents'
// state writes and the worktree's own mtime. A worktree nobody has touched in a
// week should sink, and neither signal alone is enough — an agent can be idle
// while you edit by hand, and vice versa.
func lastActivity(path string, mine []agents.Agent) int64 {
	var newest int64
	for _, a := range mine {
		if t, err := time.Parse(time.RFC3339, a.Updated); err == nil && t.Unix() > newest {
			newest = t.Unix()
		}
	}
	if st, err := statMtime(path); err == nil && st > newest {
		newest = st
	}
	return newest
}

func hasBranch(items []Item, branch string) bool {
	for _, i := range items {
		if i.Branch == branch {
			return true
		}
	}
	return false
}

func contains(list []int, n int) bool {
	for _, v := range list {
		if v == n {
			return true
		}
	}
	return false
}

func nameOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func splitProfiles(s string) []string {
	if s == "" {
		return []string{}
	}
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			if p := s[start:i]; p != "" {
				out = append(out, p)
			}
			start = i + 1
		}
	}
	return out
}

func baseName(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}

// IsWorktree reports whether a path is one of this repository's worktrees.
//
// This is the boundary that keeps a session from being a shell anywhere on the
// machine: the caller decides where one may be opened, never the request.
func (b *Builder) IsWorktree(path string) bool {
	if path == "" {
		return false
	}
	for _, w := range gitx.Worktrees(b.Cfg.Root) {
		if w.Path == path {
			return true
		}
	}
	return false
}

// SlotFor names the stack running in a worktree, "" when none is — which is what
// decides whether a logs session is offered.
func (b *Builder) SlotFor(cwd string) string {
	for _, p := range stack.Running(b.Cfg) {
		if p.Dir == cwd {
			return p.Slot
		}
	}
	return ""
}

// AgentFor is this worktree's agent, for resume: the one that most wants
// attention, which is the same order the list uses.
func (b *Builder) AgentFor(cwd string) (string, bool) {
	var paths []string
	for _, w := range gitx.Worktrees(b.Cfg.Root) {
		paths = append(paths, w.Path)
	}
	for _, a := range b.Agents.Snapshot(paths) {
		if a.Home == cwd {
			return a.ID, true
		}
	}
	return "", false
}
