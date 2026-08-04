package changes

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trivial-corp/workmux/internal/testrepo"
)

func TestStatusAndCounts(t *testing.T) {
	r := testrepo.New(t, "proj")
	r.Write("tracked.txt", "one\ntwo\nthree\n")
	r.Git("add", "-A")
	r.Git("commit", "-qm", "add tracked")

	r.Write("tracked.txt", "one\ntwo\nthree\nfour\n") // modified, unstaged
	r.Write("staged.txt", "new\n")
	r.Git("add", "staged.txt") // staged
	r.Write("untracked.txt", "loose\n")

	v := Read(r.Root, "main")
	byPath := map[string]File{}
	for _, f := range v.Files {
		byPath[f.Path] = f
	}
	if len(v.Files) != 3 {
		t.Fatalf("files = %+v", v.Files)
	}
	if f := byPath["tracked.txt"]; f.Staged || f.Untracked || f.Add != 1 {
		t.Errorf("tracked.txt = %+v, want unstaged with 1 addition", f)
	}
	if f := byPath["staged.txt"]; !f.Staged {
		t.Errorf("staged.txt = %+v, want staged", f)
	}
	if f := byPath["untracked.txt"]; !f.Untracked {
		t.Errorf("untracked.txt = %+v, want untracked", f)
	}
	// Staged first: what you're about to commit is what you're looking for.
	if !v.Files[0].Staged {
		t.Errorf("order = %v, want staged first", names(v.Files))
	}
	if v.Branch != "main" {
		t.Errorf("branch = %q", v.Branch)
	}
}

// The bug this guards: falling back to --no-index whenever the diff was empty
// reported every *unmodified* file as brand new.
func TestUnmodifiedFileHasNoDiff(t *testing.T) {
	r := testrepo.New(t, "proj")
	r.Write("quiet.txt", "unchanged\n")
	r.Git("add", "-A")
	r.Git("commit", "-qm", "add")

	if got := FileDiff(r.Root, "quiet.txt", false); got != "" {
		t.Errorf("an unmodified file produced a diff:\n%s", got)
	}
}

func TestDiffSides(t *testing.T) {
	r := testrepo.New(t, "proj")
	r.Write("f.txt", "before\n")
	r.Git("add", "-A")
	r.Git("commit", "-qm", "add")

	r.Write("f.txt", "after\n")
	unstaged := FileDiff(r.Root, "f.txt", false)
	if !strings.Contains(unstaged, "-before") || !strings.Contains(unstaged, "+after") {
		t.Errorf("worktree diff:\n%s", unstaged)
	}

	r.Git("add", "f.txt")
	// Now the change is staged: asking for the worktree side must still find it
	// rather than reporting nothing, because the file *has* changed.
	if got := FileDiff(r.Root, "f.txt", false); !strings.Contains(got, "+after") {
		t.Errorf("staged change invisible from the worktree side:\n%s", got)
	}
	if got := FileDiff(r.Root, "f.txt", true); !strings.Contains(got, "+after") {
		t.Errorf("staged side:\n%s", got)
	}
}

// An untracked file has no counterpart to diff against, so its contents are the diff.
func TestUntrackedFileShowsItsContents(t *testing.T) {
	r := testrepo.New(t, "proj")
	r.Write("brand-new.txt", "hello from a new file\n")

	got := FileDiff(r.Root, "brand-new.txt", false)
	if !strings.Contains(got, "hello from a new file") {
		t.Errorf("diff:\n%s", got)
	}
}

func TestCommitsAgainstTheBaseNotUpstream(t *testing.T) {
	r := testrepo.New(t, "proj")
	r.FakeOrigin()
	wt := r.Worktree("feature")
	r.Commit(wt, "a.txt", "1\n", "first thing")
	r.Commit(wt, "b.txt", "2\n", "second thing")

	v := Read(wt, "main")
	if len(v.Commits) != 2 {
		t.Fatalf("commits = %+v", v.Commits)
	}
	if v.Commits[0].Msg != "second thing" {
		t.Errorf("newest first expected, got %+v", v.Commits)
	}
	// Nothing is pushed yet, and with no upstream that's unknowable rather than false.
	for _, c := range v.Commits {
		if c.Pushed {
			t.Errorf("%s claims to be pushed", c.SHA)
		}
	}

	// Push, and they must *still* be listed — measuring against @{upstream} showed
	// nothing the moment you pushed, which is when a branch has the most to show.
	r.GitIn(wt, "push", "-q", "-u", "origin", "feature")
	v = Read(wt, "main")
	if len(v.Commits) != 2 {
		t.Fatalf("after pushing, commits = %+v", v.Commits)
	}
	for _, c := range v.Commits {
		if !c.Pushed {
			t.Errorf("%s should be marked pushed", c.SHA)
		}
	}
}

func TestCommitDiff(t *testing.T) {
	r := testrepo.New(t, "proj")
	r.Write("thing.txt", "content here\n")
	r.Git("add", "-A")
	r.Git("commit", "-qm", "a commit worth reading")
	sha := strings.TrimSpace(r.Git("rev-parse", "--short", "HEAD"))

	got := CommitDiff(r.Root, sha)
	if !strings.Contains(got, "a commit worth reading") || !strings.Contains(got, "+content here") {
		t.Errorf("diff:\n%s", got)
	}
	if CommitDiff(r.Root, "not-a-sha") != "" {
		t.Error("a non-sha revision must be refused")
	}
	// Options and ranges are not revisions.
	for _, bad := range []string{"--help", "HEAD~1..HEAD", "main", "-n1", ""} {
		if CommitDiff(r.Root, bad) != "" {
			t.Errorf("%q was accepted as a revision", bad)
		}
	}
}

// A diff request must not be able to read outside the worktree.
func TestFileDiffStaysInsideTheWorktree(t *testing.T) {
	r := testrepo.New(t, "proj")
	secret := filepath.Join(filepath.Dir(r.Root), "outside.txt")
	if err := os.WriteFile(secret, []byte("not yours\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"../outside.txt", "/etc/hosts", "", "--output=/tmp/x"} {
		if got := FileDiff(r.Root, rel, false); got != "" {
			t.Errorf("FileDiff(%q) returned something:\n%s", rel, got)
		}
	}
}

func TestRenamesShowTheDestination(t *testing.T) {
	r := testrepo.New(t, "proj")
	r.Write("old-name.txt", "same content\n")
	r.Git("add", "-A")
	r.Git("commit", "-qm", "add")
	r.Git("mv", "old-name.txt", "new-name.txt")

	v := Read(r.Root, "main")
	if len(v.Files) != 1 {
		t.Fatalf("files = %+v", v.Files)
	}
	if v.Files[0].Path != "new-name.txt" {
		t.Errorf("path = %q, want the destination of the rename", v.Files[0].Path)
	}
}

func TestCleanWorktree(t *testing.T) {
	r := testrepo.New(t, "proj")
	v := Read(r.Root, "main")
	if len(v.Files) != 0 {
		t.Errorf("files = %+v, want none", v.Files)
	}
	// Empty lists, not nulls: the UI iterates without checking.
	if v.Files == nil || v.Commits == nil {
		t.Error("empty must serialise as [], not null")
	}
}

func names(files []File) []string {
	var out []string
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out
}
