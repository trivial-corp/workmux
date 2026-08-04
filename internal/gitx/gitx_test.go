package gitx

import (
	"path/filepath"
	"testing"

	"github.com/trivial-corp/workmux/internal/testrepo"
)

func TestWorktreesListsPrimaryFirst(t *testing.T) {
	r := testrepo.New(t, "proj")
	feature := r.Worktree("feature-x")

	got := Worktrees(r.Root)
	if len(got) != 2 {
		t.Fatalf("got %d worktrees, want 2: %+v", len(got), got)
	}
	if got[0].Path != r.Root || got[0].Branch != "main" {
		t.Errorf("first = %+v, want the primary on main", got[0])
	}
	if got[1].Path != feature || got[1].Branch != "feature-x" {
		t.Errorf("second = %+v, want %s on feature-x", got[1], feature)
	}
	if got[1].Dir != "feature-x" {
		t.Errorf("dir = %q, want feature-x", got[1].Dir)
	}
}

// Running from inside a worktree — or a subdirectory — must still resolve to the
// repository as a whole, because that's where config and the worktree list live.
func TestPrimaryRootFromAnywhere(t *testing.T) {
	r := testrepo.New(t, "proj")
	wt := r.Worktree("feature-y")
	r.Write("src/deep/file.txt", "x\n")

	for _, from := range []string{r.Root, wt, filepath.Join(r.Root, "src", "deep")} {
		if got := PrimaryRoot(from); got != r.Root {
			t.Errorf("PrimaryRoot(%q) = %q, want %q", from, got, r.Root)
		}
	}
}

func TestIsRepo(t *testing.T) {
	r := testrepo.New(t, "proj")
	wt := r.Worktree("branchy")
	if !IsRepo(r.Root) {
		t.Error("the primary checkout is a repo")
	}
	// A worktree's .git is a file, not a directory — both must count.
	if !IsRepo(wt) {
		t.Error("a worktree is a repo")
	}
	if IsRepo(t.TempDir()) {
		t.Error("an empty directory is not a repo")
	}
}

func TestDetachedHead(t *testing.T) {
	r := testrepo.New(t, "proj")
	sha := r.Git("rev-parse", "HEAD")
	path := filepath.Join(r.Root, ".claude", "worktrees", "detached")
	r.Git("worktree", "add", "-q", "--detach", path, sha[:12])

	for _, w := range Worktrees(r.Root) {
		if w.Path == path {
			if !w.Detached || w.Branch != "" {
				t.Errorf("detached worktree = %+v, want Detached with no branch", w)
			}
			return
		}
	}
	t.Fatal("detached worktree missing from the listing")
}

func TestDefaultBranchAndBehindAhead(t *testing.T) {
	r := testrepo.New(t, "proj")
	r.FakeOrigin()

	if got := DefaultBranch(r.Root); got != "main" {
		t.Errorf("default branch = %q, want main", got)
	}

	wt := r.Worktree("ahead-by-two")
	r.Commit(wt, "a.txt", "1\n", "one")
	r.Commit(wt, "b.txt", "2\n", "two")

	behind, ahead := BehindAhead(wt, "main")
	if behind != 0 || ahead != 2 {
		t.Errorf("behind/ahead = %d/%d, want 0/2", behind, ahead)
	}

	// Move origin/main on, and the same worktree is now behind as well.
	r.Commit(r.Root, "c.txt", "3\n", "three")
	r.Git("push", "-q", "origin", "main")
	behind, ahead = BehindAhead(wt, "main")
	if behind != 1 || ahead != 2 {
		t.Errorf("behind/ahead = %d/%d, want 1/2", behind, ahead)
	}
}

// Unknown must be distinguishable from zero: "level with the base" and "we can't
// tell" render differently, and conflating them made the merge button lie.
func TestBehindAheadUnknown(t *testing.T) {
	r := testrepo.New(t, "proj")
	behind, ahead := BehindAhead(r.Root, "no-such-base")
	if behind != -1 || ahead != -1 {
		t.Errorf("behind/ahead = %d/%d, want -1/-1 for an unknown base", behind, ahead)
	}
}

func TestWebURL(t *testing.T) {
	cases := []struct{ remote, want string }{
		{"git@github.com:owner/repo.git", "https://github.com/owner/repo"},
		{"https://github.com/owner/repo.git", "https://github.com/owner/repo"},
		{"ssh://git@github.com/owner/repo.git", "https://github.com/owner/repo"},
		{"/some/local/path.git", ""}, // a local remote isn't a web URL
	}
	for _, c := range cases {
		r := testrepo.New(t, "proj")
		r.Git("remote", "add", "origin", c.remote)
		if got := WebURL(r.Root); got != c.want {
			t.Errorf("WebURL(%q) = %q, want %q", c.remote, got, c.want)
		}
	}
}

func TestWebURLWithoutOrigin(t *testing.T) {
	r := testrepo.New(t, "proj")
	if got := WebURL(r.Root); got != "" {
		t.Errorf("WebURL = %q, want empty so nothing renders a link", got)
	}
}
