package actions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trivial-corp/workmux/internal/gitx"
	"github.com/trivial-corp/workmux/internal/testrepo"
)

func TestDropRemovesAMergedWorktree(t *testing.T) {
	r := testrepo.New(t, "proj")
	wt := r.Worktree("spent-work")
	run := runner(t, r)

	rep, err := run.Drop(wt, true, false)
	if err != nil {
		t.Fatalf("a worktree with nothing in it should drop: %v", err)
	}
	if !rep.Removed || !rep.Deleted {
		t.Errorf("both the worktree and the branch were asked for: %+v", rep)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Errorf("%s is still on disk", wt)
	}
	for _, w := range gitx.Worktrees(r.Root) {
		if w.Branch == "spent-work" {
			t.Errorf("git still lists it: %+v", w)
		}
	}
}

func TestDropKeepsTheBranchWhenNotAsked(t *testing.T) {
	r := testrepo.New(t, "proj")
	wt := r.Worktree("keep-the-branch")
	run := runner(t, r)

	rep, err := run.Drop(wt, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Deleted {
		t.Error("nobody asked for the branch to go")
	}
	if !branchExists(r, "keep-the-branch") {
		t.Error("the branch should still be there")
	}
}

// The two things a worktree can hold that exist nowhere else.
func TestDropRefusesUncommittedWork(t *testing.T) {
	r := testrepo.New(t, "proj")
	wt := r.Worktree("has-edits")
	if err := os.WriteFile(filepath.Join(wt, "notes.txt"), []byte("unsaved"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := runner(t, r)

	rep, err := run.Drop(wt, false, false)
	if err == nil {
		t.Fatal("dropping a dirty worktree must be refused")
	}
	if rep == nil || rep.Dirty == 0 {
		t.Errorf("the refusal should say what is uncommitted: %+v", rep)
	}
	if !strings.Contains(err.Error(), "uncommitted") {
		t.Errorf("say why, in words: %v", err)
	}
	if _, statErr := os.Stat(wt); statErr != nil {
		t.Errorf("a refused drop must not have removed anything: %v", statErr)
	}
}

func TestDropRefusesUnpushedCommitsWithTheBranch(t *testing.T) {
	r := testrepo.New(t, "proj")
	wt := r.Worktree("has-commits")
	r.Commit(wt, "only-here.txt", "work nobody else has", "a commit only this branch has")
	run := runner(t, r)

	rep, err := run.Drop(wt, true, false)
	if err == nil {
		t.Fatal("deleting a branch with unmerged commits must be refused")
	}
	if rep == nil || rep.Ahead == 0 {
		t.Errorf("the refusal should count them, or admit it cannot: %+v", rep)
	}
	if _, statErr := os.Stat(wt); statErr != nil {
		t.Error("nothing should have been removed")
	}
	// ...but the worktree alone is fine to drop: the commits stay on the branch.
	if _, err := run.Drop(wt, false, false); err != nil {
		t.Errorf("dropping the worktree without the branch loses nothing: %v", err)
	}
	if !branchExists(r, "has-commits") {
		t.Error("the commits' branch must survive")
	}
}

func TestDropForcedTakesItAnyway(t *testing.T) {
	r := testrepo.New(t, "proj")
	wt := r.Worktree("doomed")
	r.Commit(wt, "only-here.txt", "work nobody else has", "a commit only this branch has")
	if err := os.WriteFile(filepath.Join(wt, "notes.txt"), []byte("unsaved"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := runner(t, r)

	rep, err := run.Drop(wt, true, true)
	if err != nil {
		t.Fatalf("force means force: %v", err)
	}
	if !rep.Removed || !rep.Deleted {
		t.Errorf("%+v", rep)
	}
	if branchExists(r, "doomed") {
		t.Error("the branch was asked for too")
	}
}

// The main checkout is where the repository lives. git won't remove it either.
func TestDropRefusesThePrimaryCheckout(t *testing.T) {
	r := testrepo.New(t, "proj")
	run := runner(t, r)
	if _, err := run.Drop(r.Root, false, true); err == nil {
		t.Fatal("the main checkout is not a piece of work")
	}
	if _, err := os.Stat(filepath.Join(r.Root, ".git")); err != nil {
		t.Fatalf("and it must still be there: %v", err)
	}
}

func TestDropRefusesSomewhereElse(t *testing.T) {
	r := testrepo.New(t, "proj")
	run := runner(t, r)
	other := t.TempDir()
	if _, err := run.Drop(other, false, true); err == nil {
		t.Fatal("a directory that isn't ours is not ours to delete")
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("and it must still be there: %v", err)
	}
}

// An unknown ahead-count is not a zero one. Reading -1 as "nothing to lose" removed a
// worktree whose branch git then refused to delete — a half-done destructive action.
func TestDropTreatsAnUnknownCountAsUnsafe(t *testing.T) {
	r := testrepo.New(t, "proj") // no origin: nothing to measure against
	wt := r.Worktree("unmeasurable")
	r.Commit(wt, "only-here.txt", "work nobody else has", "a commit only this branch has")
	run := runner(t, r)

	rep, err := run.DropCheck(wt)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Ahead != -1 {
		t.Fatalf("this repo has no origin, so the count cannot be known: %+v", rep)
	}
	if rep.Merged {
		t.Fatalf("a branch with its own commit is not contained in the base: %+v", rep)
	}
	if _, err := run.Drop(wt, true, false); err == nil {
		t.Fatal("an unknown count must be refused, not assumed empty")
	}
	if _, err := os.Stat(wt); err != nil {
		t.Errorf("and nothing removed on the way to refusing: %v", err)
	}
	if !branchExists(r, "unmeasurable") {
		t.Error("the branch must still be there")
	}
}

func TestDropCheckTouchesNothing(t *testing.T) {
	r := testrepo.New(t, "proj")
	wt := r.Worktree("just-looking")
	if err := os.WriteFile(filepath.Join(wt, "notes.txt"), []byte("unsaved"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := runner(t, r)

	rep, err := run.DropCheck(wt)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Dirty != 1 || rep.Branch != "just-looking" {
		t.Errorf("%+v", rep)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Error("a check is a check")
	}
}

// git marks the current branch with "*" and one checked out in another worktree with
// "+". Stripping only the first said a branch that plainly exists did not.
func branchExists(r *testrepo.Repo, name string) bool {
	for _, ln := range strings.Split(r.Git("branch", "--list", name), "\n") {
		if strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(ln), "*+ ")) == name {
			return true
		}
	}
	return false
}
