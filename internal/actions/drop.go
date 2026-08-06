package actions

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/trivial-corp/workmux/internal/gitx"
	"github.com/trivial-corp/workmux/internal/run"
)

// DropReport is what dropping a worktree would destroy, or did.
//
// The same shape answers both questions, so the confirmation you are shown is built
// from the same read that the drop itself checks against — rather than a sentence
// written once and left to drift from what the code does.
type DropReport struct {
	Path     string `json:"path"`
	Branch   string `json:"branch"`
	Dirty    int    `json:"dirty"`    // files changed and not committed
	Ahead    int    `json:"ahead"`    // commits this branch has that its base doesn't, -1 unknown
	Untitled bool   `json:"untitled"` // detached, so there's no branch to speak of
	Merged   bool   `json:"merged"`   // the branch is known to be contained in its base
	Removed  bool   `json:"removed"`  // the worktree is gone
	Deleted  bool   `json:"deleted"`  // ...and so is the branch
}

// DropCheck reports what would be lost, without touching anything.
func (r *Runner) DropCheck(path string) (*DropReport, error) {
	wt, err := r.findWorktree(path)
	if err != nil {
		return nil, err
	}
	rep := &DropReport{Path: wt.Path, Branch: wt.Branch, Untitled: wt.Detached}
	res := run.Git(wt.Path, 10*time.Second, "status", "--porcelain")
	for _, ln := range res.Lines() {
		if strings.TrimSpace(ln) != "" {
			rep.Dirty++
		}
	}
	if wt.Branch != "" {
		base := gitx.DefaultBranch(r.Cfg.Root)
		// Whether the branch is already contained in its base, asked of local refs only.
		// The ahead-count is measured against origin/<base> and is simply unknown without
		// a remote or a fetch — which would make "is this safe to delete" unanswerable in
		// a repo that has no origin at all. This is answerable everywhere.
		rep.Merged = run.Git(wt.Path, 10*time.Second, "merge-base", "--is-ancestor", "HEAD", base).OK()
		_, ahead := gitx.BehindAhead(wt.Path, base)
		rep.Ahead = ahead
	}
	return rep, nil
}

// Drop removes a worktree, and its branch when asked.
//
// It refuses by default rather than asking git to force anything: uncommitted changes
// and commits that exist nowhere else are the two things here that no other copy of
// this repository has, and a dashboard that can lose them with one click is a worse
// tool than one that cannot delete at all. `force` is the caller having read what it
// would cost and said yes anyway.
func (r *Runner) Drop(path string, deleteBranch, force bool) (*DropReport, error) {
	rep, err := r.DropCheck(path)
	if err != nil {
		return nil, err
	}
	if !force {
		if rep.Dirty > 0 {
			return rep, fmt.Errorf("%s has %d uncommitted change(s) — commit them, or drop it anyway",
				filepath.Base(rep.Path), rep.Dirty)
		}
		// Merged, not "ahead == 0": an unknown count is -1, and reading that as "nothing
		// to lose" is how this removed a worktree whose branch git then refused to
		// delete. What isn't known to be safe isn't safe.
		if deleteBranch && rep.Branch != "" && !rep.Merged {
			base := gitx.DefaultBranch(r.Cfg.Root)
			if rep.Ahead > 0 {
				return rep, fmt.Errorf("%s has %d commit(s) that %s doesn't — push or merge them, or drop it anyway",
					rep.Branch, rep.Ahead, base)
			}
			return rep, fmt.Errorf("%s isn't merged into %s — drop it anyway to delete it",
				rep.Branch, base)
		}
	}

	argv := []string{"worktree", "remove"}
	if force {
		argv = append(argv, "--force")
	}
	argv = append(argv, rep.Path)
	if res := run.Git(r.Cfg.Root, 60*time.Second, argv...); !res.OK() {
		return rep, fmt.Errorf("%s", res.LastLine("could not remove the worktree"))
	}
	rep.Removed = true

	if deleteBranch && rep.Branch != "" {
		// -D, not -d: the check above already decided whether unmerged commits are
		// acceptable, and -d would ask the same question again in git's words.
		flag := "-d"
		if force || !rep.Merged {
			flag = "-D"
		}
		if res := run.Git(r.Cfg.Root, 30*time.Second, "branch", flag, rep.Branch); !res.OK() {
			// The worktree is already gone, which is most of what was asked for. Say
			// what didn't happen instead of pretending the whole thing failed.
			r.invalidate()
			return rep, fmt.Errorf("worktree removed, but the branch is still there: %s",
				res.LastLine("git branch refused"))
		}
		rep.Deleted = true
	}
	r.invalidate()
	return rep, nil
}

// findWorktree resolves a path to one of this repo's worktrees, and refuses the
// primary checkout: it is where the repository itself lives, and `git worktree remove`
// won't take it either.
func (r *Runner) findWorktree(path string) (*gitx.Worktree, error) {
	want, err := filepath.EvalSymlinks(path)
	if err != nil {
		want = path
	}
	trees := gitx.Worktrees(r.Cfg.Root)
	for i, wt := range trees {
		got, err := filepath.EvalSymlinks(wt.Path)
		if err != nil {
			got = wt.Path
		}
		if got != want {
			continue
		}
		if i == 0 {
			return nil, fmt.Errorf("that's the main checkout, not a piece of work")
		}
		return &trees[i], nil
	}
	return nil, fmt.Errorf("not a worktree of this repository")
}
