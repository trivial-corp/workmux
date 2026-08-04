// Package actions is everything that changes something: new work, a stack up or
// down, merging a base branch in, checking a PR out.
//
// Two rules run through all of it. Errors are user-facing sentences, because they
// are shown next to the button that caused them. And nothing is delegated to a
// project script that might not exist — git work is git, so it happens here, while
// container work goes through whatever the project's config says.
package actions

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/trivial-corp/workmux/internal/config"
	"github.com/trivial-corp/workmux/internal/gitx"
	"github.com/trivial-corp/workmux/internal/run"
)

// Runner performs actions for one repository.
type Runner struct {
	Cfg *config.Config
	// Spawn starts an agent in a directory. Nil, or a project with no spawn command,
	// means new work is created without one rather than failing.
	Spawn func(cwd, prompt string) (string, error)
	// Invalidate drops cached reads after something changes, so the next poll sees it.
	Invalidate func()
}

// stopwords keep a derived branch name about the task rather than about English.
var stopwords = map[string]bool{
	"the": true, "a": true, "an": true, "and": true, "or": true, "but": true, "for": true,
	"to": true, "of": true, "in": true, "on": true, "at": true, "by": true, "with": true,
	"we": true, "i": true, "it": true, "is": true, "are": true, "be": true, "should": true,
	"need": true, "needs": true, "make": true, "let": true, "lets": true, "please": true,
	"add": true, "fix": true, "this": true, "that": true, "so": true, "can": true,
}

