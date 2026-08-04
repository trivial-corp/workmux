// Package testrepo builds throwaway git repositories for tests.
//
// Real git, not a mock: every claim this tool makes about worktrees, branches and
// commit counts comes from git's own output, so a fake would only test the fake.
package testrepo

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Repo is a temporary repository with a first commit on the default branch.
type Repo struct {
	Root string
	t    *testing.T
}

// New creates a repository named name under a temp directory.
func New(t *testing.T, name string) *Repo {
	t.Helper()
	base := t.TempDir()
	// Resolved, because git prints resolved paths: on macOS t.TempDir() hands back
	// /var/... while git says /private/var/..., and every path comparison in this
	// tool is a string prefix.
	if resolved, err := filepath.EvalSymlinks(base); err == nil {
		base = resolved
	}
	root := filepath.Join(base, name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	r := &Repo{Root: root, t: t}
	r.Git("init", "-q", "-b", "main")
	// Identity and signing are set locally: a developer's global config must not
	// decide whether the suite passes.
	r.Git("config", "user.email", "test@workmux.dev")
	r.Git("config", "user.name", "workmux tests")
	r.Git("config", "commit.gpgsign", "false")
	r.Write("README.md", "hello\n")
	r.Git("add", "-A")
	r.Git("commit", "-qm", "init")
	return r
}

// Write puts a file in the repo, creating parents.
func (r *Repo) Write(rel, body string) {
	r.t.Helper()
	p := filepath.Join(r.Root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// Git runs a git command in the repo and fails the test if it errors.
func (r *Repo) Git(args ...string) string {
	r.t.Helper()
	return r.git(r.Root, args...)
}

// GitIn runs a git command in a specific worktree.
func (r *Repo) GitIn(dir string, args ...string) string {
	r.t.Helper()
	return r.git(dir, args...)
}

func (r *Repo) git(dir string, args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	// A committer identity for worktrees created outside r.Root, and a locale that
	// won't translate porcelain output.
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=workmux tests", "GIT_AUTHOR_EMAIL=test@workmux.dev",
		"GIT_COMMITTER_NAME=workmux tests", "GIT_COMMITTER_EMAIL=test@workmux.dev",
		"LC_ALL=C")
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// Worktree adds a worktree on a new branch and returns its path.
func (r *Repo) Worktree(branch string) string {
	r.t.Helper()
	path := filepath.Join(r.Root, ".claude", "worktrees", branch)
	r.Git("worktree", "add", "-q", "-b", branch, path)
	return path
}

// Commit makes a commit in dir, so behind/ahead counts have something to count.
func (r *Repo) Commit(dir, file, body, msg string) {
	r.t.Helper()
	p := filepath.Join(dir, file)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		r.t.Fatal(err)
	}
	r.GitIn(dir, "add", "-A")
	r.GitIn(dir, "commit", "-qm", msg)
}

// FakeOrigin points origin at a bare clone of this repo, so behind/ahead and the
// PR-link URL have a remote to resolve against without touching the network.
func (r *Repo) FakeOrigin() string {
	r.t.Helper()
	bare := filepath.Join(filepath.Dir(r.Root), "origin.git")
	cmd := exec.Command("git", "init", "-q", "--bare", "-b", "main", bare)
	if out, err := cmd.CombinedOutput(); err != nil {
		r.t.Fatalf("init bare: %v\n%s", err, out)
	}
	r.Git("remote", "add", "origin", bare)
	r.Git("push", "-q", "-u", "origin", "main")
	return bare
}
