// Package gitx reads the repository. Nothing here writes, so it's safe to call
// on every poll.
package gitx

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/trivial-corp/workmux/internal/bg"
	"github.com/trivial-corp/workmux/internal/run"
)

// Worktree is one checkout: where it is, what it's on, and how far it has drifted.
type Worktree struct {
	Path     string `json:"path"`
	Dir      string `json:"dir"`
	Branch   string `json:"branch"`
	Detached bool   `json:"detached"`
}

// PrimaryRoot is the main checkout for the repo containing start.
//
// It matters that this is the *primary* one and not `--show-toplevel`: worktrees
// live inside it, so running from one of them must still resolve config, the
// worktree list and the base branch against the repository as a whole.
func PrimaryRoot(start string) string {
	res := run.Git(start, 8*time.Second, "worktree", "list", "--porcelain")
	for _, ln := range res.Lines() {
		if strings.HasPrefix(ln, "worktree ") {
			return strings.TrimSpace(strings.TrimPrefix(ln, "worktree "))
		}
	}
	return start
}

// IsRepo reports whether root is a git checkout. A worktree's .git is a file,
// the primary's is a directory, so this only asks whether it exists.
func IsRepo(root string) bool {
	_, err := os.Stat(filepath.Join(root, ".git"))
	return err == nil
}

// Worktrees lists every checkout in git's own order, primary first.
func Worktrees(root string) []Worktree {
	res := run.Git(root, 8*time.Second, "worktree", "list", "--porcelain")
	var out []Worktree
	var cur *Worktree
	flush := func() {
		if cur != nil {
			out = append(out, *cur)
			cur = nil
		}
	}
	for _, ln := range res.Lines() {
		switch {
		case strings.HasPrefix(ln, "worktree "):
			flush()
			p := strings.TrimSpace(strings.TrimPrefix(ln, "worktree "))
			cur = &Worktree{Path: p, Dir: filepath.Base(p)}
		case cur == nil:
			continue
		case strings.HasPrefix(ln, "branch "):
			ref := strings.TrimSpace(strings.TrimPrefix(ln, "branch "))
			cur.Branch = strings.TrimPrefix(ref, "refs/heads/")
		case ln == "detached":
			cur.Detached = true
		}
	}
	flush()
	return out
}

// Names reads every branch's description in one call — workmux stores the
// human-given name of a piece of work there, so it lives with the repository
// and git's own tooling (`git branch --edit-description`) sees the same thing.
func Names(root string) map[string]string {
	res := run.Git(root, 8*time.Second, "config", "--get-regexp", `^branch\..+\.description$`)
	out := map[string]string{}
	for _, ln := range res.Lines() {
		// A multi-line description continues on lines without the key; only the
		// first line is a name.
		key, val, ok := strings.Cut(ln, " ")
		if !ok || !strings.HasPrefix(key, "branch.") || !strings.HasSuffix(key, ".description") {
			continue
		}
		out[strings.TrimSuffix(strings.TrimPrefix(key, "branch."), ".description")] = val
	}
	return out
}

var (
	baseOnce  sync.Mutex
	baseCache = map[string]string{}
)

// DefaultBranch is what "up to date" is measured against: origin's HEAD, falling
// back to a local main or master. Cached — it does not change while running, and
// asking the remote is a network round.
func DefaultBranch(root string) string {
	baseOnce.Lock()
	defer baseOnce.Unlock()
	if b, ok := baseCache[root]; ok {
		return b
	}
	b := ""
	if res := run.Git(root, 8*time.Second, "symbolic-ref", "--quiet", "--short",
		"refs/remotes/origin/HEAD"); res.OK() {
		b = strings.TrimPrefix(strings.TrimSpace(res.Out), "origin/")
	}
	if b == "" {
		for _, cand := range []string{"main", "master"} {
			if run.Git(root, 6*time.Second, "rev-parse", "--verify", "--quiet",
				"refs/heads/"+cand).OK() {
				b = cand
				break
			}
		}
	}
	if b == "" {
		b = "main"
	}
	baseCache[root] = b
	return b
}

// BehindAhead counts commits between origin/<base> and HEAD, from local refs
// only. (-1, -1) means unknown — a detached head, or a base that isn't fetched.
func BehindAhead(path, base string) (int, int) {
	res := run.Git(path, 8*time.Second, "rev-list", "--left-right", "--count",
		"origin/"+base+"...HEAD")
	parts := strings.Fields(res.Out)
	if !res.OK() || len(parts) != 2 {
		return -1, -1
	}
	behind, err1 := strconv.Atoi(parts[0])
	ahead, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return -1, -1
	}
	return behind, ahead
}

var (
	fetchMu   sync.Mutex
	fetchedAt = map[string]time.Time{}
)

// KickFetch refreshes origin/<base> in the background, at most once a minute.
//
// Behind-counts come from local refs, so without this they quietly go stale and
// the merge button lies. Doing it inline would make every poll wait on the
// network instead.
func KickFetch(root, base string) {
	fetchMu.Lock()
	if time.Since(fetchedAt[root]) < time.Minute {
		fetchMu.Unlock()
		return
	}
	fetchedAt[root] = time.Now()
	fetchMu.Unlock()
	// Through bg, so this fetch can be waited on: a detached goroutine writing into
	// .git outlived the test that started it and raced the removal of its own repo.
	bg.Go(func() { run.Git(root, 45*time.Second, "fetch", "--quiet", "origin", base) })
}

var (
	remoteMu    sync.Mutex
	remoteCache = map[string]string{}
)

// WebURL is the https URL of the origin remote, for linking PRs and commits.
// Empty when there is no origin — in which case nothing should render a link.
// Cached per root: it doesn't change while running, and it's a subprocess.
func WebURL(root string) string {
	remoteMu.Lock()
	defer remoteMu.Unlock()
	if u, ok := remoteCache[root]; ok {
		return u
	}
	remoteCache[root] = ""
	res := run.Git(root, 8*time.Second, "remote", "get-url", "origin")
	if !res.OK() {
		return ""
	}
	u := strings.TrimSpace(res.Out)
	u = strings.TrimSuffix(u, ".git")
	switch {
	case strings.HasPrefix(u, "git@"):
		// git@github.com:owner/repo → https://github.com/owner/repo
		if rest := strings.TrimPrefix(u, "git@"); strings.Contains(rest, ":") {
			parts := strings.SplitN(rest, ":", 2)
			u = "https://" + parts[0] + "/" + parts[1]
		}
	case strings.HasPrefix(u, "ssh://git@"):
		u = "https://" + strings.TrimPrefix(u, "ssh://git@")
	case !strings.HasPrefix(u, "http"):
		u = ""
	}
	remoteCache[root] = u
	return u
}
