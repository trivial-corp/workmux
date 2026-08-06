package project

import (
	"path/filepath"
	"testing"

	"github.com/trivial-corp/workmux/internal/testrepo"
)

// Two checkouts of the same-named repository is the ordinary way to collide — you
// have the repo and a copy of it — and one must not shadow the other.
func TestIDsAreUnique(t *testing.T) {
	a := testrepo.New(t, "same")
	b := testrepo.New(t, "same")

	set, err := New([]string{a.Root, b.Root})
	if err != nil {
		t.Fatal(err)
	}
	if set.Len() != 2 {
		t.Fatalf("set holds %d projects", set.Len())
	}
	ids := []string{set.List()[0].ID, set.List()[1].ID}
	if ids[0] != "same" || ids[1] != "same-2" {
		t.Errorf("ids = %v, want same and same-2", ids)
	}
	for _, id := range ids {
		if _, ok := set.Get(id); !ok {
			t.Errorf("%q is not resolvable", id)
		}
	}
	// The suffix has to reach the builder too, or the merged list can't tell the
	// two apart — which is the only reason the suffix exists.
	if set.List()[1].Builder.ID != "same-2" {
		t.Errorf("builder id = %q", set.List()[1].Builder.ID)
	}
}

// The same root twice is one project. It happens whenever you run workmux where
// you already ran it.
func TestTheSameRootIsOneProject(t *testing.T) {
	r := testrepo.New(t, "once")
	set, err := New([]string{r.Root, r.Root})
	if err != nil {
		t.Fatal(err)
	}
	if set.Len() != 1 {
		t.Errorf("set holds %d projects", set.Len())
	}
	if p, err := set.Add(r.Root); err != nil || p.ID != "once" || set.Len() != 1 {
		t.Errorf("adding it again = %v %v, %d projects", p, err, set.Len())
	}
}

func TestAddAndRemove(t *testing.T) {
	a := testrepo.New(t, "alpha")
	b := testrepo.New(t, "beta")
	set, err := New([]string{a.Root})
	if err != nil {
		t.Fatal(err)
	}

	// A project added while the server runs must come out fully wired, which is
	// what OnAdd is for — it's the only difference between arriving at startup and
	// arriving over HTTP an hour later.
	wired := ""
	set.OnAdd = func(p *Project) { wired = p.ID }
	if _, err := set.Add(b.Root); err != nil {
		t.Fatal(err)
	}
	if wired != "beta" {
		t.Errorf("OnAdd saw %q", wired)
	}

	if _, err := set.Remove("beta"); err != nil {
		t.Fatal(err)
	}
	if _, ok := set.Get("beta"); ok {
		t.Error("beta is still resolvable")
	}
	if set.Len() != 1 {
		t.Errorf("set holds %d projects", set.Len())
	}
	if _, err := set.Remove("alpha"); err == nil {
		t.Error("the last project should not be removable")
	}
	if _, err := set.Remove("nope"); err == nil {
		t.Error("removing an unknown project should say so")
	}
}

func TestNotARepo(t *testing.T) {
	if _, err := New([]string{t.TempDir()}); err == nil {
		t.Error("a bare directory is not a project")
	}
	if _, err := New(nil); err == nil {
		t.Error("no roots at all should be refused")
	}
}

// A project's worktrees are its own. This is the check every path from a request
// passes through, so it is the one that must not be loose.
func TestOwnsIsPerProject(t *testing.T) {
	a := testrepo.New(t, "alpha")
	b := testrepo.New(t, "beta")
	tree := a.Worktree("a-change")

	set, err := New([]string{a.Root, b.Root})
	if err != nil {
		t.Fatal(err)
	}
	alpha, _ := set.Get("alpha")
	beta, _ := set.Get("beta")

	if !alpha.Owns(tree) {
		t.Error("alpha should own its own worktree")
	}
	if beta.Owns(tree) {
		t.Error("beta must not own alpha's worktree")
	}
	if alpha.Owns(b.Root) {
		t.Error("alpha must not own beta's checkout")
	}
	if alpha.Owns(filepath.Join(a.Root, "nope")) {
		t.Error("a path that isn't a worktree is not owned")
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"trip1":   "trip1",
		"My Repo": "my-repo",
		"a/b":     "a-b",
		"...":     "project",
		"":        "project",
		// Anything that can't go in a path becomes a separator rather than being
		// dropped, so two names that differ only there don't collapse into one id.
		"Ünïcode-Repo": "n-code-repo",
	} {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
}