var (
	wordRe  = regexp.MustCompile(`[a-z0-9]+`)
	nameOK  = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]{0,60}$`)
	prNumRe = regexp.MustCompile(`(\d+)`)
)

// NewWork is a worktree, a branch, the gitignored files it needs, and — if the
// project has an agent — an agent working on the task.
//
// Deliberately never touches containers: this is the "small change, no app needed"
// path, and waiting a minute for six containers you won't look at is the thing that
// stops people branching at all.
type NewWorkResult struct {
	Path          string `json:"path"`
	Dir           string `json:"dir"`
	Branch        string `json:"branch"`
	Base          string `json:"base"`
	Copied        int    `json:"copied"`
	AgentStarting bool   `json:"agent_starting"`
}

func (r *Runner) NewWork(name, prompt, base string) (*NewWorkResult, error) {
	root := r.Cfg.Root
	// A name is optional: derive one from the task, because nobody should have to
	// invent a branch name to start working.
	slug := slugify(name)
	if slug == "" {
		slug = slugify(nameFromTask(prompt))
	}
	if slug == "" || !nameOK.MatchString(slug) {
		return nil, errors.New("describe the task, or give it a short name")
	}
	slug = uniqueSlug(root, r.Cfg.Worktrees.Path, slug)
	if slug == "" {
		return nil, errors.New("couldn't find a free branch name — try different wording")
	}
	if base == "" {
		base = gitx.DefaultBranch(root)
	}

	// Branch from origin/<base> so new work starts level with the remote rather than
	// on top of whatever the primary checkout happens to be holding.
	run.Git(root, 40*time.Second, "fetch", "--quiet", "origin", base)
	start := "origin/" + base
	if !run.Git(root, 8*time.Second, "rev-parse", "--verify", "--quiet", start).OK() {
		start = base
	}
	path := filepath.Join(root, r.Cfg.Worktrees.Path, slug)
	res := run.Git(root, 90*time.Second, "worktree", "add", "-b", slug, path, start)
	if !res.OK() {
		return nil, errors.New(res.LastLine("git worktree add failed"))
	}

	copied := CopyLocalFiles(r.Cfg, path)

	out := &NewWorkResult{Path: path, Dir: slug, Branch: slug, Base: base, Copied: copied}
	// Start the agent behind the response. Waiting on it held the request for as
	// long as the agent took to boot, and if it ever hangs the button just sits
	// there; it turns up in the next poll either way.
	if strings.TrimSpace(prompt) != "" && r.Spawn != nil && r.Cfg.Agent.Spawn != "" {
		out.AgentStarting = true
		go func() {
			_, _ = r.Spawn(path, prompt)
			r.invalidate()
		}()
	}
	r.invalidate()
	return out, nil
}

// MergeBase brings a worktree level with its base branch.
//
// Implemented here rather than shelled out to a project script, so it works in any
// repository. A conflict is aborted rather than left in the tree: this runs from a
// button, and a half-merged worktree you didn't ask for is worse than a refusal.
func (r *Runner) MergeBase(path, base string) (string, error) {
	if base == "" {
		base = gitx.DefaultBranch(r.Cfg.Root)
	}
	if res := run.Git(path, 60*time.Second, "fetch", "--quiet", "origin", base); !res.OK() {
		return "", errors.New(res.LastLine("could not fetch " + base))
	}
	if dirty := run.Git(path, 20*time.Second, "status", "--porcelain"); strings.TrimSpace(dirty.Out) != "" {
		return "", errors.New("this worktree has uncommitted changes — commit or stash them first")
	}
	res := run.Git(path, 90*time.Second, "merge", "--no-edit", "origin/"+base)
	if !res.OK() {
		run.Git(path, 30*time.Second, "merge", "--abort")
		return "", fmt.Errorf("merge conflicts with origin/%s — aborted, resolve it in a session", base)
	}
	r.invalidate()
	return strings.TrimSpace(res.Out), nil
}

// CheckoutPR puts a pull request in its own worktree, so reviewing one doesn't
// disturb what you were doing.
func (r *Runner) CheckoutPR(ref string) (*NewWorkResult, error) {
	m := prNumRe.FindStringSubmatch(ref)
	if m == nil {
		return nil, errors.New("which PR?")
	}
	num := m[1]
	root := r.Cfg.Root
	branch := "pr-" + num
	path := filepath.Join(root, r.Cfg.Worktrees.Path, branch)
	if _, err := os.Stat(path); err == nil {
		return &NewWorkResult{Path: path, Dir: branch, Branch: branch}, nil
	}
	// The PR's head, fetched by number: works for forks, and doesn't need the branch
	// to exist locally.
	if res := run.Git(root, 60*time.Second, "fetch", "--quiet", "origin",
		"pull/"+num+"/head:"+branch); !res.OK() {
		return nil, errors.New(res.LastLine("could not fetch PR #" + num))
	}
	if res := run.Git(root, 90*time.Second, "worktree", "add", path, branch); !res.OK() {
		return nil, errors.New(res.LastLine("could not create a worktree for PR #" + num))
	}
	copied := CopyLocalFiles(r.Cfg, path)
	r.invalidate()
	return &NewWorkResult{Path: path, Dir: branch, Branch: branch, Copied: copied}, nil
}

// Stack runs a configured container action for one slot. The command is whatever
// the project says it is; an unconfigured action is refused rather than improvised.
func (r *Runner) Stack(action, slot, path string) (string, error) {
	if !r.Cfg.HasStack() {
		return "", errors.New("this project has no stack")
	}
	switch action {
	case "up", "restart", "stop":
	default:
		return "", errors.New("unknown stack action")
	}
	if slot != "" && !r.Cfg.IsSlot(slot) {
		return "", errors.New("not one of this project's slots")
	}
	cmd := r.Cfg.StackCmd(action, slot, path, "")
	if cmd == "" {
		return "", fmt.Errorf("no %s command configured", action)
	}
	dir := path
	if dir == "" {
		dir = r.Cfg.Root
	}
	// Through a login shell, because the configured command is a shell command and
	// often a project script that expects a real PATH.
	res := run.Env(dir, append(os.Environ(),
		"STACK="+slot, "COMPOSE_PROFILES="+r.Cfg.Profiles()),
		20*time.Minute, loginShell(), "-lc", cmd)
	if !res.OK() {
		return "", errors.New(res.LastLine(action + " failed"))
	}
	r.invalidate()
	return res.Out, nil
}

// CopyLocalFiles brings worktrees.copy over from the primary checkout.
//
// A worktree is a clean checkout, so everything gitignored is missing: .env,
// service-account keys, credential masters. The app then fails minutes later with
// something that looks nothing like "you forgot a file".
//
// Patterns read like .gitignore: no slash matches that basename at any depth, a
// slash anchors to the repo root. Noisy directories are pruned rather than filtered,
// including the worktree root itself — walking 40 worktrees' node_modules to find
// .env files took five seconds.
func CopyLocalFiles(cfg *config.Config, dest string) int {
	patterns := cfg.Worktrees.Copy
	if len(patterns) == 0 {
		return 0
	}
	prune := map[string]bool{
		".git": true, ".hg": true, "node_modules": true, "vendor": true, "dist": true,
		"build": true, "target": true, "__pycache__": true, ".venv": true, ".next": true,
		".turbo": true, ".cache": true, ".terraform": true,
	}
	wtRoot := filepath.Join(cfg.Root, cfg.Worktrees.Path)
	var loose, anchored []string
	for _, p := range patterns {
		if filepath.IsAbs(p) || strings.Contains(p, "..") {
			continue // must be a path inside the repo
		}
		if strings.Contains(p, "/") {
			anchored = append(anchored, p)
		} else {
			loose = append(loose, p)
		}
	}
	copied := 0
	_ = filepath.Walk(cfg.Root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if path != cfg.Root && (prune[info.Name()] || path == wtRoot) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(cfg.Root, path)
		if err != nil {
			return nil
		}
		if !matchAny(info.Name(), loose) && !matchAny(rel, anchored) {
			return nil
		}
		target := filepath.Join(dest, rel)
		if _, err := os.Stat(target); err == nil {
			return nil // never overwrite what's already there
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if os.MkdirAll(filepath.Dir(target), 0o755) != nil {
			return nil
		}
		mode := info.Mode().Perm()
		if os.WriteFile(target, body, mode) == nil {
			copied++
		}
		return nil
	})
	return copied
}

func matchAny(name string, patterns []string) bool {
	for _, p := range patterns {
		if ok, _ := filepath.Match(p, name); ok {
			return true
		}
	}
	return false
}

// slugify turns free text into something git will accept as a branch.
func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	lastDash := true
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '/':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-./")
	if len(out) > 60 {
		out = strings.Trim(out[:60], "-./")
	}
	return out
}

// nameFromTask derives a branch name from what the task says.
func nameFromTask(prompt string) string {
	var words []string
	for _, w := range wordRe.FindAllString(strings.ToLower(prompt), -1) {
		if len(w) > 1 && !stopwords[w] {
			words = append(words, w)
		}
		if len(words) == 4 {
			break
		}
	}
	name := strings.Join(words, "-")
	if len(name) > 40 {
		name = name[:40]
	}
	return name
}

// uniqueSlug finds a name no branch or directory is using yet.
func uniqueSlug(root, worktrees, slug string) string {
	for i := 0; i < 40; i++ {
		candidate := slug
		if i > 0 {
			candidate = slug + "-" + strconv.Itoa(i+1)
		}
		if _, err := os.Stat(filepath.Join(root, worktrees, candidate)); err == nil {
			continue
		}
		if run.Git(root, 8*time.Second, "rev-parse", "--verify", "--quiet",
			"refs/heads/"+candidate).OK() {
			continue
		}
		return candidate
	}
	return ""
}

func (r *Runner) invalidate() {
	if r.Invalidate != nil {
		r.Invalidate()
	}
}

func loginShell() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		if _, err := os.Stat(sh); err == nil {
			return sh
		}
	}
	return "/bin/sh"
}
