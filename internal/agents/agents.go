// Package agents answers two questions the dashboard is built around: which
// agents live in which worktree, and which of them are working right now.
//
// Both are read rather than guessed. An agent CLI keeps per-agent state on disk
// (config's agent.jobs), so ownership comes from that state — and "working" comes
// from a live process, because the state file is only written at turn boundaries
// and therefore reads idle for the agent you are watching work.
package agents

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/trivial-corp/workmux/internal/run"
)

// Agent is one agent, as the UI needs it.
type Agent struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Tempo   string `json:"tempo"`  // blocked | active | idle | ""
	Detail  string `json:"detail"` // its own one-line status
	Updated string `json:"updated"`
	Home    string `json:"home"` // the worktree that owns it
	PRs     []int  `json:"prs"`
	Tokens  int    `json:"tokens"`
}

// state is the on-disk shape. Only the fields that matter are named; an agent CLI
// is free to write more.
type state struct {
	Name         string `json:"name"`
	Tempo        string `json:"tempo"`
	Detail       string `json:"detail"`
	UpdatedAt    string `json:"updatedAt"`
	CWD          string `json:"cwd"`
	WorktreePath string `json:"worktreePath"`
	Tokens       int    `json:"tokens"`
	Children     []struct {
		Kind  string `json:"kind"`
		Href  string `json:"href"`
		Title string `json:"title"`
	} `json:"children"`
}

var (
	prHref = regexp.MustCompile(`/pull/(\d+)`)
	prText = regexp.MustCompile(`\bPR #?(\d+)`)
	// tempoRank puts what needs a human first.
	tempoRank = map[string]int{"blocked": 0, "active": 1, "idle": 2}
)

// Reader loads agent state for one repository.
type Reader struct {
	JobsDir string // "" when this project has no agent
	Process string // process name that means "an agent is working here"

	mu       sync.Mutex
	cached   []Agent
	cachedAt time.Time

	liveMu sync.Mutex
	live   map[string]bool
	liveAt time.Time
}

// Snapshot lists every agent whose home is one of worktrees, newest state first.
// Cached briefly: the dashboard polls, and this reads a directory of JSON files.
func (r *Reader) Snapshot(worktrees []string) []Agent {
	r.mu.Lock()
	if time.Since(r.cachedAt) < 3*time.Second && r.cached != nil {
		defer r.mu.Unlock()
		return r.cached
	}
	r.mu.Unlock()

	out := []Agent{}
	if r.JobsDir != "" {
		entries, err := os.ReadDir(r.JobsDir)
		if err == nil {
			for _, e := range entries {
				a, ok := r.load(e.Name(), worktrees)
				if ok {
					out = append(out, a)
				}
			}
		}
	}
	// Most recently touched first, then by what wants attention. Callers group by
	// worktree, so a stable global order keeps the lists deterministic.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Updated > out[j].Updated })
	sort.SliceStable(out, func(i, j int) bool {
		return rankOf(out[i].Tempo) < rankOf(out[j].Tempo)
	})

	r.mu.Lock()
	r.cached, r.cachedAt = out, time.Now()
	r.mu.Unlock()
	return out
}

func rankOf(tempo string) int {
	if n, ok := tempoRank[tempo]; ok {
		return n
	}
	return 3
}

func (r *Reader) load(id string, worktrees []string) (Agent, bool) {
	b, err := os.ReadFile(filepath.Join(r.JobsDir, id, "state.json"))
	if err != nil {
		return Agent{}, false
	}
	var s state
	if err := json.Unmarshal(b, &s); err != nil {
		return Agent{}, false
	}
	// An agent belongs to a worktree if *either* directory field points into it:
	// one is set when it was spawned there, the other when it moved there itself.
	// worktreePath wins, being the more specific claim.
	home := LongestOwner(s.WorktreePath, worktrees)
	if home == "" {
		home = LongestOwner(s.CWD, worktrees)
	}
	if home == "" {
		return Agent{}, false // not our repo's business
	}

	a := Agent{
		ID: id, Name: strings.TrimSpace(s.Name), Tempo: s.Tempo,
		Detail: strings.TrimSpace(s.Detail), Updated: s.UpdatedAt,
		Home: home, Tokens: s.Tokens, PRs: []int{},
	}
	// Every PR this agent has produced, not just the newest: one task commonly
	// yields several (a fix, its follow-up, the revert), and showing one made the
	// rest invisible.
	for _, ch := range s.Children {
		if ch.Kind == "pr" || strings.Contains(ch.Href, "/pull/") {
			if m := prHref.FindStringSubmatch(ch.Href); m != nil {
				a.PRs = appendUnique(a.PRs, m[1])
			}
		}
	}
	for _, m := range prText.FindAllStringSubmatch(a.Detail, -1) {
		a.PRs = appendUnique(a.PRs, m[1])
	}
	return a, true
}

func appendUnique(list []int, num string) []int {
	n, err := strconv.Atoi(num)
	if err != nil {
		return list
	}
	for _, have := range list {
		if have == n {
			return list
		}
	}
	return append(list, n)
}

// LongestOwner returns the worktree that owns dir: the longest path containing it.
//
// Longest, not first: worktrees typically live *inside* the primary checkout, so
// "is dir under this worktree" is true of the primary for every worktree — which
// is exactly how the primary ended up permanently marked as busy.
func LongestOwner(dir string, worktrees []string) string {
	if dir == "" {
		return ""
	}
	best := ""
	for _, w := range worktrees {
		if w == "" {
			continue
		}
		if dir == w || strings.HasPrefix(dir, w+string(filepath.Separator)) {
			if len(w) > len(best) {
				best = w
			}
		}
	}
	return best
}

// LiveDirs is the set of worktrees with an agent process actually running in
// them. Cached for a few seconds: it costs a pgrep plus an lsof.
func (r *Reader) LiveDirs(worktrees []string) map[string]bool {
	r.liveMu.Lock()
	if time.Since(r.liveAt) < 4*time.Second && r.live != nil {
		defer r.liveMu.Unlock()
		return r.live
	}
	r.liveMu.Unlock()

	owners := map[string]bool{}
	if r.Process != "" {
		for _, dir := range processDirs(r.Process) {
			if owner := LongestOwner(dir, worktrees); owner != "" {
				owners[owner] = true
			}
		}
	}
	r.liveMu.Lock()
	r.live, r.liveAt = owners, time.Now()
	r.liveMu.Unlock()
	return owners
}

// processDirs lists the working directories of every process matching name.
func processDirs(name string) []string {
	res := run.Cmd("", 6*time.Second, "pgrep", "-f", name)
	var pids []string
	for _, f := range strings.Fields(res.Out) {
		if _, err := strconv.Atoi(f); err == nil {
			pids = append(pids, f)
			if len(pids) >= 80 { // a runaway match shouldn't turn into a huge lsof
				break
			}
		}
	}
	if len(pids) == 0 {
		return nil
	}
	// -d cwd -Fn: just the working directory, one "n<path>" line per process.
	res = run.Cmd("", 10*time.Second, "lsof", "-a", "-d", "cwd", "-Fn", "-p", strings.Join(pids, ","))
	var dirs []string
	for _, ln := range res.Lines() {
		if strings.HasPrefix(ln, "n/") {
			dirs = append(dirs, ln[1:])
		}
	}
	return dirs
}

// Invalidate drops the cache, for right after something is known to have changed.
func (r *Reader) Invalidate() {
	r.mu.Lock()
	r.cached, r.cachedAt = nil, time.Time{}
	r.mu.Unlock()
}
